package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// logBuf 是内存环形日志：所有 goroutine 都能写，界面按需读取。
// 每次写入后调用 notify（去抖在调用方），用于刷新窗口与托盘。
type logBuf struct {
	mu     sync.Mutex
	lines  []string
	max    int
	notify func()
}

func newLogBuf(max int, notify func()) *logBuf {
	return &logBuf{max: max, notify: notify}
}

// Printf 追加一行（带时间戳）。
func (l *logBuf) Printf(format string, args ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.lines = append(l.lines, line)
	if len(l.lines) > l.max {
		l.lines = l.lines[len(l.lines)-l.max:]
	}
	l.mu.Unlock()
	if l.notify != nil {
		l.notify()
	}
}

// Lines 返回最近的日志副本。
func (l *logBuf) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// Tail 返回最后 n 行。
func (l *logBuf) Tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n >= len(l.lines) {
		return append([]string(nil), l.lines...)
	}
	return append([]string(nil), l.lines[len(l.lines)-n:]...)
}

// Text 返回全文（复制用）。
func (l *logBuf) Text() string {
	return strings.Join(l.Lines(), "\n")
}

// Clear 清空。
func (l *logBuf) Clear() {
	l.mu.Lock()
	l.lines = nil
	l.mu.Unlock()
	if l.notify != nil {
		l.notify()
	}
}
