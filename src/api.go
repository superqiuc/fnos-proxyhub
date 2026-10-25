package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed webui/index.html webui/app.css webui/app.js
var uiFS embed.FS

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/api/status", a.apiStatus)
	mux.HandleFunc("/api/settings", a.apiSettings)
	mux.HandleFunc("/api/subscription/update", a.apiSubUpdate)
	mux.HandleFunc("/api/nodes", a.apiNodes)
	mux.HandleFunc("/api/nodes/select", a.apiSelect)
	mux.HandleFunc("/api/core/restart", a.apiRestart)
	mux.HandleFunc("/api/logs", a.apiLogs)
	mux.HandleFunc("/api/nodes/delay", a.apiDelay)
	mux.HandleFunc("/pac", a.apiPAC)
	mux.HandleFunc("/api/pac", a.apiPACInfo)
	mux.HandleFunc("/api/pac/update", a.apiPACUpdate)
	mux.HandleFunc("/api/core", a.apiCoreInfo)
	mux.HandleFunc("/api/core/update", a.apiCoreUpdate)
	mux.HandleFunc("/", a.serveUI)
	return mux
}

func (a *App) serveUI(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if name != "index.html" && name != "app.js" && name != "app.css" {
		name = "index.html"
	}
	data, err := uiFS.ReadFile("webui/" + name)
	if name == "index.html" {
		data = []byte(strings.ReplaceAll(string(data), "{{V}}", appVersion))
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := "text/html; charset=utf-8"
	if strings.HasSuffix(name, ".js") {
		ct = "application/javascript; charset=utf-8"
	} else if strings.HasSuffix(name, ".css") {
		ct = "text/css; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Write(data)
}

type nodeGroup struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Now     string   `json:"now"`
	Options []string `json:"options"`
}

func (a *App) fetchGroups() ([]nodeGroup, error) {
	body, err := a.core.controllerGet("/proxies")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Proxies map[string]struct {
			Type string   `json:"type"`
			Now  string   `json:"now"`
			All  []string `json:"all"`
		} `json:"proxies"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	groups := []nodeGroup{}
	for name, p := range resp.Proxies {
		switch p.Type {
		case "Selector", "URLTest", "Fallback", "LoadBalance":
			groups = append(groups, nodeGroup{Name: name, Type: p.Type, Now: p.Now, Options: p.All})
		}
	}
	return groups, nil
}

func (a *App) apiStatus(w http.ResponseWriter, r *http.Request) {
	s := a.store.Get()
	groups, _ := a.fetchGroups()
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	// 优先展示常用策略组，避免落到 GLOBAL
	preferred := []string{"PROXY", "Proxy", "节点选择", "手动切换", "SELECT", "选择节点"}
	pick := ""
	for _, name := range preferred {
		for _, g := range groups {
			if g.Name == name && g.Type == "Selector" {
				pick = g.Name
				break
			}
		}
		if pick != "" {
			break
		}
	}
	if pick == "" {
		for _, g := range groups {
			if g.Type == "Selector" && g.Name != "GLOBAL" {
				pick = g.Name
				break
			}
		}
	}
	if pick == "" && len(groups) > 0 {
		pick = groups[0].Name
	}
	current := ""
	options := []string{}
	for _, g := range groups {
		if g.Name == pick {
			current = g.Now
			options = g.Options
			break
		}
	}
	// 解析嵌套分组：FastFly → 自动选择 → 实际节点
	byName := map[string]nodeGroup{}
	for _, g := range groups {
		byName[g.Name] = g
	}
	chain := []string{}
	real := current
	for i := 0; i < 6; i++ {
		g, ok := byName[real]
		if !ok || g.Now == "" {
			break
		}
		chain = append(chain, real)
		real = g.Now
	}
	last := ""
	if t := a.core.LastUpdate(); !t.IsZero() {
		last = t.Format("2006-01-02 15:04")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"running": a.core.Running(), "version": a.core.Version(),
		"mixed_port": s.MixedPort, "controller_port": s.ControllerPort,
		"mode": s.Mode, "allow_lan": s.AllowLAN, "log_level": s.LogLevel, "ipv6": s.IPv6,
		"subscription_set": s.SubscriptionURL != "", "last_update": last,
		"current_node": real, "current_pick": current, "current_chain": strings.Join(chain, " → "), "current_group": pick, "options": options,
		"groups": groups, "app_version": appVersion,
	})
}

func (a *App) apiSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s := a.store.Get()
		s.Secret = "***"
		writeJSON(w, http.StatusOK, s)
	case http.MethodPut:
		cur := a.store.Get()
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			apiErr(w, http.StatusBadRequest, "读取请求失败")
			return
		}
		// 以当前设置为基准，只覆盖请求里出现的字段，避免部分提交把布尔项重置为 false
		in := cur
		if err := json.Unmarshal(body, &in); err != nil {
			apiErr(w, http.StatusBadRequest, "参数错误")
			return
		}
		// 兼容部分字段提交：缺省值沿用当前设置，避免被零值覆盖。
		if in.Listen == "" {
			in.Listen = cur.Listen
		}
		if in.MixedPort <= 0 {
			in.MixedPort = cur.MixedPort
		}
		if in.ControllerPort <= 0 {
			in.ControllerPort = cur.ControllerPort
		}
		if in.Mode == "" {
			in.Mode = cur.Mode
		}
		if in.LogLevel == "" {
			in.LogLevel = cur.LogLevel
		}
		if in.UpdateHours <= 0 {
			in.UpdateHours = cur.UpdateHours
		}
		if in.Secret == "" || strings.Contains(in.Secret, "***") {
			in.Secret = cur.Secret
		}
		coreChanged := in.MixedPort != cur.MixedPort || in.ControllerPort != cur.ControllerPort ||
			in.IPv6 != cur.IPv6 || in.Mode != cur.Mode || in.LogLevel != cur.LogLevel || in.AllowLAN != cur.AllowLAN ||
			in.AutoTune != cur.AutoTune || in.AutoTuneInterval != cur.AutoTuneInterval
		noSub := false
		if coreChanged {
			if err := a.core.SyncConfig(in); err != nil {
				if strings.Contains(err.Error(), "缺少订阅配置") {
					// 还没配置订阅时只保存设置，等订阅就绪后再生效
					noSub = true
				} else {
					apiErr(w, http.StatusBadRequest, "配置无效，未保存："+err.Error())
					return
				}
			}
		}
		if err := a.store.Save(in); err != nil {
			apiErr(w, http.StatusInternalServerError, "保存失败："+err.Error())
			return
		}
		out := a.store.Get()
		needRestart := in.ControllerPort != cur.ControllerPort || in.IPv6 != cur.IPv6 ||
			in.AutoTune != cur.AutoTune || in.AutoTuneInterval != cur.AutoTuneInterval
		hot := in.Mode != cur.Mode || in.LogLevel != cur.LogLevel || in.AllowLAN != cur.AllowLAN || in.MixedPort != cur.MixedPort
		hotApplied := false
		if noSub {
			hot = false
			needRestart = false
		}
		if hot && !a.core.Running() {
			needRestart = true
		} else if hot {
			patch := map[string]any{}
			if in.Mode != cur.Mode {
				patch["mode"] = out.Mode
			}
			if in.LogLevel != cur.LogLevel {
				patch["log-level"] = out.LogLevel
			}
			if in.AllowLAN != cur.AllowLAN {
				patch["allow-lan"] = out.AllowLAN
			}
			if in.MixedPort != cur.MixedPort {
				patch["mixed-port"] = out.MixedPort
			}
			if _, err := a.core.controllerPatch("/configs", patch); err != nil {
				a.logf("热更新失败，将重启内核：%v", err)
				needRestart = true
			} else {
				hotApplied = true
				a.logf("设置已热更新（模式 %s，日志 %s，局域网 %v，混合端口 %d）", out.Mode, out.LogLevel, out.AllowLAN, out.MixedPort)
			}
		}
		restarted := false
		restartErr := ""
		if needRestart {
			err := a.core.Start(out)
			if err != nil && strings.Contains(err.Error(), "请先更新订阅") {
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				if _, e2 := a.core.UpdateSubscription(ctx); e2 == nil {
					err = a.core.Start(out)
				} else {
					err = e2
				}
				cancel()
			}
			if err != nil {
				restartErr = a.rollbackSettings(cur, err)
			} else if err := a.core.WaitReady(15 * time.Second); err != nil {
				restartErr = a.rollbackSettings(cur, err)
			} else {
				restarted = true
				a.logf("设置已保存并重启内核（混合端口 %d，模式 %s，IPv6 %v）", out.MixedPort, out.Mode, out.IPv6)
			}
		} else if !hotApplied {
			a.logf("设置已保存（混合端口 %d，模式 %s）", out.MixedPort, out.Mode)
		}
		out.Secret = "***"
		writeJSON(w, http.StatusOK, map[string]any{"settings": out, "restarted": restarted, "hot_applied": hotApplied, "restart_error": restartErr})
	default:
		apiErr(w, http.StatusMethodNotAllowed, "方法不支持")
	}
}

// rollbackSettings 用旧设置恢复配置并重启，返回错误描述。
func (a *App) rollbackSettings(old Settings, cause error) string {
	a.logf("新设置启动失败，正在回滚：%v", cause)
	if err := a.store.Save(old); err != nil {
		a.logf("回滚保存设置失败：%v", err)
		return cause.Error()
	}
	if err := a.core.SyncConfig(old); err != nil {
		a.logf("回滚配置失败：%v", err)
		return cause.Error()
	}
	if err := a.core.Start(old); err != nil {
		a.logf("回滚后启动失败：%v", err)
		return cause.Error()
	}
	if err := a.core.WaitReady(15 * time.Second); err != nil {
		a.logf("回滚后内核未就绪：%v", err)
		return cause.Error()
	}
	a.logf("已回滚到旧设置，内核恢复运行")
	return cause.Error() + "（已回滚到原设置）"
}

func (a *App) apiSubUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiErr(w, http.StatusMethodNotAllowed, "方法不支持")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	n, err := a.core.UpdateSubscription(ctx)
	if err != nil {
		a.logf("更新订阅失败：%v", err)
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.logf("订阅更新成功，节点数约 %d", n)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "nodes": n})
}

func (a *App) apiNodes(w http.ResponseWriter, r *http.Request) {
	groups, err := a.fetchGroups()
	if err != nil {
		apiErr(w, http.StatusBadGateway, "读取节点失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (a *App) apiSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiErr(w, http.StatusMethodNotAllowed, "方法不支持")
		return
	}
	var req struct {
		Group string `json:"group"`
		Name  string `json:"name"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
	if req.Group == "" || req.Name == "" {
		apiErr(w, http.StatusBadRequest, "参数不完整")
		return
	}
	s := a.store.Get()
	body := strings.NewReader(fmt.Sprintf(`{"name":%q}`, req.Name))
	u := fmt.Sprintf("http://127.0.0.1:%d/proxies/%s", s.ControllerPort, url.PathEscape(req.Group))
	httpReq, _ := http.NewRequest(http.MethodPut, u, body)
	httpReq.Header.Set("Authorization", "Bearer "+s.Secret)
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		apiErr(w, http.StatusBadGateway, "切换失败："+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		apiErr(w, http.StatusBadGateway, fmt.Sprintf("切换失败：HTTP %d", resp.StatusCode))
		return
	}
	a.logf("切换节点：%s → %s", req.Group, req.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiErr(w, http.StatusMethodNotAllowed, "方法不支持")
		return
	}
	s := a.store.Get()
	if err := a.core.Start(s); err != nil {
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := a.core.WaitReady(15 * time.Second); err != nil {
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.logf("内核已重启：%s", a.core.Version())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": a.core.Version()})
}

// isInfoNode 识别订阅里的信息条目（流量、到期、官网等），它们不是真实节点。
func isInfoNode(name string) bool {
	for _, kw := range []string{"剩余流量", "套餐到期", "到期", "官网", "网址", "地址", "流量", "订阅"} {
		if strings.Contains(name, kw) {
			return true
		}
	}
	return false
}

type delayResult struct {
	Name  string `json:"name"`
	Delay int    `json:"delay"`
	Error string `json:"error,omitempty"`
}

// apiDelay 对策略组内所有节点并发测速。
func (a *App) apiDelay(w http.ResponseWriter, r *http.Request) {
	group := r.URL.Query().Get("group")
	if group == "" {
		apiErr(w, http.StatusBadRequest, "缺少策略组")
		return
	}
	groups, err := a.fetchGroups()
	if err != nil {
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	var options []string
	for _, g := range groups {
		if g.Name == group {
			options = g.Options
			break
		}
	}
	if len(options) == 0 {
		apiErr(w, http.StatusNotFound, "策略组不存在")
		return
	}
	s := a.store.Get()
	results := make([]delayResult, len(options))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i, name := range options {
		if name == "DIRECT" || name == "REJECT" || isInfoNode(name) {
			results[i] = delayResult{Name: name, Delay: 0}
			continue
		}
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			u := fmt.Sprintf("http://127.0.0.1:%d/proxies/%s/delay?timeout=5000&url=%s",
				s.ControllerPort, url.PathEscape(name), url.QueryEscape("https://www.gstatic.com/generate_204"))
			req, _ := http.NewRequest(http.MethodGet, u, nil)
			req.Header.Set("Authorization", "Bearer "+s.Secret)
			client := &http.Client{Timeout: 8 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				results[i] = delayResult{Name: name, Error: err.Error()}
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			var out struct {
				Delay   int    `json:"delay"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(body, &out)
			if resp.StatusCode != 200 || out.Delay <= 0 {
				msg := out.Message
				if msg == "" {
					msg = fmt.Sprintf("超时（HTTP %d）", resp.StatusCode)
				}
				results[i] = delayResult{Name: name, Error: msg}
				return
			}
			results[i] = delayResult{Name: name, Delay: out.Delay}
		}(i, name)
	}
	wg.Wait()
	a.logf("节点测速完成（%s，%d 个节点）", group, len(options))
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "group": group})
}

func (a *App) apiCoreInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	latest, err := a.latestCoreVersion(ctx)
	body := map[string]any{"current": a.core.Version(), "latest": latest}
	if err != nil {
		body["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

func (a *App) apiCoreUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiErr(w, http.StatusMethodNotAllowed, "方法不支持")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cur, latest, updated, err := a.UpdateCore(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "current": cur, "latest": latest})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"current": cur, "latest": latest, "updated": updated})
}

func (a *App) apiLogs(w http.ResponseWriter, r *http.Request) {
	tail := a.coreLogTail(120)
	writeJSON(w, http.StatusOK, map[string]any{"lines": a.logs.Tail(200), "core": tail})
}
