//go:build !windows

// Package util 提供与平台相关的终端辅助能力。
package util

// EnableUTF8Console 非 Windows 平台无需切换代码页，空操作。
func EnableUTF8Console() {}
