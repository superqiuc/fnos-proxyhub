package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Settings struct {
	MixedPort        int    `json:"mixed_port"`
	ControllerPort   int    `json:"controller_port"`
	AllowLAN         bool   `json:"allow_lan"`
	IPv6             bool   `json:"ipv6"`
	TCPConcurrent    bool   `json:"tcp_concurrent"`
	AutoTune         bool   `json:"auto_tune"`
	PACMode          string `json:"pac_mode"`
	AutoTuneInterval int    `json:"auto_tune_interval"`
	Mode             string `json:"mode"`
	LogLevel         string `json:"log_level"`
	SubscriptionURL  string `json:"subscription_url"`
	UpdateHours      int    `json:"update_hours"`
	AutoUpdate       bool   `json:"auto_update"`
	Secret           string `json:"secret"`
	PACHost          string `json:"pac_host,omitempty"`
	AccessPassword   string `json:"access_password,omitempty"` // 加盐 SHA-256；空表示不启用登录
}

func defaultSettings() Settings {
	return Settings{
		MixedPort: 7890, ControllerPort: 9091,
		AllowLAN: true, Mode: "rule", LogLevel: "info",
		UpdateHours: 12, AutoUpdate: true,
	}
}

func randomSecret() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type Store struct {
	mu      sync.RWMutex
	writeMu sync.Mutex
	path    string
	data    Settings
	warn    string
}

func NewStore(path string) (*Store, error) {
	st := &Store{path: path}
	recovered := false
	b, err := os.ReadFile(path)
	if err == nil {
		if e := json.Unmarshal(b, &st.data); e != nil {
			// 保留损坏文件供排查，不能让用户设置悄悄消失。
			bad := path + ".corrupt"
			if we := os.WriteFile(bad, b, 0o600); we == nil {
				st.warn = fmt.Sprintf("设置文件解析失败（%v），已备份为 %s 并恢复默认设置", e, filepath.Base(bad))
			} else {
				st.warn = fmt.Sprintf("设置文件解析失败（%v），且备份失败（%v），已恢复默认设置", e, we)
			}
			st.data = defaultSettings()
			st.data.Secret = randomSecret()
			recovered = true
		}
	} else if errors.Is(err, os.ErrNotExist) {
		st.data = defaultSettings()
		st.data.Secret = randomSecret()
	} else {
		return nil, err
	}
	// 1.1.0 的默认端口是 7891；旧 mihomo 卸载后统一迁回 7890
	if st.data.MixedPort == 7891 {
		st.data.MixedPort = 7890
	}
	st.normalize()
	if recovered {
		b, _ := json.MarshalIndent(st.data, "", "  ")
		if err := writeFileAtomic(path, b, 0o600); err != nil {
			st.warn += fmt.Sprintf("；恢复默认设置后写盘失败：%v", err)
		}
	}
	return st, nil
}

// Warning 返回启动时可记录的设置恢复提示。
func (s *Store) Warning() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.warn
}

func (s *Store) normalize() {
	d := defaultSettings()
	if s.data.MixedPort <= 0 || s.data.MixedPort > 65535 {
		s.data.MixedPort = d.MixedPort
	}
	if s.data.ControllerPort <= 0 || s.data.ControllerPort > 65535 {
		s.data.ControllerPort = d.ControllerPort
	}
	s.data.PACHost = strings.TrimSpace(s.data.PACHost)
	if s.data.PACMode != "global" {
		s.data.PACMode = "gfwlist"
	}
	if s.data.AutoTuneInterval < 30 {
		s.data.AutoTuneInterval = 30
	}
	if s.data.AutoTuneInterval > 86400 {
		s.data.AutoTuneInterval = 86400
	}
	switch s.data.Mode {
	case "rule", "global", "direct":
	default:
		s.data.Mode = "rule"
	}
	switch s.data.LogLevel {
	case "debug", "info", "warning", "error", "silent":
	default:
		s.data.LogLevel = "info"
	}
	if s.data.UpdateHours <= 0 || s.data.UpdateHours > 168 {
		s.data.UpdateHours = d.UpdateHours
	}
	if s.data.Secret == "" {
		s.data.Secret = randomSecret()
	}
	s.data.SubscriptionURL = strings.TrimSpace(s.data.SubscriptionURL)
}

func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

func (s *Store) Save(in Settings) error {
	_, err := s.Update(func(st *Settings) { *st = in })
	return err
}

// Update 在写锁内完成“读当前值 -> 修改 -> 落盘”，
// 避免两个并发保存互相覆盖（例如改设置的同时设置密码）。
func (s *Store) Update(fn func(*Settings)) (Settings, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	data := s.data
	fn(&data)
	s.data = data
	s.normalize()
	out := s.data
	s.mu.Unlock()
	b, _ := json.MarshalIndent(out, "", "  ")
	if err := writeFileAtomic(s.path, b, 0o600); err != nil {
		return out, err
	}
	return out, nil
}

// writeFileAtomic 先写同目录临时文件再 rename，避免内核启动时读到半截配置。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
