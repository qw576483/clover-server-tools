// Package util 提供与运行环境相关的小工具。
package util

// 命令行配色: 采用成熟的语义化方案 (Amber 橘黄复古终端 + 现代 CLI 惯例)
//   - 标题/分隔/提示符: 橘黄 (amber, #FFB000)
//   - 成功/就绪/运行中:  绿 (green)
//   - 错误/失败/缺失:    红 (red)
//   - 警告/跳过/提示:    黄 (yellow)
//   - 标签/信息 (端口/PID/组件): 青 (cyan)
//   - 次要/停用:         暗灰 (dim)
const (
	cReset  = "\033[0m"
	cAmber  = "\033[38;5;214m" // 橘黄
	cGreen  = "\033[32m"
	cRed    = "\033[31m"
	cYellow = "\033[33m"
	cCyan   = "\033[36m"
	cDim    = "\033[2m"
	cBold   = "\033[1m"
)

func colorize(c, s string) string { return c + s + cReset }

// 橘黄: 标题 / 分隔线 / 提示符。
func Amber(s string) string { return colorize(cAmber, s) }

// 绿: 成功 / 就绪 / 运行中 / 启动 / 停止完成。
func Green(s string) string { return colorize(cGreen, s) }

// 红: 错误 / 失败 / 缺失 / 未就绪。
func Red(s string) string { return colorize(cRed, s) }

// 黄: 警告 / 跳过 / 提示。
func Yellow(s string) string { return colorize(cYellow, s) }

// 青: 标签 / 信息 (端口 / PID / 组件名)。
func Cyan(s string) string { return colorize(cCyan, s) }

// 暗灰: 次要信息 / 停用状态。
func Dim(s string) string { return colorize(cDim, s) }

// 加粗。
func Bold(s string) string { return colorize(cBold, s) }
