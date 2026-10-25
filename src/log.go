package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	file  *os.File
}

func NewLogBuffer(path string) *LogBuffer {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	lb := &LogBuffer{file: f}
	if b, err := os.ReadFile(path); err == nil {
		lb.lines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lb.lines) > 300 {
			lb.lines = lb.lines[len(lb.lines)-300:]
		}
	}
	return lb
}

func (l *LogBuffer) Add(msg string) {
	line := time.Now().Format("2006-01-02 15:04:05") + " " + msg
	l.mu.Lock()
	l.lines = append(l.lines, line)
	if len(l.lines) > 500 {
		l.lines = l.lines[len(l.lines)-500:]
	}
	if l.file != nil {
		fmt.Fprintln(l.file, line)
	}
	l.mu.Unlock()
}

func (l *LogBuffer) Tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.lines) {
		n = len(l.lines)
	}
	out := make([]string, n)
	copy(out, l.lines[len(l.lines)-n:])
	return out
}
