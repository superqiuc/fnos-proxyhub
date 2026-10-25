package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const appVersion = "1.1.0"

type App struct {
	appDir   string
	dataDir  string
	listen   string
	store    *Store
	core     *Core
	logs     *LogBuffer
	pacStore *pacStore
}

func NewApp(appDir, dataDir, listen string) (*App, error) {
	absApp, _ := filepath.Abs(appDir)
	absData, _ := filepath.Abs(dataDir)
	if err := os.MkdirAll(absData, 0o755); err != nil {
		return nil, err
	}
	store, err := NewStore(filepath.Join(absData, "settings.json"))
	if err != nil {
		return nil, err
	}
	s := store.Get()
	if listen == "" {
		listen = s.Listen
	}
	a := &App{
		appDir: absApp, dataDir: absData, listen: listen,
		store: store, core: NewCore(absApp, absData, store),
		logs:     NewLogBuffer(filepath.Join(absData, "app.log")),
		pacStore: &pacStore{},
	}
	return a, nil
}

func (a *App) logf(format string, args ...any) {
	a.logs.Add(fmt.Sprintf(format, args...))
}

// Boot 启动内核：有配置直接用；没有配置则尝试拉订阅。
func portFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// pickControllerPort 控制器端口被占用时自动换一个空闲端口。
func (a *App) pickControllerPort(s Settings) Settings {
	if portFree(s.ControllerPort) {
		return s
	}
	for p := 9091; p < 9200; p++ {
		if portFree(p) {
			a.logf("控制器端口 %d 被占用，改用 %d", s.ControllerPort, p)
			s.ControllerPort = p
			_ = a.store.Save(s)
			return s
		}
	}
	return s
}

func (a *App) Boot() {
	s := a.store.Get()
	s = a.pickControllerPort(s)
	if !portFree(s.MixedPort) {
		a.logf("混合端口 %d 已被占用，请到设置页修改端口后重启内核", s.MixedPort)
		return
	}
	if _, err := os.Stat(a.core.configPath()); err != nil {
		if s.SubscriptionURL == "" {
			a.logf("尚未配置订阅，等待用户在设置页填写")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		n, err := a.core.UpdateSubscription(ctx)
		cancel()
		if err != nil {
			a.logf("首次拉取订阅失败：%v", err)
			return
		}
		a.logf("首次拉取订阅成功，节点数约 %d", n)
	}
	if err := a.core.Start(s); err != nil {
		a.logf("启动内核失败：%v", err)
		return
	}
	if err := a.core.WaitReady(10 * time.Second); err != nil {
		a.logf("内核未就绪：%v", err)
		return
	}
	a.logf("内核已启动：%s，混合端口 %d", a.core.Version(), s.MixedPort)
}

func (a *App) autoUpdateLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s := a.store.Get()
			if !s.AutoUpdate || s.SubscriptionURL == "" {
				continue
			}
			last := a.core.LastUpdate()
			if !last.IsZero() && time.Since(last) < time.Duration(s.UpdateHours)*time.Hour {
				continue
			}
			a.logf("自动更新订阅（间隔 %d 小时）", s.UpdateHours)
			uctx, cancel := context.WithTimeout(ctx, 90*time.Second)
			n, err := a.core.UpdateSubscription(uctx)
			cancel()
			if err != nil {
				a.logf("自动更新失败：%v", err)
			} else {
				a.logf("自动更新成功，节点数约 %d", n)
			}
		}
	}
}

func (a *App) Run() error {
	a.loadPAC()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.autoUpdateLoop(ctx)
	a.Boot()
	srv := &http.Server{Addr: ":" + a.listen, Handler: a.routes()}
	a.logf("代理中心已启动（版本 %s，端口 %s）", appVersion, a.listen)
	return srv.ListenAndServe()
}

func (a *App) coreLogTail(n int) []string {
	b, err := os.ReadFile(a.core.logPath())
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

var _ = exec.Command
