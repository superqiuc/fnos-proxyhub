package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Fetch 拉取订阅文本，兼容 base64 编码的订阅。
func Fetch(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "clash.meta/1.19.32 mihomo")
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载订阅失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("订阅返回 HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	text := strings.TrimSpace(string(body))
	if !strings.Contains(text, "proxies:") {
		if dec, err := base64.StdEncoding.DecodeString(text); err == nil {
			text = strings.TrimSpace(string(dec))
		}
	}
	if !strings.Contains(text, "proxies:") {
		return "", fmt.Errorf("订阅内容不是有效的 Clash/Mihomo 配置")
	}
	return text, nil
}

// UpdateSubscription 拉取并应用订阅：校验通过才覆盖配置，失败保留旧配置。
func (c *Core) UpdateSubscription(ctx context.Context) (int, error) {
	s := c.store.Get()
	if s.SubscriptionURL == "" {
		return 0, fmt.Errorf("请先在设置里填写订阅链接")
	}
	text, err := Fetch(ctx, s.SubscriptionURL)
	if err != nil {
		return 0, err
	}
	cfg := c.BuildConfig(text, s)
	if err := c.Validate(cfg); err != nil {
		return 0, err
	}
	if err := os.WriteFile(c.subPath(), []byte(text), 0o600); err != nil {
		return 0, err
	}
	if err := os.WriteFile(c.configPath(), []byte(cfg), 0o600); err != nil {
		return 0, err
	}
	count := strings.Count(text, "- name:")
	if c.Running() {
		if err := c.Start(s); err != nil {
			return count, err
		}
		if err := c.WaitReady(15 * time.Second); err != nil {
			return count, err
		}
	}
	return count, nil
}

func (c *Core) LastUpdate() time.Time {
	st, err := os.Stat(c.configPath())
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
