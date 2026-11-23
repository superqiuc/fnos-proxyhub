package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newCoreUpdateTestApp(t *testing.T) *App {
	t.Helper()
	appDir := t.TempDir()
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(appDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &App{dataDir: dataDir, core: NewCore(appDir, dataDir, nil, nil)}
}

func TestBackupInstallRestoreCore(t *testing.T) {
	app := newCoreUpdateTestApp(t)
	old := []byte("old-core-binary")
	if err := os.WriteFile(app.core.corePath(), old, 0o755); err != nil {
		t.Fatal(err)
	}
	backup, err := app.backupCore("v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.Base(backup), "v1.2.3") {
		t.Fatalf("backup name should include version: %s", backup)
	}
	if b, err := os.ReadFile(backup); err != nil || string(b) != string(old) {
		t.Fatalf("backup content mismatch: %q, %v", b, err)
	}

	src := filepath.Join(app.dataDir, "mihomo.new")
	if err := os.WriteFile(src, []byte("new-core-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := app.installCore(src); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(app.core.corePath()); string(b) != "new-core-binary" {
		t.Fatalf("core was not replaced: %q", b)
	}
	if _, err := os.Stat(app.core.corePath() + ".new"); !os.IsNotExist(err) {
		t.Fatal("staged file should be renamed away")
	}

	if err := app.restoreCore(backup); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(app.core.corePath()); string(b) != string(old) {
		t.Fatalf("core was not restored: %q", b)
	}
}

func TestInstallCoreFailureKeepsOldBinary(t *testing.T) {
	app := newCoreUpdateTestApp(t)
	old := []byte("old-core-binary")
	if err := os.WriteFile(app.core.corePath(), old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := app.installCore(filepath.Join(app.dataDir, "missing")); err == nil {
		t.Fatal("expected install error for missing source")
	}
	if b, _ := os.ReadFile(app.core.corePath()); string(b) != string(old) {
		t.Fatalf("old core should be untouched, got %q", b)
	}
}

func TestBackupCoreMissingBinary(t *testing.T) {
	app := newCoreUpdateTestApp(t)
	if _, err := app.backupCore("v1"); err == nil {
		t.Fatal("expected backup error when core is missing")
	}
}

func TestVerifyCoreELF(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("\x7fELFrest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyCoreELF(good); err != nil {
		t.Fatalf("valid ELF rejected: %v", err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("not-elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyCoreELF(bad); err == nil {
		t.Fatal("non-ELF should be rejected")
	}
}

func TestRecoverInterruptedUpdate(t *testing.T) {
	app := newCoreUpdateTestApp(t)
	old := []byte("old-core")
	if err := os.WriteFile(app.core.corePath(), old, 0o755); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(app.dataDir, "backups", "mihomo-old")
	if err := writeFileAtomic(backup, old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app.core.corePath(), []byte("new-core"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := app.beginCoreUpdate(backup); err != nil {
		t.Fatal(err)
	}
	app.recoverInterruptedUpdate()
	if b, _ := os.ReadFile(app.core.corePath()); string(b) != string(old) {
		t.Fatalf("core was not rolled back: %q", b)
	}
	if _, err := os.Stat(app.updateMarkerPath()); !os.IsNotExist(err) {
		t.Fatal("update marker should be removed after recovery")
	}
}

func TestPruneBackups(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := 0; i < 7; i++ {
		path := filepath.Join(dir, fmt.Sprintf("mihomo-v%d", i))
		if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	app := &App{}
	app.pruneBackups(dir, 5)
	for i := 0; i < 7; i++ {
		_, err := os.Stat(filepath.Join(dir, fmt.Sprintf("mihomo-v%d", i)))
		if i < 2 && !os.IsNotExist(err) {
			t.Fatalf("old backup v%d should be pruned", i)
		}
		if i >= 2 && err != nil {
			t.Fatalf("new backup v%d should be kept: %v", i, err)
		}
	}
}
