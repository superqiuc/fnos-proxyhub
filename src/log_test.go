package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := tailLines(path, 1<<20, 2)
	if strings.Join(got, ",") != "b,c" {
		t.Fatalf("tailLines() = %v, want [b c]", got)
	}
	if lines := tailLines(filepath.Join(t.TempDir(), "missing"), 1024, 5); lines != nil {
		t.Fatalf("missing file should return nil, got %v", lines)
	}
}

func TestTailLinesPartialReadDropsFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.log")
	var b strings.Builder
	for i := 0; i < 1000; i++ {
		b.WriteString("0123456789abcdef\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := tailLines(path, 64, 3)
	if len(got) == 0 || len(got) > 3 {
		t.Fatalf("unexpected tail lines: %v", got)
	}
	for _, line := range got {
		if len(line) != 16 {
			t.Fatalf("partial line was not dropped: %q", line)
		}
	}
}

func TestRotatingWriterRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "core.log")
	w, err := newRotatingWriter(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("123456")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("78901")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != "78901" {
		t.Fatalf("current log = %q, want 78901", cur)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "123456" {
		t.Fatalf("rotated log = %q, want 123456", old)
	}
}

func TestLogBufferKeepsRing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	lb := NewLogBuffer(path)
	for i := 0; i < 510; i++ {
		lb.Add("line")
	}
	if got := lb.Tail(500); len(got) != 500 {
		t.Fatalf("Tail(500) length = %d, want 500", len(got))
	}
}
