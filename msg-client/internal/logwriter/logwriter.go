// Package logwriter 提供按天滚动的文件日志，同时保持控制台输出。
package logwriter

import (
	"fmt"
	"io"
	stdlog "log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// 匹配 ANSI 转义序列（颜色、光标控制等）。
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(p []byte) []byte {
	return ansiRe.ReplaceAll(p, nil)
}

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
	// 文件日志去除 ANSI 颜色码，保持纯文本。
	return w.file.Write(stripANSI(p))
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

var dwGlobal *DailyWriter

// 初始化按天分割日志。
// 固定目录 "logs"，文件名为 {YYYY-MM-DD}.log；标准 log 包输出会同时落盘。
// 注意：CLI 是交互式程序，os.Stdout/os.Stderr 保持原控制台，避免破坏输入体验。
func Init() (*DailyWriter, error) {
	dw := NewDailyWriter("logs")
	if err := dw.rotate(time.Now().Format("2006-01-02")); err != nil {
		return nil, err
	}
	dwGlobal = dw

	// 标准 log 包同时落盘；fmt.Println 保持只输出控制台。
	stdlog.SetOutput(io.MultiWriter(os.Stderr, dw))

	return dw, nil
}

// 同时输出到控制台和日志文件（自动去除 ANSI 颜色码）。
func Print(a ...any) {
	fmt.Print(a...)
	if dwGlobal != nil {
		_, _ = dwGlobal.Write([]byte(fmt.Sprint(a...)))
	}
}

// 同时输出到控制台和日志文件（自动去除 ANSI 颜色码）。
func Println(a ...any) {
	fmt.Println(a...)
	if dwGlobal != nil {
		_, _ = dwGlobal.Write([]byte(fmt.Sprintln(a...)))
	}
}

// 同时输出到控制台和日志文件（自动去除 ANSI 颜色码）。
func Printf(format string, a ...any) {
	fmt.Printf(format, a...)
	if dwGlobal != nil {
		_, _ = dwGlobal.Write([]byte(fmt.Sprintf(format, a...)))
	}
}
