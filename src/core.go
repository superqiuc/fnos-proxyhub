package main

import (
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

var reTopKey = regexp.MustCompile(`(?m)^(mixed-port|port|socks-port|external-controller|secret|allow-lan|mode|log-level|ipv6|external-ui|external-ui-url)\s*:.*$`)

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
	fmt.Fprintf(&b, "external-controller: 127.0.0.1:%d\n", s.ControllerPort)
	fmt.Fprintf(&b, "secret: %q\n", s.Secret)
	fmt.Fprintf(&b, "mode: %s\n", s.Mode)
	fmt.Fprintf(&b, "log-level: %s\n", s.LogLevel)
	b.WriteString(clean)
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
