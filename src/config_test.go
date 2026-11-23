package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewStoreCorruptSettingsIsBackedUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := st.Get()
	if got.MixedPort != 7890 || got.ControllerPort != 9091 || got.Secret == "" {
		t.Fatalf("corrupt settings should fall back to defaults, got %+v", got)
	}
	if st.Warning() == "" {
		t.Fatal("corrupt settings should produce a warning")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt settings backup missing: %v", err)
	}
	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Warning() != "" {
		t.Fatalf("recovered settings should be written back, warning: %s", reloaded.Warning())
	}
	if reloaded.Get().MixedPort != 7890 {
		t.Fatalf("recovered settings mismatch: %+v", reloaded.Get())
	}
}

func TestNewStoreValidSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"mixed_port":7899,"mode":"global","secret":"abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := st.Get()
	if got.MixedPort != 7899 || got.Mode != "global" || got.Secret != "abc" {
		t.Fatalf("valid settings not preserved: %+v", got)
	}
	if st.Warning() != "" {
		t.Fatalf("unexpected warning: %s", st.Warning())
	}
}

func TestNewStoreDefaultSecret(t *testing.T) {
	st, err := NewStore(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Get().Secret) != 32 {
		t.Fatalf("expected 32-char random secret, got %q", st.Get().Secret)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yaml")
	if err := writeFileAtomic(path, []byte("mode: rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(path); string(b) != "mode: rule\n" {
		t.Fatalf("content = %q", b)
	}
	if err := writeFileAtomic(path, []byte("mode: global\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "mode: global\n" {
		t.Fatalf("content after overwrite = %q", b)
	}
	tmp, _ := filepath.Glob(filepath.Join(dir, "nested", ".config.yaml.tmp-*"))
	if len(tmp) != 0 {
		t.Fatalf("temp files left behind: %v", tmp)
	}
}

func TestStoreUpdatePreservesUnrelatedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	hashed, err := hashPassword("secret123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(func(s *Settings) { s.AccessPassword = hashed }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(func(s *Settings) { s.Mode = "global" }); err != nil {
		t.Fatal(err)
	}
	got := st.Get()
	if got.AccessPassword != hashed || got.Mode != "global" {
		t.Fatalf("concurrent updates lost fields: %+v", got)
	}
	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Get().AccessPassword != hashed || reloaded.Get().Mode != "global" {
		t.Fatalf("persisted settings lost fields: %+v", reloaded.Get())
	}
}
