package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	gfwlistURL        = "https://raw.githubusercontent.com/gfwlist/gfwlist/master/gfwlist.txt"
	builtinPACVersion = "2026-10-31"
)

//go:embed gfwlist.txt
var gfwlistEmbed string

type pacRules struct {
	Direct   []string `json:"direct"`
	Suffix   []string `json:"suffix"`
	Keyword  []string `json:"keyword"`
	Patterns []string `json:"patterns"`
}

func (r *pacRules) count() int {
	if r == nil {
		return 0
	}
	return len(r.Direct) + len(r.Suffix) + len(r.Keyword) + len(r.Patterns)
}

type pacStore struct {
	mu      sync.RWMutex
	rules   *pacRules
	updated time.Time
	source  string // builtin / online
	version string
	err     string
	busy    bool
}

func (a *App) pac() *pacStore { return a.pacStore }

func (a *App) pacPath() string { return filepath.Join(a.dataDir, "pac.json") }

// loadPAC 优先使用本地已保存的规则，否则使用内置快照，不联网。
func (a *App) loadPAC() {
	st := a.pac()
	if b, err := os.ReadFile(a.pacPath()); err == nil {
		var saved struct {
			Rules   pacRules  `json:"rules"`
			Updated time.Time `json:"updated"`
			Version string    `json:"version"`
		}
		if json.Unmarshal(b, &saved) == nil && saved.Rules.count() > 0 {
			st.mu.Lock()
			st.rules = &saved.Rules
			st.updated = saved.Updated
			st.source = "online"
			st.version = saved.Version
			st.mu.Unlock()
			a.logf("PAC 已加载本地规则：%d 条（%s）", saved.Rules.count(), saved.Version)
			return
		}
	}
	cached := a.loadBuiltinPAC()
	st.mu.Lock()
	st.rules = cached
	st.source = "builtin"
	st.version = builtinPACVersion
	st.mu.Unlock()
	a.logf("PAC 使用内置规则：%d 条（%s）", cached.count(), builtinPACVersion)
}

func (a *App) loadBuiltinPAC() *pacRules {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(gfwlistEmbed))
	if err != nil {
		raw = []byte(gfwlistEmbed)
	}
	return parseGFWList(string(raw))
}

// UpdatePAC 手动更新：通过当前代理拉取 gfwlist 并保存到 data/pac.json。
func (a *App) UpdatePAC(ctx context.Context) error {
	st := a.pac()
	st.mu.Lock()
	if st.busy {
		st.mu.Unlock()
		return fmt.Errorf("正在更新，请稍候")
	}
	st.busy = true
	st.mu.Unlock()
	defer func() {
		st.mu.Lock()
		st.busy = false
		st.mu.Unlock()
	}()

	body, err := a.fetchThroughProxy(ctx, gfwlistURL)
	if err != nil {
		client := &http.Client{Timeout: 45 * time.Second}
		if resp, e2 := client.Get(gfwlistURL); e2 == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				body, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			}
		}
	}
	if err != nil {
		st.mu.Lock()
		st.err = err.Error()
		st.mu.Unlock()
		return fmt.Errorf("获取 gfwlist 失败：%v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		raw = body
	}
	rules := parseGFWList(string(raw))
	if rules.count() == 0 {
		return fmt.Errorf("gfwlist 内容为空")
	}
	now := time.Now()
	version := now.Format("2006-01-02 15:04")
	saved, _ := json.MarshalIndent(map[string]any{"rules": rules, "updated": now, "version": version}, "", " ")
	_ = os.WriteFile(a.pacPath(), saved, 0o644)
	st.mu.Lock()
	st.rules = rules
	st.updated = now
	st.source = "online"
	st.version = version
	st.err = ""
	st.mu.Unlock()
	a.logf("PAC 已更新：%d 条规则（%s）", rules.count(), version)
	return nil
}

func (a *App) fetchThroughProxy(ctx context.Context, target string) ([]byte, error) {
	s := a.store.Get()
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", s.MixedPort))
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), ResponseHeaderTimeout: 30 * time.Second}
	client := &http.Client{Timeout: 60 * time.Second, Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "proxyhub-pac/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

func parseGFWList(text string) *pacRules {
	r := &pacRules{}
	direct := map[string]bool{}
	suffix := map[string]bool{}
	keyword := map[string]bool{}
	pattern := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "!") || strings.HasPrefix(line, "[") {
			continue
		}
		if strings.Contains(line, "[AutoProxy") {
			continue
		}
		isDirect := strings.HasPrefix(line, "@@")
		if isDirect {
			line = strings.TrimPrefix(line, "@@")
		}
		if strings.HasPrefix(line, "||") {
			line = line[2:]
		} else if strings.HasPrefix(line, "|") {
			line = line[1:]
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") && strings.HasSuffix(line, "/") && len(line) > 2 {
			pat := line[1 : len(line)-1]
			if isDirect {
				continue
			}
			if _, err := regexp.Compile(pat); err == nil {
				pattern[pat] = true
			}
			continue
		}
		if strings.Contains(line, "*") {
			parts := strings.Split(line, "*")
			var b strings.Builder
			for _, p := range parts {
				p = strings.Trim(p, ".")
				if p == "" {
					continue
				}
				if b.Len() > 0 {
					b.WriteString(".*")
				}
				b.WriteString(regexpQuote(p))
			}
			if b.Len() > 0 && !isDirect {
				pattern["^.*"+b.String()+".*$"] = true
			}
			continue
		}
		d := line
		if i := strings.Index(d, "/"); i > 0 {
			d = d[:i]
		}
		d = strings.TrimPrefix(d, "http://")
		d = strings.TrimPrefix(d, "https://")
		d = strings.Trim(d, ".")
		if !strings.Contains(d, ".") {
			if len(d) > 2 && !isDirect {
				keyword[strings.ToLower(d)] = true
			}
			continue
		}
		d = strings.ToLower(d)
		if isDirect {
			direct[d] = true
			continue
		}
		suffix[d] = true
	}
	for d := range direct {
		r.Direct = append(r.Direct, d)
	}
	for d := range suffix {
		if direct[d] {
			continue
		}
		r.Suffix = append(r.Suffix, d)
	}
	for k := range keyword {
		r.Keyword = append(r.Keyword, k)
	}
	for p := range pattern {
		r.Patterns = append(r.Patterns, p)
	}
	sort.Strings(r.Direct)
	sort.Strings(r.Suffix)
	sort.Strings(r.Keyword)
	sort.Strings(r.Patterns)
	return r
}

