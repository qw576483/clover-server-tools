// Package util 提供与运行环境相关的小工具。
package util

import (
	"syscall"
	"unsafe"
)

// 把控制台代码页切到 UTF-8 (65001),
// 避免 Go 程序在 Windows 控制台输出中文时出现乱码。
func EnableUTF8Console() {
	k := syscall.NewLazyDLL("kernel32.dll")
	if p := k.NewProc("SetConsoleOutputCP"); p.Find() == nil {
		_, _, _ = p.Call(65001)
	}
	if p := k.NewProc("SetConsoleCP"); p.Find() == nil {
		_, _, _ = p.Call(65001)
	}
}

// 开启 Windows 控制台的虚拟终端处理,
// 使 ANSI 转义序列 (颜色) 生效 (Windows 10+ 默认支持)。
// 旧系统不支持时静默忽略, 文字仍正常显示 (仅无颜色)。
func EnableANSIColors() {
	const stdOutputHandle = ^uintptr(11) // -11
	const enableVT = 0x0004             // ENABLE_VIRTUAL_TERMINAL_PROCESSING

	k := syscall.NewLazyDLL("kernel32.dll")
	h, _, _ := k.NewProc("GetStdHandle").Call(stdOutputHandle)
	if h == 0 || h == uintptr(^uintptr(0)) { // INVALID_HANDLE_VALUE
		return
	}
	var mode uint32
	k.NewProc("GetConsoleMode").Call(h, uintptr(unsafe.Pointer(&mode)))
	mode |= enableVT
	k.NewProc("SetConsoleMode").Call(h, uintptr(mode))
}
