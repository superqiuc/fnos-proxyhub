package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	authCookieName = "proxyhub_auth"
	authSessionTTL = 30 * 24 * time.Hour
	passwordMinLen = 6
	passwordMaxLen = 128
)

// hashPassword 使用随机盐 + SHA-256 保存口令，避免设置文件里出现明文。
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	saltHex := hex.EncodeToString(salt)
	sum := sha256.Sum256([]byte("proxyhub:" + saltHex + ":" + pw))
	return "sha256$" + saltHex + "$" + hex.EncodeToString(sum[:]), nil
}

func verifyPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 3 || parts[0] != "sha256" {
		return false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte("proxyhub:" + parts[1] + ":" + pw))
	return subtle.ConstantTimeCompare(sum[:], want) == 1
}

// authToken 由内核 secret、口令哈希与会话到期时间共同派生；
// 改密码或令牌过期后旧会话自动失效。
func (a *App) authToken(s Settings, expires int64) string {
	mac := hmac.New(sha256.New, []byte(s.Secret))
	fmt.Fprintf(mac, "proxyhub-auth-v2:%s:%d", s.AccessPassword, expires)
	return strconv.FormatInt(expires, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

// parseAuthExpiry 取出令牌里的到期时间（Unix 秒），签名在 authenticated 中校验。
func parseAuthExpiry(token string) (int64, bool) {
	i := strings.IndexByte(token, '.')
	if i <= 0 || i >= len(token)-1 {
		return 0, false
	}
	exp, err := strconv.ParseInt(token[:i], 10, 64)
	if err != nil || exp <= 0 {
		return 0, false
	}
	return exp, true
}

func (a *App) authEnabled() bool {
	return a.store.Get().AccessPassword != ""
}

func (a *App) authenticated(r *http.Request) bool {
	s := a.store.Get()
	if s.AccessPassword == "" {
		return true
	}
	c, err := r.Cookie(authCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	exp, ok := parseAuthExpiry(c.Value)
	if !ok || time.Now().Unix() >= exp {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.authToken(s, exp))) == 1
}

func (a *App) setAuthCookie(w http.ResponseWriter, s Settings) {
	expires := time.Now().Add(authSessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    a.authToken(s, expires.Unix()),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(authSessionTTL.Seconds()),
		Expires:  expires,
	})
}

func (a *App) clearAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// requireAuth 保护管理接口；未启用口令时保持原来的直接访问。
func (a *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.authenticated(r) {
			next(w, r)
			return
		}
		apiErr(w, http.StatusUnauthorized, "需要登录")
	}
}

func (a *App) apiAuthStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       a.authEnabled(),
		"authenticated": a.authenticated(r),
	})
}

func (a *App) apiLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiErr(w, http.StatusMethodNotAllowed, "方法不支持")
		return
	}
	s := a.store.Get()
	if s.AccessPassword == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false})
		return
	}
	ip := clientIP(r)
	if wait, blocked := defaultLoginGuard.blocked(ip); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		apiErr(w, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		apiErr(w, http.StatusBadRequest, "参数错误")
		return
	}
	if !verifyPassword(s.AccessPassword, req.Password) {
		// 失败时计入锁定并固定延迟，降低局域网内的在线爆破速度。
		defaultLoginGuard.fail(ip)
		time.Sleep(500 * time.Millisecond)
		a.logf("登录失败：%s", ip)
		apiErr(w, http.StatusUnauthorized, "密码错误")
		return
	}
	defaultLoginGuard.success(ip)
	a.setAuthCookie(w, s)
	a.logf("登录成功：%s", ip)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": true})
}

func (a *App) apiLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiErr(w, http.StatusMethodNotAllowed, "方法不支持")
		return
	}
	a.clearAuthCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// apiSetPassword 设置或关闭访问密码；关闭后管理接口恢复免登录。
func (a *App) apiSetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		apiErr(w, http.StatusMethodNotAllowed, "方法不支持")
		return
	}
	var req struct {
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		apiErr(w, http.StatusBadRequest, "参数错误")
		return
	}
	pw := req.NewPassword
	if pw != "" && (len(pw) < passwordMinLen || len(pw) > passwordMaxLen) {
		apiErr(w, http.StatusBadRequest, fmt.Sprintf("密码长度需为 %d-%d 位", passwordMinLen, passwordMaxLen))
		return
	}
	hashed := ""
	if pw != "" {
		var err error
		hashed, err = hashPassword(pw)
		if err != nil {
			apiErr(w, http.StatusInternalServerError, "密码处理失败")
			return
		}
	}
	s, err := a.store.Update(func(st *Settings) {
		st.AccessPassword = hashed
	})
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	if pw == "" {
		a.clearAuthCookie(w)
		a.logf("访问密码已关闭")
	} else {
		a.setAuthCookie(w, s)
		a.logf("访问密码已设置")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": pw != ""})
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

const (
	loginMaxFails   = 5
	loginBlockFor   = 30 * time.Second
	loginFailWindow = 10 * time.Minute
)

type loginAttempt struct {
	fails        int
	last         time.Time
	blockedUntil time.Time
}

type loginGuard struct {
	mu       sync.Mutex
	attempts map[string]*loginAttempt
}

func newLoginGuard() *loginGuard {
	return &loginGuard{attempts: map[string]*loginAttempt{}}
}

var defaultLoginGuard = newLoginGuard()

func (g *loginGuard) blocked(ip string) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.attempts[ip]
	if a == nil {
		return 0, false
	}
	now := time.Now()
	if a.blockedUntil.After(now) {
		return time.Until(a.blockedUntil), true
	}
	if now.Sub(a.last) > loginFailWindow {
		delete(g.attempts, ip)
	}
	return 0, false
}

func (g *loginGuard) fail(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	a := g.attempts[ip]
	if a == nil || now.Sub(a.last) > loginFailWindow {
		a = &loginAttempt{}
		g.attempts[ip] = a
	}
	a.last = now
	a.fails++
	if a.fails >= loginMaxFails {
		a.blockedUntil = now.Add(loginBlockFor)
		a.fails = 0
	}
	if len(g.attempts) > 512 {
		for key, attempt := range g.attempts {
			if now.Sub(attempt.last) > loginFailWindow {
				delete(g.attempts, key)
			}
		}
	}
}

func (g *loginGuard) success(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.attempts, ip)
}
