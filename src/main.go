package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	appDir := flag.String("app", "", "应用安装目录")
	dataDir := flag.String("data", "", "数据目录")
	listen := flag.String("listen", "", "管理端口")
	applyWiz := flag.Bool("apply-wizard", false, "写入向导配置后退出")
	showVersion := flag.Bool("version", false, "打印版本")
	flag.Parse()
	if *showVersion {
		fmt.Printf("proxyhub %s\n", appVersion)
		return
	}
	base := *appDir
	if base == "" {
		exe, _ := os.Executable()
		base = filepath.Dir(filepath.Dir(exe))
	}
	data := *dataDir
	if data == "" {
		data = filepath.Join(base, "..", "var")
	}
	if *applyWiz {
		if err := applyWizard(data); err != nil {
			fmt.Fprintln(os.Stderr, "写入配置失败：", err)
			os.Exit(1)
		}
		return
	}
	app, err := NewApp(base, data, *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		os.Exit(1)
	}
	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "运行失败：", err)
	}
	_ = strings.TrimSpace("")
}

func applyWizard(dataDir string) error {
	store, err := NewStore(filepath.Join(dataDir, "settings.json"))
	if err != nil {
		return err
	}
	s := store.Get()
	if v := os.Getenv("wizard_subscription_url"); v != "" {
		s.SubscriptionURL = v
	}
	if v := os.Getenv("wizard_mixed_port"); v != "" {
		fmt.Sscanf(v, "%d", &s.MixedPort)
	}
	return store.Save(s)
}
