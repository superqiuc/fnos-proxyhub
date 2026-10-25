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
	appDir    string
	dataDir   string
	store     *Store
	cmd       *exec.Cmd
	coreVer   string
	startedAt time.Time
}

func NewCore(appDir, dataDir string, store *Store) *Core {
	return &Core{appDir: appDir, dataDir: dataDir, store: store}
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
	tmp := filepath.Join(c.dataDir, "config.check.yaml")
	if err := os.WriteFile(tmp, []byte(cfg), 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
	go func() { _ = cmd.Wait(); logf.Close() }()
	c.cmd = cmd
	c.startedAt = time.Now()
	return nil
}

func (c *Core) stopLocked() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _, _ = c.cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = c.cmd.Process.Kill()
		}
		c.cmd = nil
	}
}

func (c *Core) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

func (c *Core) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cmd != nil && c.cmd.Process != nil
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
