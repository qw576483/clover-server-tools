package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/qw576483/clover-server-tools/robot/internal/load"
)

// renderReport 把压测报告渲染成人类可读文本。
//
// 与 --json 的关系：两者信息量**必须一致**。人类读的是这份，脚本读的是 JSON，
// 如果某个数字只出现在其中一边，就会出现"我看报告说没问题、脚本却判失败"。
func renderReport(rep *load.Report) string {
	var b strings.Builder

	b.WriteString("\n=== robot 压测报告 ===\n\n")

	// ---- 运行 ----
	r := rep.Run
	fmt.Fprintf(&b, "运行    %d 个机器人 · 传输 %s · ramp %s · 保持 %s\n",
		r.Robots, r.Transport, rampText(r.RampPerSec), r.Duration)
	fmt.Fprintf(&b, "        网关 %s（tcp %s）· 账号服 %s\n", r.Gateway, r.GatewayTCP, r.AuthAddr)

	// 启动速度太慢时点名：ramp 会吃掉保持窗口，不提示的话很容易被忽略。
	fmt.Fprintf(&b, "        实测耗时 %s", humanMS(r.WallMS))
	if r.Deadline != "" {
		fmt.Fprintf(&b, " · 截止 %s", r.Deadline)
		if slow := wallVsDeadline(r); slow != "" {
			fmt.Fprintf(&b, "  ⚠ %s", slow)
		}
	}
	b.WriteString("\n\n")

	// ---- 连接 ----
	fmt.Fprintf(&b, "连接    成功 %d / %d", rep.Connect.OK, rep.Connect.Attempted)
	if rep.Connect.Failed > 0 {
		fmt.Fprintf(&b, "（失败 %d）", rep.Connect.Failed)
	}
	b.WriteString("\n")
	writeDist(&b, "延迟(ms)", rep.Connect.LatencyMS)
	if len(rep.Connect.ByTransport) > 0 {
		var parts []string
		for _, k := range sortedKeys(rep.Connect.ByTransport) {
			parts = append(parts, fmt.Sprintf("%s %d", k, rep.Connect.ByTransport[k]))
		}
		fmt.Fprintf(&b, "        传输层    %s\n", strings.Join(parts, " · "))
	}
	b.WriteString("\n")

	// ---- 登录 ----
	fmt.Fprintf(&b, "登录    成功 %d / %d", rep.Login.OK, rep.Login.Attempted)
	if rep.Login.Failed > 0 {
		fmt.Fprintf(&b, "（失败 %d）", rep.Login.Failed)
	}
	if rep.Login.SignupTried > 0 {
		fmt.Fprintf(&b, " · 自动注册 %d", rep.Login.SignupTried)
	}
	b.WriteString("\n")
	writeDist(&b, "长连接(ms)", rep.Login.LatencyMS)
	writeDist(&b, "账号服(ms)", rep.Login.AuthHTTPMS)
	b.WriteString("\n")

	// ---- 会话 ----
	fmt.Fprintf(&b, "会话    全量同步 %d / %d · 断线 %d\n",
		rep.Session.FullSync, rep.Login.OK, rep.Session.Disconnects)
	writeDist(&b, "在线(ms)", rep.Session.OnlineMS)
	b.WriteString("\n")

	// ---- 流量 ----
	t := rep.Traffic
	fmt.Fprintf(&b, "流量    发送 %d · 接收 %d · 发送失败 %d\n", t.Sent, t.Recv, t.SendFailed)
	if t.Sent > 0 {
		fmt.Fprintf(&b, "        吞吐 %.0f msg/s\n", t.PerSec)
	}
	writeDist(&b, "RTT(ms)", t.RTTMS)

	// 这三个是「计量链路是否可信」的指标，正常应全为 0。
	// 分开说而不是合成一句：三者成因完全不同，混在一起会让读报告的人
	// 把「对端没回包」（预期行为）误当成「工具丢帧」（工具 bug）。
	if t.Dropped > 0 || t.Unmatched > 0 || t.PendingOvf > 0 {
		fmt.Fprintf(&b, "        ⚠ 丢帧 %d · 未配对回包 %d · 未回包被清 %d\n",
			t.Dropped, t.Unmatched, t.PendingOvf)
		if t.Dropped > 0 {
			b.WriteString("          丢帧 > 0 是本工具侧的问题（消费端跟不上），recv 与 RTT 会偏低。\n")
		}
		if t.Unmatched > 0 {
			b.WriteString("          未配对回包 > 0：收到配不上任何请求的回包（多为已经超时放弃的请求）。\n")
		}
		if t.PendingOvf > 0 {
			b.WriteString("          未回包被清 > 0：大量请求没等到回包。若发的消息号服务端没注册 handler\n")
			b.WriteString("          就本就不回包（引擎行为），属预期；否则说明回包路径有压力。\n")
		}
	}
	b.WriteString("\n")

	// ---- 失败归类 ----
	if len(rep.Errors) > 0 {
		b.WriteString("失败原因\n")
		for _, k := range sortedKeys(rep.Errors) {
			fmt.Fprintf(&b, "  %-56s %d\n", truncate(k, 56), rep.Errors[k])
		}
		b.WriteString("\n")
	}

	// ---- 失败明细 ----
	if len(rep.Failures) > 0 {
		fmt.Fprintf(&b, "失败明细（前 %d 条）\n", len(rep.Failures))
		for _, f := range rep.Failures {
			fmt.Fprintf(&b, "  #%-5d %-16s %-8s %s\n", f.Index, f.Account, f.Stage, truncate(f.Error, 70))
		}
		if rep.FailuresTruncated {
			b.WriteString("  …（更多明细已省略，看上面的失败归类即可）\n")
		}
		b.WriteString("\n")
	}

	// ---- 诊断提示 ----
	//
	// 只在有把握时说话：这些是压测里最容易把结论带偏的两种模式，
	// 报告里点一句能省掉一轮"翻服务端日志"的时间。
	if rep.Login.ServerClosed > 0 {
		fmt.Fprintf(&b, "诊断    有 %d 个机器人在登录阶段被服务端直接关闭连接。\n", rep.Login.ServerClosed)
		b.WriteString("        这类失败最常见的原因不是登录链路，而是网关的「连接级准入」：\n")
		b.WriteString("          · 每秒新建连接数超限（配置 gateway.max_conns_per_sec）\n")
		b.WriteString("          · 等候队列已满（gateway.queue_cap；未启用排队时超限直接拒）\n")
		b.WriteString("        服务端日志对应：gwcore \"admission rejected for <conn> (rate/queue)\"\n")
		b.WriteString("        验证方法：把 --ramp 逐步调小（如 --ramp 1）重跑，失败显著减少即确认为准入限流；\n")
		b.WriteString("        阈值边缘本身不稳定（同一 --ramp 复跑可能 3/3 也可能 1/3），不必要求降到 0。\n")
		b.WriteString("        这不是游戏服容量上限：准入卡的是「新建连接有多快」，\n")
		b.WriteString("        调大 gateway.max_conns_per_sec / 启用 queue_cap 排队后才能压出真实容量。\n\n")
	}

	if rep.OK {
		b.WriteString("结论    全部机器人连接 + 登录成功。\n")
	} else {
		b.WriteString("结论    有机器人失败，见上面的失败原因与明细。\n")
	}
	return b.String()
}

