package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// mihomo 在首次遇到 GEOIP / GEOSITE 规则时会自己去 GitHub 下载地理数据。
// 国内网络到 GitHub 往往只有几 KB/s，而配置校验又在几十秒内超时，结果就是
// 「配置校验失败 -> 不写配置 -> 内核永远起不来」。这里在验证配置前先把需要的
// 地理数据从镜像下载到数据目录，mihomo 校验时直接读本地文件即可。
const geoRulesRepo = "MetaCubeX/meta-rules-dat"

// geodataMaxAge 是单个文件一次下载的总时限。
const geodataMaxAge = 4 * time.Minute

type geoAsset struct {
	Name    string // mihomo 读取的文件名（大小写固定）
	Remote  string // 镜像/Release 中的文件名（mirror 大小写敏感）
	Label   string
	MinSize int64 // 小于该大小视为残缺文件，需要重新下载
}

var geoAssetDefs = map[string]geoAsset{
	"GeoSite.dat":  {Name: "GeoSite.dat", Remote: "geosite.dat", Label: "GeoSite 域名规则", MinSize: 2 << 20},
	"GeoIP.dat":    {Name: "GeoIP.dat", Remote: "geoip.dat", Label: "GeoIP 国家规则", MinSize: 8 << 20},
	"Country.mmdb": {Name: "Country.mmdb", Remote: "country.mmdb", Label: "国家 IP 库", MinSize: 4 << 20},
	"ASN.mmdb":     {Name: "ASN.mmdb", Remote: "GeoLite2-ASN.mmdb", Label: "ASN 数据库", MinSize: 1 << 20},
}

var reGeodataMode = regexp.MustCompile(`(?m)^\s*geodata-mode\s*:\s*["']?true\b`)

// geoNeeds 根据配置内容推断需要哪些地理数据文件。
func geoNeeds(cfg string) []string {
	upper := strings.ToUpper(cfg)
	need := make([]string, 0, 2)
	if strings.Contains(upper, "GEOSITE") {
		need = append(need, "GeoSite.dat")
	}
	if strings.Contains(upper, "GEOIP") {
		// mihomo 默认用 Country.mmdb 解析 GEOIP；仅当 geodata-mode: true 时才用 GeoIP.dat。
		if reGeodataMode.MatchString(cfg) {
			need = append(need, "GeoIP.dat")
		} else {
			need = append(need, "Country.mmdb")
		}
	}
	if strings.Contains(upper, "GEOIP,ASN") || strings.Contains(upper, "SRC-GEOIP,ASN") {
		need = append(need, "ASN.mmdb")
	}
	return need
}

// geoURLs 返回按优先级排列的下载地址：国内可直连的 jsDelivr 在前，GitHub 兜底。
func geoURLs(remote string) []string {
	return []string{
		"https://cdn.jsdelivr.net/gh/" + geoRulesRepo + "@release/" + remote,
		"https://gcore.jsdelivr.net/gh/" + geoRulesRepo + "@release/" + remote,
		"https://ghproxy.net/https://github.com/" + geoRulesRepo + "/releases/download/latest/" + remote,
		"https://github.com/" + geoRulesRepo + "/releases/download/latest/" + remote,
	}
}

