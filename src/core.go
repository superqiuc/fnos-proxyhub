package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var reTopKey = regexp.MustCompile(`(?m)^(mixed-port|port|socks-port|external-controller|secret|allow-lan|mode|log-level|ipv6|tcp-concurrent|external-ui|external-ui-url)\s*:.*$`)

type Core struct {
	mu        sync.Mutex
	geoMu     sync.Mutex
	appDir    string
	dataDir   string
	store     *Store
	logs      *LogBuffer
	cmd       *exec.Cmd
	done      chan struct{}
	coreVer   string
	startedAt time.Time
}

func NewCore(appDir, dataDir string, store *Store, logs *LogBuffer) *Core {
	return &Core{appDir: appDir, dataDir: dataDir, store: store, logs: logs}
}

func (c *Core) logf(format string, args ...any) {
	if c.logs != nil {
		c.logs.Add(fmt.Sprintf(format, args...))
	}
}

func (c *Core) corePath() string   { return filepath.Join(c.appDir, "bin", "mihomo") }
func (c *Core) configPath() string { return filepath.Join(c.dataDir, "config.yaml") }
func (c *Core) subPath() string    { return filepath.Join(c.dataDir, "subscription.yaml") }
func (c *Core) logPath() string    { return filepath.Join(c.dataDir, "core.log") }

// BuildConfig 在订阅配置基础上覆盖运行参数，保留节点与规则。
func (c *Core) BuildConfig(sub string, s Settings) string {
	clean := reTopKey.ReplaceAllString(sub, "")
	var b strings.Builder
	fmt.Fprintf(&b, "mixed-port: %d\n", s.MixedPort)
	fmt.Fprintf(&b, "allow-lan: %t\n", s.AllowLAN)
	fmt.Fprintf(&b, "ipv6: %t\n", s.IPv6)
	fmt.Fprintf(&b, "tcp-concurrent: %t\n", s.TCPConcurrent)
	fmt.Fprintf(&b, "external-controller: 127.0.0.1:%d\n", s.ControllerPort)
	fmt.Fprintf(&b, "secret: %q\n", s.Secret)
	fmt.Fprintf(&b, "mode: %s\n", s.Mode)
	fmt.Fprintf(&b, "log-level: %s\n", s.LogLevel)
	b.WriteString(applyGroupTuning(clean, s.AutoTune, s.AutoTuneInterval))
	return b.String()
}

func (c *Core) Validate(cfg string) error {
	// 先补齐 GEOIP/GEOSITE 需要的数据文件，避免 mihomo 在校验阶段去 GitHub 慢速下载。
	if err := c.ensureGeoData(cfg); err != nil {
		c.logf("地理数据准备不完整，继续尝试校验：%v", err)
	}
	err := c.validateOnce(cfg)
	if err == nil {
		return nil
	}
	// 本地地理数据可能残缺（例如上次下载被强杀），清掉后重试一次。
	if needsGeoRetry(err, cfg) {
		c.logf("地理数据校验失败，清理后重试一次：%v", err)
		c.purgeGeoData(cfg)
		if e := c.ensureGeoData(cfg); e != nil {
			c.logf("重新下载地理数据失败：%v", e)
		}
		if e := c.validateOnce(cfg); e == nil {
			return nil
		}
	}
	return err
}

