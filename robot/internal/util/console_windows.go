//go:build windows

// Package util 提供与平台相关的终端辅助能力。
package util

import "syscall"

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleCP = kernel32.NewProc("SetConsoleOutputCP")
)

// EnableUTF8Console 把 Windows 控制台**输出**代码页切到 UTF-8(65001)。
//
// 为什么需要：报告与进度条里有中文，而 Windows 控制台默认代码页（936/GBK）
// 会让中文变成乱码。此调用在非 Windows 上是空操作（见 console_other.go）。
// 失败不报错：拿不到控制台（重定向到文件 / CI）时本就无需切换。
func EnableUTF8Console() {
	_, _, _ = procSetConsoleCP.Call(65001)
}
