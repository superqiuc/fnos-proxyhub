package main

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newAuthTestApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	logs := NewLogBuffer(filepath.Join(dir, "app.log"))
	app := &App{
		appDir: dir, dataDir: dir, listen: "8898",
		store: store, core: NewCore(dir, dir, store, logs),
		logs: logs, pacStore: &pacStore{},
	}
	srv := httptest.NewServer(app.routes())
	t.Cleanup(srv.Close)
	return app, srv
}

func TestPasswordHashRoundTrip(t *testing.T) {
	hashed, err := hashPassword("secret123")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hashed, "secret123") {
		t.Fatal("password must not appear in stored hash")
	}
	if !verifyPassword(hashed, "secret123") {
		t.Fatal("correct password should verify")
	}
	if verifyPassword(hashed, "wrong") {
		t.Fatal("wrong password must not verify")
	}
	if verifyPassword("", "secret123") || verifyPassword("bad", "secret123") {
		t.Fatal("malformed hash must not verify")
	}
}

func TestAuthProtectsAdminAPI(t *testing.T) {
	_, srv := newAuthTestApp(t)
	anon := srv.Client()

	// 未启用密码时保持原来的免登录行为。
	res, err := anon.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("without password status = %d, want 200", res.StatusCode)
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/auth/password", strings.NewReader(`{"new_password":"secret123"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("set password status = %d, want 200", res.StatusCode)
	}

	// 设置密码后，未登录请求应被拒绝。
	res, err = anon.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", res.StatusCode)
	}

	// 登录后携带会话 cookie 可以访问。
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/login", strings.NewReader(`{"password":"secret123"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", res.StatusCode)
	}
	res, err = client.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	_ = json.NewDecoder(res.Body).Decode(&settings)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", res.StatusCode)
	}
	if _, ok := settings["access_password"]; ok {
		t.Fatal("GET /api/settings must not expose access_password")
	}

	// PUT 的响应同样不能带出密码哈希或真实 secret。
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/api/settings", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var putResp struct {
		Settings map[string]any `json:"settings"`
	}
	_ = json.NewDecoder(res.Body).Decode(&putResp)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT settings status = %d, want 200", res.StatusCode)
	}
	if _, ok := putResp.Settings["access_password"]; ok {
		t.Fatal("PUT /api/settings must not expose access_password")
	}
	if putResp.Settings["secret"] != "***" {
		t.Fatalf("PUT /api/settings secret = %v, want ***", putResp.Settings["secret"])
	}

	// 关闭密码后恢复免登录。
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/api/auth/password", strings.NewReader(`{"new_password":""}`))
	req.Header.Set("Content-Type", "application/json")
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("clear password status = %d, want 200", res.StatusCode)
	}
	res, err = anon.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("after clearing password status = %d, want 200", res.StatusCode)
	}
}

func TestAuthStatusEndpoint(t *testing.T) {
	_, srv := newAuthTestApp(t)
	res, err := srv.Client().Get(srv.URL + "/api/auth")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Enabled       bool `json:"enabled"`
		Authenticated bool `json:"authenticated"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Enabled || !out.Authenticated {
		t.Fatalf("unexpected auth status: %+v", out)
	}
}

func TestLoginGuardBlocksAfterFailures(t *testing.T) {
	g := newLoginGuard()
	const ip = "192.0.2.10"
	for i := 0; i < loginMaxFails; i++ {
		g.fail(ip)
	}
	wait, blocked := g.blocked(ip)
	if !blocked || wait <= 0 {
		t.Fatalf("expected ip to be blocked, blocked=%v wait=%v", blocked, wait)
	}
	if _, blocked := g.blocked("192.0.2.11"); blocked {
		t.Fatal("other ip must not be blocked")
	}
	g.success(ip)
	if _, blocked := g.blocked(ip); blocked {
		t.Fatal("successful login should clear the counter")
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	app, srv := newAuthTestApp(t)
	s := app.store.Get()
	hashed, err := hashPassword("secret123")
	if err != nil {
		t.Fatal(err)
	}
	s.AccessPassword = hashed
	if err := app.store.Save(s); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		exp  time.Time
		want int
	}{
		{"expired", time.Now().Add(-time.Minute), http.StatusUnauthorized},
		{"valid", time.Now().Add(time.Minute), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/settings", nil)
			req.AddCookie(&http.Cookie{Name: authCookieName, Value: app.authToken(s, tc.exp.Unix())})
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.want)
			}
		})
	}
}
