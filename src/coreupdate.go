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
	"sort"
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

const maxCoreRawSize = 200 << 20

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
	// 限制解压后大小，避免恶意 gzip 炸弹写满数据盘。
	n, copyErr := io.Copy(f, io.LimitReader(zr, maxCoreRawSize+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || n > maxCoreRawSize {
		os.Remove(tmp)
		if n > maxCoreRawSize {
			return "", fmt.Errorf("内核解压后超过 %d MB，已拒绝", maxCoreRawSize>>20)
		}
		if copyErr != nil {
			return "", copyErr
		}
		return "", closeErr
	}
	if err := verifyCoreELF(tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	out, err := exec.Command(tmp, "-v").CombinedOutput()
	text := strings.ToLower(string(out))
	if err != nil || !strings.Contains(text, "mihomo") || !strings.Contains(text, strings.ToLower(version)) {
		os.Remove(tmp)
		return "", fmt.Errorf("新内核校验失败：%v %s", err, strings.TrimSpace(string(out)))
	}
	return tmp, nil
}

// verifyCoreELF 在运行新内核前确认下载内容确实是 Linux ELF。
func verifyCoreELF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var h [4]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return fmt.Errorf("读取内核文件头失败：%v", err)
	}
	if string(h[:]) != "\x7fELF" {
		return fmt.Errorf("下载内容不是 ELF 可执行文件")
	}
	return nil
}

// UpdateCore 下载并原子替换内核，启动或就绪失败时回滚到备份。
func (a *App) UpdateCore(ctx context.Context) (current, latest string, updated bool, err error) {
	a.coreUpdateMu.Lock()
	defer a.coreUpdateMu.Unlock()
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

	backup, err := a.backupCore(current)
	if err != nil {
		return current, latest, false, fmt.Errorf("备份当前内核失败，未做替换：%v", err)
	}
	if err := a.beginCoreUpdate(backup); err != nil {
		return current, latest, false, fmt.Errorf("写入更新标记失败，未做替换：%v", err)
	}
	a.core.Stop()
	if err := a.installCore(tmp); err != nil {
		// 替换是「先写 .new 再 rename」，失败时旧内核文件没有被改动。
		a.finishCoreUpdate()
		if serr := a.core.Start(a.store.Get()); serr != nil {
			a.logf("替换失败后重启旧内核失败：%v", serr)
		}
		return current, latest, false, fmt.Errorf("替换内核失败：%v", err)
	}
	if err := a.core.Start(a.store.Get()); err != nil {
		if rerr := a.restoreCore(backup); rerr != nil {
			return current, latest, false, fmt.Errorf("新内核启动失败（%v），且回滚失败：%v", err, rerr)
		}
		a.finishCoreUpdate()
		if serr := a.core.Start(a.store.Get()); serr != nil {
			return current, latest, false, fmt.Errorf("新内核启动失败（%v），回滚后仍无法启动：%v", err, serr)
		}
		return current, latest, false, fmt.Errorf("新内核启动失败，已回滚：%v", err)
	}
	if err := a.core.WaitReady(15 * time.Second); err != nil {
		a.core.Stop()
		if rerr := a.restoreCore(backup); rerr != nil {
			return current, latest, false, fmt.Errorf("新内核未就绪（%v），且回滚失败：%v", err, rerr)
		}
		a.finishCoreUpdate()
		if serr := a.core.Start(a.store.Get()); serr != nil {
			return current, latest, false, fmt.Errorf("新内核未就绪（%v），回滚后仍无法启动：%v", err, serr)
		}
		return current, latest, false, fmt.Errorf("新内核未就绪，已回滚：%v", err)
	}
	a.finishCoreUpdate()
	a.pruneBackups(filepath.Join(a.dataDir, "backups"), 5)
	a.logf("内核已更新：%s -> %s", current, latest)
	return current, latest, true, nil
}

// backupCore 把当前内核复制到数据目录，返回备份路径。
func (a *App) backupCore(current string) (string, error) {
	old, err := os.ReadFile(a.core.corePath())
	if err != nil {
		return "", err
	}
	dir := filepath.Join(a.dataDir, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := strings.NewReplacer("/", "_", "\\", "_", " ", "_").Replace(current)
	if name == "" {
		name = "unknown"
	}
	path := filepath.Join(dir, fmt.Sprintf("mihomo-%s-%s", name, time.Now().Format("20060102-150405")))
	if err := writeFileAtomic(path, old, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

const updateMarkerName = "core-update.inprogress"

func (a *App) updateMarkerPath() string { return filepath.Join(a.dataDir, updateMarkerName) }

// beginCoreUpdate 在替换内核前落一个标记，进程中途被杀时下次启动可据此回滚。
func (a *App) beginCoreUpdate(backup string) error {
	return writeFileAtomic(a.updateMarkerPath(), []byte(backup), 0o600)
}

func (a *App) finishCoreUpdate() {
	_ = os.Remove(a.updateMarkerPath())
}

// recoverInterruptedUpdate 在启动时检查未完成的更新标记并回滚。
func (a *App) recoverInterruptedUpdate() {
	path := a.updateMarkerPath()
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	backup := strings.TrimSpace(string(b))
	if backup == "" {
		_ = os.Remove(path)
		return
	}
	a.logf("检测到未完成的内核更新，尝试回滚到备份：%s", filepath.Base(backup))
	if err := a.restoreCore(backup); err != nil {
		if os.IsNotExist(err) {
			a.logf("更新备份不存在，保留当前内核：%v", err)
			_ = os.Remove(path)
			return
		}
		a.logf("自动回滚失败，保留更新标记以便下次重试：%v", err)
		return
	}
	_ = os.Remove(path)
	a.logf("已回滚到备份内核")
}

// pruneBackups 只保留最近 keep 份内核备份，避免数据目录无限增长。
func (a *App) pruneBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		path string
		mod  time.Time
	}
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "mihomo-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: filepath.Join(dir, e.Name()), mod: info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	for i := keep; i < len(items); i++ {
		_ = os.Remove(items[i].path)
	}
}

// installCore 先写同目录临时文件再 rename，避免复制中途失败损坏现有内核。
func (a *App) installCore(src string) error {
	dst := a.core.corePath()
	staged := dst + ".new"
	_ = os.Remove(staged)
	if err := copyFileMode(src, staged, 0o755); err != nil {
		_ = os.Remove(staged)
		return err
	}
	if err := os.Rename(staged, dst); err != nil {
		_ = os.Remove(staged)
		return err
	}
	return nil
}

// restoreCore 用备份原子恢复内核。
func (a *App) restoreCore(backup string) error {
	staged := a.core.corePath() + ".rollback"
	_ = os.Remove(staged)
	if err := copyFileMode(backup, staged, 0o755); err != nil {
		_ = os.Remove(staged)
		return err
	}
	if err := os.Rename(staged, a.core.corePath()); err != nil {
		_ = os.Remove(staged)
		return err
	}
	return nil
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
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}
