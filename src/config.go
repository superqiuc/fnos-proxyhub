package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Settings struct {
	Listen          string `json:"listen"`
	MixedPort       int    `json:"mixed_port"`
	ControllerPort  int    `json:"controller_port"`
	AllowLAN        bool   `json:"allow_lan"`
	Mode            string `json:"mode"`
	LogLevel        string `json:"log_level"`
	SubscriptionURL string `json:"subscription_url"`
	UpdateHours     int    `json:"update_hours"`
	AutoUpdate      bool   `json:"auto_update"`
	Secret          string `json:"secret"`
	PACHost         string `json:"pac_host,omitempty"`
}

func defaultSettings() Settings {
	return Settings{
		Listen: "8898", MixedPort: 7890, ControllerPort: 9091,
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
	mu   sync.RWMutex
	path string
	data Settings
}

func NewStore(path string) (*Store, error) {
	st := &Store{path: path}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &st.data)
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
	return st, nil
}

func (s *Store) normalize() {
	d := defaultSettings()
	if s.data.Listen == "" {
		s.data.Listen = d.Listen
	}
	if s.data.MixedPort <= 0 || s.data.MixedPort > 65535 {
		s.data.MixedPort = d.MixedPort
	}
	if s.data.ControllerPort <= 0 || s.data.ControllerPort > 65535 {
		s.data.ControllerPort = d.ControllerPort
	}
	s.data.PACHost = strings.TrimSpace(s.data.PACHost)
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
	s.mu.Lock()
	s.data = in
	s.normalize()
	data := s.data
	s.mu.Unlock()
	b, _ := json.MarshalIndent(data, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
