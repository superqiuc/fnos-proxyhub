package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	appLogMax  = 2 << 20
	coreLogMax = 8 << 20
)

// rotatingWriter 是带单份备份的按大小滚动写入器。
// 内核日志和 app.log 都通过它写入，避免长期运行后无限增长。
type rotatingWriter struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func newRotatingWriter(path string, max int64) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path, max: max}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f = f
	w.size = st.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.max > 0 && w.size+int64(len(p)) > w.max {
		// 轮转失败（例如改名被占用）时继续写当前文件，不能让内核日志把进程卡住。
		if err := w.rotateLocked(); err != nil && w.f == nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotateLocked() error {
	closeErr := w.f.Close()
	w.f = nil
	_ = os.Remove(w.path + ".1")
	renameErr := os.Rename(w.path, w.path+".1")
	if renameErr != nil && os.IsNotExist(renameErr) {
		renameErr = nil
	}
	// 即使改名失败也重新打开文件继续写，不能让日志把业务打断。
	if err := w.open(); err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return renameErr
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// tailLines 只读取文件尾部，避免为了展示最后几百行而把整个日志读进内存。
func tailLines(path string, maxBytes int64, maxLines int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return nil
	}
	size := st.Size()
	off := int64(0)
	if maxBytes > 0 && size > maxBytes {
		off = size - maxBytes
	}
	buf := make([]byte, size-off)
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil
	}
	buf = buf[:n]
	if off > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return nil
		}
		buf = buf[i+1:]
	}
	text := strings.TrimRight(string(buf), "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}

type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	file  *rotatingWriter
}

func NewLogBuffer(path string) *LogBuffer {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	w, _ := newRotatingWriter(path, appLogMax)
	lb := &LogBuffer{file: w}
	if lines := tailLines(path, 256<<10, 300); len(lines) > 0 {
		lb.lines = lines
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
		_, _ = l.file.Write([]byte(line + "\n"))
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