func (c *Core) validateOnce(cfg string) error {
	tmp := filepath.Join(c.dataDir, "config.check.yaml")
	if err := os.WriteFile(tmp, []byte(cfg), 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, c.corePath(), "-t", "-d", c.dataDir, "-f", tmp).CombinedOutput()
	if err != nil {
		return fmt.Errorf("配置校验失败：%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// SyncConfig 用当前订阅 + 新设置重建并校验 config.yaml，失败时不覆盖旧文件。
func (c *Core) SyncConfig(s Settings) error {
	sub, err := os.ReadFile(c.subPath())
	if err != nil || len(strings.TrimSpace(string(sub))) == 0 {
		return fmt.Errorf("缺少订阅配置，请先更新订阅")
	}
	cfg := c.BuildConfig(string(sub), s)
	if err := c.Validate(cfg); err != nil {
		return err
	}
	return os.WriteFile(c.configPath(), []byte(cfg), 0o600)
}

func (c *Core) Start(s Settings) error {
	// 已存在的配置在启动前也确保地理数据齐全，避免内核运行时再走慢速下载。
	if cfg, err := os.ReadFile(c.configPath()); err == nil {
		if err := c.ensureGeoData(string(cfg)); err != nil {
			c.logf("启动前准备地理数据失败：%v", err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
	if _, err := os.Stat(c.configPath()); err != nil {
		return fmt.Errorf("缺少配置文件，请先更新订阅")
	}
	logf, err := os.OpenFile(c.logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(c.corePath(), "-d", c.dataDir, "-f", c.configPath())
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return fmt.Errorf("启动内核失败：%v", err)
	}
	done := make(chan struct{})
	c.cmd = cmd
	c.done = done
	c.startedAt = time.Now()
	go func() {
		_ = cmd.Wait()
		logf.Close()
		close(done)
	}()
	return nil
}

// stopLocked 等待 Start 中唯一的 Wait goroutine 结束，避免重复 Wait 导致
// 旧进程还没退出就启动新进程（端口被占用、重启失败）。
func (c *Core) stopLocked() {
	cmd := c.cmd
	done := c.done
	if cmd == nil || cmd.Process == nil {
		c.cmd = nil
		c.done = nil
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(time.Second):
			}
		}
	}
	c.cmd = nil
	c.done = nil
}

func (c *Core) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

func (c *Core) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil {
		return false
	}
	if c.done != nil {
		select {
		case <-c.done:
			c.cmd = nil
			c.done = nil
			return false
		default:
		}
	}
	return true
}

func (c *Core) controllerURL(path string) string {
	s := c.store.Get()
	return fmt.Sprintf("http://127.0.0.1:%d%s", s.ControllerPort, path)
}

func (c *Core) controllerPatch(path string, payload any) ([]byte, error) {
	s := c.store.Get()
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPatch, c.controllerURL(path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Secret)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, string(raw))
	}
	return raw, nil
}

func (c *Core) controllerGet(path string) ([]byte, error) {
	s := c.store.Get()
	req, _ := http.NewRequest(http.MethodGet, c.controllerURL(path), nil)
	req.Header.Set("Authorization", "Bearer "+s.Secret)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("控制器返回 HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func (c *Core) Version() string {
	body, err := c.controllerGet("/version")
	if err != nil {
		return ""
	}
	var v struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(body, &v)
	return v.Version
}

func (c *Core) WaitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.Version() != "" {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("内核启动超时，请查看日志")
}

// applyGroupTuning 覆盖 url-test 组的测速间隔，并过滤信息节点。
// 仅处理 flow 风格（proxy-groups: [...]）的订阅，块状 YAML 原样返回。
func applyGroupTuning(text string, tune bool, interval int) string {
	re := regexp.MustCompile(`(?m)^proxy-groups:[ \t]*(\[.*\])[ \t]*$`)
	loc := re.FindStringSubmatchIndex(text)
	if loc == nil {
		return text
	}
	arr := text[loc[2]:loc[3]]
	var groups []map[string]any
	if err := json.Unmarshal([]byte(arr), &groups); err != nil {
		return text
	}
	for _, g := range groups {
		if names, ok := g["proxies"].([]any); ok {
			kept := make([]any, 0, len(names))
			for _, n := range names {
				name, _ := n.(string)
				if isInfoNode(name) {
					continue
				}
				kept = append(kept, n)
			}
			if len(kept) == 0 {
				kept = append(kept, "DIRECT")
			}
			g["proxies"] = kept
		}
		if tune {
			if t, _ := g["type"].(string); t == "url-test" {
				g["interval"] = interval
				g["tolerance"] = 50
				// lazy 会让内核跳过周期测速，导致自动选择长期不变
				g["lazy"] = false
			}
		}
	}
	out, err := json.Marshal(groups)
	if err != nil {
		return text
	}
	return text[:loc[2]] + string(out) + text[loc[3]:]
}
