// Package util 提供与运行环境相关的小工具 (含命令行配色)。
package util

import "strings"

// 命令行配色: 采用成熟的语义化方案 (Amber 橘黄复古终端 + 现代 CLI 惯例)
//   - 标题/分隔/提示符: 橘黄 (amber, #FFB000)
//   - 成功/就绪/已连接/已发送: 绿 (green)
//   - 错误/失败/断开: 红 (red)
//   - 警告/提示/未加载: 黄 (yellow)
//   - 标签/信息 (地址/消息名/计数): 青 (cyan)
//   - 次要信息: 暗灰 (dim)
const (
	cReset  = "\033[0m"
	cAmber  = "\033[38;5;214m" // 橘黄
	cGreen  = "\033[32m"
	cRed    = "\033[31m"
	cYellow = "\033[33m"
	cCyan    = "\033[36m"
	cMagenta = "\033[94m" // 亮蓝（业务消息）
	cDim     = "\033[2m"
	cBold    = "\033[1m"
)

func colorize(c, s string) string { return c + s + cReset }

// 橘黄: 标题 / 分隔线 / 提示符。
func Amber(s string) string { return colorize(cAmber, s) }

// 绿: 成功 / 就绪 / 已连接 / 已发送。
func Green(s string) string { return colorize(cGreen, s) }

// 红: 错误 / 失败 / 断开。
func Red(s string) string { return colorize(cRed, s) }

// 黄: 警告 / 提示 / 未加载。
func Yellow(s string) string { return colorize(cYellow, s) }

// 青: 标签 / 信息 (地址 / 引擎消息)。
func Cyan(s string) string { return colorize(cCyan, s) }

// 亮蓝: 业务消息标识。
func Magenta(s string) string { return colorize(cMagenta, s) }

// 暗灰: 次要信息。
func Dim(s string) string { return colorize(cDim, s) }

// 加粗。
func Bold(s string) string { return colorize(cBold, s) }

// 去除字符串中的 ANSI 转义序列。
// 用于日志/统计等需要纯文本的场景。
func StripANSI(s string) string {
	// 仅处理颜色/SGR 类 (\x1b[...m), 简单场景足够。
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == 0x1b && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
