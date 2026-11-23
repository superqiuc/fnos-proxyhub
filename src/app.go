package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// appVersion 由构建脚本通过 -ldflags "-X main.appVersion=..." 注入。
var appVersion = "dev"

type App struct {
	appDir   string
	dataDir  string
	listen   string
	store    *Store
	core     *Core
	logs     *LogBuffer
	pacStore *pacStore

	// coreUpdateMu 串行化「换内核」和「重启内核」，避免替换过程中被并发重启打进中间态。
	coreUpdateMu sync.Mutex
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
	if listen == "" {
		listen = "8898"
	}
	logs := NewLogBuffer(filepath.Join(absData, "app.log"))
	a := &App{
		appDir: absApp, dataDir: absData, listen: listen,
		store: store, core: NewCore(absApp, absData, store, logs),
		logs:     logs,
		pacStore: &pacStore{},
	}
	if w := store.Warning(); w != "" {
		a.logf("%s", w)
	}
	return a, nil
}

func (a *App) logf(format string, args ...any) {
	if a.logs != nil {
		a.logs.Add(fmt.Sprintf(format, args...))
	}
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
	// 若上次更新内核时进程被强杀/断电，先回滚到备份再启动。
	a.recoverInterruptedUpdate()
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
		// 首次拉取订阅时可能还要下载地理数据，给足时间（地理数据下载自身有独立超时）。
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
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

// updateSubscription 与内核更新/重启串行化，避免自动更新在换内核的中间态重启进程。
func (a *App) updateSubscription(ctx context.Context) (int, error) {
	a.coreUpdateMu.Lock()
	defer a.coreUpdateMu.Unlock()
	return a.core.UpdateSubscription(ctx)
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
			uctx, cancel := context.WithTimeout(ctx, 300*time.Second)
			n, err := a.updateSubscription(uctx)
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
	// 启动时按当前设置重建配置，保证升级后新的调优参数立即生效
	if _, err := os.Stat(a.core.subPath()); err == nil {
		if err := a.core.SyncConfig(a.store.Get()); err != nil {
			a.logf("启动时重建配置失败：%v", err)
		} else {
			a.logf("启动时已按当前设置重建配置")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	a.Boot()
	// 管理端口绑定失败或服务退出时，确保不留下孤儿 mihomo。
	defer a.core.Stop()
	// 初始启动完成后再开始定时任务，避免首次拉取订阅与自动更新并发。
	go a.autoUpdateLoop(ctx)
	ln, err := net.Listen("tcp", ":"+a.listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-sigCtx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	a.logf("代理中心已启动（版本 %s，端口 %s）", appVersion, a.listen)
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (a *App) coreLogTail(n int) []string {
	return tailLines(a.core.logPath(), 256<<10, n)
}
