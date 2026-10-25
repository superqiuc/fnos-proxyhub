package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const coreRepo = "https://github.com/MetaCubeX/mihomo"

// coreHTTPClient 通过当前运行的 mihomo 代理访问 GitHub，不再走镜像。
func (a *App) coreHTTPClient() *http.Client {
	s := a.store.Get()
	proxyURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", s.MixedPort))
	tr := &http.Transport{
		Proxy:                 http.ProxyURL(proxyURL),
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       60 * time.Second,
	}
	if err != nil {
		tr.Proxy = nil
	}
	return &http.Client{Timeout: 300 * time.Second, Transport: tr}
}

func coreAssetArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64-compatible"
}

func (a *App) fetchURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "proxyhub-core-updater/1.0")
	client := a.coreHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func (a *App) latestCoreVersion(ctx context.Context) (string, error) {
	body, err := a.fetchURL(ctx, coreRepo+"/releases/latest/download/version.txt")
	if err != nil {
		return "", fmt.Errorf("通过代理获取版本失败：%v（请确认代理正在运行且可访问 GitHub）", err)
	}
	v := strings.TrimSpace(string(body))
	if !strings.HasPrefix(v, "v") || len(v) > 24 {
		return "", fmt.Errorf("版本号异常：%q", v)
	}
	return v, nil
}

func (a *App) downloadCore(ctx context.Context, version string) (string, error) {
	asset := fmt.Sprintf("mihomo-linux-%s-%s.gz", coreAssetArch(), version)
	body, err := a.fetchURL(ctx, coreRepo+"/releases/download/"+version+"/"+asset)
	if err != nil {
		return "", fmt.Errorf("通过代理下载内核失败：%v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("解压内核失败：%v", err)
	}
	defer zr.Close()
	tmp := filepath.Join(a.dataDir, "mihomo.new")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, zr)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	out, err := exec.Command(tmp, "-v").CombinedOutput()
	if err != nil || !strings.Contains(strings.ToLower(string(out)), "mihomo") {
		os.Remove(tmp)
		return "", fmt.Errorf("新内核校验失败：%v %s", err, strings.TrimSpace(string(out)))
	}
	return tmp, nil
}

// UpdateCore 下载并替换内核，失败回滚。
func (a *App) UpdateCore(ctx context.Context) (current, latest string, updated bool, err error) {
	current = a.core.Version()
	latest, err = a.latestCoreVersion(ctx)
	if err != nil {
		return current, "", false, err
	}
	if current == latest {
		return current, latest, false, nil
	}
	tmp, err := a.downloadCore(ctx, latest)
	if err != nil {
		return current, latest, false, err
	}
	defer os.Remove(tmp)

	backupDir := filepath.Join(a.dataDir, "backups")
	_ = os.MkdirAll(backupDir, 0o755)
	backup := filepath.Join(backupDir, "mihomo-"+strings.ReplaceAll(current, "/", "_"))
	if old, err := os.ReadFile(a.core.corePath()); err == nil {
		_ = os.WriteFile(backup, old, 0o755)
	}
	a.core.Stop()
	time.Sleep(300 * time.Millisecond)
	if err := copyFileMode(tmp, a.core.corePath(), 0o755); err != nil {
		_ = a.core.Start(a.store.Get())
		return current, latest, false, fmt.Errorf("替换失败：%v", err)
	}
	if err := a.core.Start(a.store.Get()); err != nil {
		if werr := copyFileMode(backup, a.core.corePath(), 0o755); werr == nil {
			_ = a.core.Start(a.store.Get())
		}
		return current, latest, false, fmt.Errorf("新内核启动失败，已回滚：%v", err)
	}
	if err := a.core.WaitReady(15 * time.Second); err != nil {
		return current, latest, false, err
	}
	a.logf("内核已更新：%s -> %s", current, latest)
	return current, latest, true, nil
}

func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}