// ensureGeoData 确保 cfg 需要的地理数据已存在于数据目录，缺失时自动下载。
// 下载失败只记录日志并返回错误，由调用方决定是否继续校验。
func (c *Core) ensureGeoData(cfg string) error {
	need := geoNeeds(cfg)
	if len(need) == 0 {
		return nil
	}
	c.geoMu.Lock()
	defer c.geoMu.Unlock()

	var firstErr error
	for _, name := range need {
		asset, ok := geoAssetDefs[name]
		if !ok {
			continue
		}
		path := filepath.Join(c.dataDir, name)
		if geoAssetReady(path, asset) {
			continue
		}
		if ok, err := c.copyBundledGeo(asset); err != nil {
			c.logf("复制安装包内置的%s失败：%v", asset.Label, err)
		} else if ok {
			c.logf("%s 已从安装包内置数据就绪", asset.Label)
			continue
		}
		c.logf("%s 缺失或已损坏，正在从镜像下载（首次启动需要，请稍候）…", asset.Label)
		if err := c.downloadGeoAsset(asset); err != nil {
			c.logf("%s 下载失败：%v", asset.Label, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		c.logf("%s 已就绪", asset.Label)
	}
	return firstErr
}

// copyBundledGeo 优先把安装包里内置的地理数据复制到数据目录，
// 这样即使机器无法访问镜像，首次启动也能直接工作。
func (c *Core) copyBundledGeo(a geoAsset) (bool, error) {
	if c.appDir == "" {
		return false, nil
	}
	src := filepath.Join(c.appDir, "share", "geo", a.Name)
	if !geoAssetReady(src, a) {
		return false, nil
	}
	dst := filepath.Join(c.dataDir, a.Name)
	tmp := dst + ".copy"
	_ = os.Remove(tmp)
	if err := copyFileMode(src, tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// geoAssetReady 判断本地文件是否可用：大小达标，mmdb 还要带 MaxMind 元数据标记，
// 避免把上次被强杀留下的残缺文件当成有效文件。
func geoAssetReady(path string, a geoAsset) bool {
	st, err := os.Stat(path)
	if err != nil || st.Size() < a.MinSize {
		return false
	}
	if strings.HasSuffix(a.Name, ".mmdb") {
		return hasMMDBMarker(path)
	}
	return true
}

func hasMMDBMarker(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() < 1024 {
		return false
	}
	const tail = 1 << 20
	off := st.Size() - tail
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return false
	}
	return bytes.Contains(buf, []byte("\xab\xcd\xefMaxMind.com"))
}

// purgeGeoData 删除配置需要的地理数据，用于校验失败后重试一次。
func (c *Core) purgeGeoData(cfg string) {
	c.geoMu.Lock()
	defer c.geoMu.Unlock()
	for _, name := range geoNeeds(cfg) {
		_ = os.Remove(filepath.Join(c.dataDir, name))
		_ = os.Remove(filepath.Join(c.dataDir, name+".download"))
	}
}

// needsGeoRetry 判断配置校验失败是否可能与地理数据损坏有关。
func needsGeoRetry(err error, cfg string) bool {
	if err == nil || len(geoNeeds(cfg)) == 0 {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"mmdb", "geosite", "geoip", "can't download"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

func (c *Core) downloadGeoAsset(a geoAsset) error {
	ctx, cancel := context.WithTimeout(context.Background(), geodataMaxAge)
	defer cancel()

	tmp := filepath.Join(c.dataDir, a.Name+".download")
	_ = os.Remove(tmp)
	var lastErr error
	for _, u := range geoURLs(a.Remote) {
		if err := ctx.Err(); err != nil {
			break
		}
		if err := fetchToFile(ctx, u, tmp, a.MinSize); err != nil {
			lastErr = err
			_ = os.Remove(tmp)
			c.logf("%s 下载源不可用（%s）：%v", a.Label, geoHost(u), err)
			continue
		}
		if err := os.Rename(tmp, filepath.Join(c.dataDir, a.Name)); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("下载超时")
	}
	return lastErr
}

func geoHost(rawURL string) string {
	if i := strings.Index(rawURL, "://"); i >= 0 {
		rest := rawURL[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return rawURL
}

func fetchToFile(ctx context.Context, rawURL, dst string, minSize int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "proxyhub/"+appVersion)
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > 0 && resp.ContentLength < minSize {
		return fmt.Errorf("文件大小异常（%d 字节）", resp.ContentLength)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, 128<<20))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n < minSize {
		return fmt.Errorf("下载不完整（%d 字节）", n)
	}
	return nil
}
