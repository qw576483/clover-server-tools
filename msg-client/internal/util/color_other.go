//go:build !windows

// Package util: 非 Windows 平台无需额外初始化 (控制台默认 UTF-8 + ANSI)。
package util

// 在非 Windows 平台是空操作: 类 Unix 终端默认就是 UTF-8。
func EnableUTF8Console() {}

// 在非 Windows 平台是空操作: 类 Unix 终端原生支持 ANSI。
func EnableANSIColors() {}
