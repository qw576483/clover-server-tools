// Package logwriter 提供按天滚动的文件日志，同时保持控制台输出。
package logwriter

import (
	"fmt"
	"io"
	stdlog "log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var stdout = os.Stdout

// 按天滚动的日志写入器。
// 文件路径：{baseDir}/{YYYY-MM-DD}.log
type DailyWriter struct {
	baseDir string

	mu      sync.Mutex
	current string
	file    *os.File
}

// 创建按天滚动写入器。
func NewDailyWriter(baseDir string) *DailyWriter {
	return &DailyWriter{baseDir: baseDir}
}

func (w *DailyWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	day := time.Now().Format("2006-01-02")
	if w.current != day || w.file == nil {
		if err := w.rotate(day); err != nil {
			return 0, err
		}
	}
	return w.file.Write(p)
}

func (w *DailyWriter) rotate(day string) error {
	if w.file != nil {
		_ = w.file.Sync()
		_ = w.file.Close()
		w.file = nil
	}
	if err := os.MkdirAll(w.baseDir, 0755); err != nil {
		return fmt.Errorf("daily log: mkdir %s: %w", w.baseDir, err)
	}
	path := filepath.Join(w.baseDir, day+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("daily log: open %s: %w", path, err)
	}
	w.current = day
	w.file = f
	return nil
}

// 刷盘。
func (w *DailyWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Sync()
	}
	return nil
}

// 关闭当前文件句柄。
func (w *DailyWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		w.current = ""
		return err
	}
	return nil
}

// 写回原始控制台句柄，避免递归。
type consoleWriter struct{}

func (consoleWriter) Write(p []byte) (int, error) { return stdout.Write(p) }

// 初始化按天分割日志。
// 固定目录 "logs"，文件名为 {YYYY-MM-DD}.log；标准 log 包输出同时落盘。
func Init() (*DailyWriter, error) {
	dw := NewDailyWriter("logs")
	if err := dw.rotate(time.Now().Format("2006-01-02")); err != nil {
		return nil, err
	}

	mw := io.MultiWriter(&consoleWriter{}, dw)
	stdlog.SetOutput(mw)
	return dw, nil
}
