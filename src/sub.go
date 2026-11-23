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
	// 拉取期间设置可能被修改，重新读取，避免把旧端口/模式写回配置。
	s = c.store.Get()
	cfg := c.BuildConfig(text, s)
	if err := c.Validate(cfg); err != nil {
		return 0, err
	}
	if err := writeFileAtomic(c.subPath(), []byte(text), 0o600); err != nil {
		return 0, err
	}
	if err := writeFileAtomic(c.configPath(), []byte(cfg), 0o600); err != nil {
		return 0, err
	}
	count := countProxies(text)
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

// countProxies 统计订阅中的代理节点数，兼容 flow 风格（proxies: [{...}]）
// 和常见的块风格（proxies:\n  - name: ...）。仅用于界面提示，允许近似。
func countProxies(text string) int {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "proxies:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "proxies:"))
		if strings.HasPrefix(rest, "[") {
			var b strings.Builder
			b.WriteString(rest)
			for j := i + 1; j < len(lines) && !flowArrayClosed(b.String()); j++ {
				b.WriteString("\n")
				b.WriteString(lines[j])
			}
			return countFlowObjects(b.String())
		}
		count := 0
		for j := i + 1; j < len(lines); j++ {
			l := lines[j]
			trimmed := strings.TrimSpace(l)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if l[0] != ' ' && l[0] != '\t' {
				break
			}
			if strings.HasPrefix(trimmed, "- name:") {
				count++
			}
		}
		return count
	}
	return 0
}

func flowArrayClosed(s string) bool {
	depth := 0
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	return depth <= 0
}

func countFlowObjects(s string) int {
	depth := 0
	inStr, esc := false, false
	count := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[', '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
			if depth == 1 {
				count++
			}
		case ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return count
}