func regexpQuote(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch c {
		case '.', '+', '?', '(', ')', '[', ']', '{', '}', '^', '$', '\\', '|':
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func jsStringArray(items []string) string {
	b, _ := json.Marshal(items)
	return string(b)
}

func (a *App) buildPAC(proxyHost string) string {
	s := a.store.Get()
	if proxyHost == "" {
		proxyHost = "127.0.0.1"
	}
	proxy := fmt.Sprintf("PROXY %s:%d; DIRECT", proxyHost, s.MixedPort)
	st := a.pac()
	st.mu.RLock()
	rules := st.rules
	st.mu.RUnlock()
	if rules == nil {
		rules = a.loadBuiltinPAC()
	}
	var b strings.Builder
	b.WriteString("// proxyhub PAC (gfwlist)\n")
	b.WriteString(fmt.Sprintf("var PROXY = '%s';\n", proxy))
	b.WriteString("var DIRECT_LIST = " + jsStringArray(rules.Direct) + ";\n")
	b.WriteString("var SUFFIX_LIST = " + jsStringArray(rules.Suffix) + ";\n")
	b.WriteString("var KEYWORD_LIST = " + jsStringArray(rules.Keyword) + ";\n")
	b.WriteString("var REGEX_SRC = " + jsStringArray(rules.Patterns) + ";\n")
	b.WriteString("var REGEX_LIST = [];\n")
	b.WriteString("for (var r = 0; r < REGEX_SRC.length; r++) { try { REGEX_LIST.push(new RegExp(REGEX_SRC[r])); } catch (e) {} }\n")
	b.WriteString(`function FindProxyForURL(url, host) {
  host = host.toLowerCase();
  if (isPlainHostName(host)) return "DIRECT";
  for (var i = 0; i < DIRECT_LIST.length; i++) {
    var d = DIRECT_LIST[i];
    if (host === d || dnsDomainIs(host, "." + d)) return "DIRECT";
  }
  for (var j = 0; j < SUFFIX_LIST.length; j++) {
    var s = SUFFIX_LIST[j];
    if (host === s || dnsDomainIs(host, "." + s)) return PROXY;
  }
  for (var k = 0; k < KEYWORD_LIST.length; k++) {
    if (host.indexOf(KEYWORD_LIST[k]) >= 0) return PROXY;
  }
  for (var m = 0; m < REGEX_LIST.length; m++) {
    if (REGEX_LIST[m].test(host)) return PROXY;
  }
  return "DIRECT";
}
`)
	return b.String()
}

// lanIP 返回本机第一个可用的局域网 IPv4。
func lanIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(iface.Name)
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "virbr") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			ip := ipnet.IP.To4()
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			return ip.String()
		}
	}
	return ""
}

// pacBase 返回 PAC 的访问基地址：优先手动配置，否则用本机局域网 IP + Web 端口。
func (a *App) pacBase(r *http.Request) string {
	s := a.store.Get()
	host := strings.TrimSpace(s.PACHost)
	if host != "" {
		if strings.Contains(host, "://") {
			if u, err := url.Parse(host); err == nil {
				host = u.Host
			}
		}
		if !strings.Contains(host, ":") {
			port := a.listen
			if port == "" {
				port = s.Listen
			}
			host = host + ":" + port
		}
		return "http://" + host
	}
	ip := lanIP()
	if ip == "" {
		ip = "127.0.0.1"
	}
	port := a.listen
	if port == "" {
		port = s.Listen
	}
	if port == "" {
		port = "8898"
	}
	return fmt.Sprintf("http://%s:%s", ip, port)
}

func (a *App) proxyHostFor(r *http.Request) string {
	base := a.pacBase(r)
	u, err := url.Parse(base)
	if err != nil {
		return "127.0.0.1"
	}
	return u.Hostname()
}

func (a *App) apiPAC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(a.buildPAC(a.proxyHostFor(r))))
}

func (a *App) apiPACInfo(w http.ResponseWriter, r *http.Request) {
	st := a.pac()
	st.mu.RLock()
	defer st.mu.RUnlock()
	base := a.pacBase(r)
	host, port := "", ""
	if u, err := url.Parse(base); err == nil {
		host = u.Hostname()
		port = u.Port()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url":     base + "/pac",
		"host":    host,
		"port":    port,
		"path":    "/pac",
		"rules":   st.rules.count(),
		"source":  st.source,
		"version": st.version,
		"updated": func() string {
			if st.updated.IsZero() {
				return ""
			}
			return st.updated.Format("2006-01-02 15:04:05")
		}(),
		"error": st.err,
		"busy":  st.busy,
	})
}

func (a *App) apiPACUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiErr(w, http.StatusMethodNotAllowed, "方法不支持")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := a.UpdatePAC(ctx); err != nil {
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.apiPACInfo(w, r)
}
