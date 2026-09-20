//go:build !windows

package main

// enableUTF8Console 在非 Windows 平台是空操作（类 Unix 终端默认就是 UTF-8）。
func enableUTF8Console() {}

// isOwnConsole 在非 Windows 平台恒为 false：类 Unix 的终端由用户的 shell 持有，
// 程序退出后窗口不会消失，不存在「双击后闪退」的问题。
func isOwnConsole() bool { return false }