// writeDist 输出一行分布。
func writeDist(b *strings.Builder, label string, d load.DistSnap) {
	if d.Count == 0 {
		fmt.Fprintf(b, "        %-10s 无样本\n", label)
		return
	}
	fmt.Fprintf(b, "        %-10s P50 %d · P90 %d · P99 %d · 最大 %d\n",
		label, d.P50, d.P90, d.P99, d.Max)
}

// wallVsDeadline 在实测耗时明显超出计划窗口时给一句解释。
//
// 为什么要点名：截止时刻是「启动耗时 + 保持时长」算出来的，若实际用时远超它，
// 说明有机器人卡在登录超时上（连不上 / 账号不对），而那些机器人在线时长为 0 ——
// 不提示的话很容易被读成"服务端很慢"。
func wallVsDeadline(r load.RunMeta) string {
	if r.Deadline == "" {
		return ""
	}
	dl, err := time.Parse(time.RFC3339Nano, r.Deadline)
	if err != nil {
		return ""
	}
	st, err := time.Parse(time.RFC3339Nano, r.StartedAt)
	if err != nil {
		return ""
	}
	planned := dl.Sub(st).Milliseconds()
	if planned <= 0 || r.WallMS <= planned*2 {
		return ""
	}
	return fmt.Sprintf("实测耗时约为计划窗口(%s)的 2 倍以上，可能有机器人卡在登录超时", humanMS(planned))
}

// humanMS 把毫秒转成好读的时长。
func humanMS(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d >= time.Minute {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// truncate 按字符截断（避免中文被按字节切断成半个字）。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
