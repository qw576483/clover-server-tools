//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// enableUTF8Console 把 Windows 控制台代码页切到 UTF-8（65001）。
//
// 必要性：本工具的启动诊断、证书告警、协议不匹配提示都是中文，而 Windows 控制台默认
// 代码页是 936（GBK），Go 程序输出的 UTF-8 中文会显示成乱码——「看不懂的报错」和
// 「闪退」叠加，等于完全没有可排查信息。msg-client 有同名能力（internal/util），
// 但两者是独立 module，故此处自带一份最小实现。
func enableUTF8Console() {
	k := syscall.NewLazyDLL("kernel32.dll")
	if p := k.NewProc("SetConsoleOutputCP"); p.Find() == nil {
		_, _, _ = p.Call(65001)
	}
	if p := k.NewProc("SetConsoleCP"); p.Find() == nil {
		_, _, _ = p.Call(65001)
	}
}

// isOwnConsole 判断当前控制台是否为「本进程独享」（双击 exe / Start-Process 新开窗口时的会话）。
//
// 依据 GetConsoleProcessList：它返回附着在同一控制台上的进程列表。双击启动时控制台是为本进程
// 临时新建的，列表里只有自己（返回 1）；而从 cmd / PowerShell / Windows Terminal 启动时，
// 控制台里还附着着 shell（返回 >= 2）。只有独享控制台才需要「退出前停一下」——否则窗口本就不会关。
// 无控制台（重定向 / 服务方式启动）时返回 0，同样不需要停。
func isOwnConsole() bool {
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	if proc.Find() != nil {
		return false
	}
	var buf [64]uint32
	n, _, _ := proc.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return n == 1
}
