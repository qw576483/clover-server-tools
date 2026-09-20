// Package cli 实现 robot 的命令行：批量压测 / 批量注册 / 连通性自检。
//
// 定位：msg-client / msg-web 是「单连接的交互式调试器」，robot 是它们的
// 「无人值守、N 连接的自动化对照」。同一个登录链路
// （账号服 HTTP 换 token → 长连接 EMsgLogin{token} → EPushPlayerFullSync），
// 只是把单个变成一批，并给出可判定的量化报告。
//
// # 面向 AI / 脚本的自动化契约（改这个包前先读）
//
// 与 manager 保持同一套契约，理由也一样：压测最需要的是"能被脚本反复跑、
// 结果能被机器判定"，而不是好看。
//
//  1. **机器可读**：`--json` 把整个报告以**单个 JSON 对象**写到 stdout。
//     人类可读的表格只在非 `--json` 时出现。
//  2. **错误也结构化**：`--json` 下失败同样输出 JSON（`{"ok":false,"error":"..."}`）。
//  3. **绝不阻塞等输入**：stdin 不是终端时，会往服务端写数据的操作
//     （`signup` / `run --signup`）必须显式带 `--yes`，否则立即 ExitUsage(2)。
//  4. **退出码可判定**：见下方 Exit* 常量。
//  5. **流分离**：报告走 stdout，进度与诊断走 stderr。压测的进度条绝不能
//     污染 stdout —— 否则 `robot run --json | jq` 会直接解析失败。
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/qw576483/clover-server-tools/robot/internal/config"
)

// 退出码：脚本与 AI 靠它判断成败，不解析文案。
const (
	// ExitOK 全部机器人连接 + 登录成功。
	ExitOK = 0
	// ExitError 运行失败：配置错误、或**全军覆没**（一个都没成功）。
	//
	// 注意「全军覆没」判 1 而不是 3：那说明环境根本没搭好（网关没起 / 账号
	// 全错），报成"部分成功"会误导调用方以为"再多跑几次就好了"。
	ExitError = 1
	// ExitUsage 用法错误：参数缺失、非法组合，或非交互环境下未带 --yes。
	ExitUsage = 2
	// ExitPartial 部分成功：有机器人失败但并非全部 —— 这才是压测里最常见的
	// 需要人去看一眼的结果（容量到顶 / 个别账号有问题）。
	ExitPartial = 3
)

// usageText 顶层帮助。
const usageText = `robot —— clover 机器人 / 自动化压测客户端

用法:
  robot [-config <path>] [--json] <命令> [参数...]

命令:
  run       批量机器人：并发登录 + 可选持续压测，输出量化报告
  signup    批量注册机器人账号（压测前铺数据）
  doctor    连通性自检：网关 QUIC/TCP + 账号服

通用参数:
  -config <path>   配置文件路径（默认 ./config.yaml，缺失自动生成）
  --json           机器可读输出（单个 JSON 对象写到 stdout）

退出码:
  0 全部成功   1 运行失败/全军覆没   2 用法错误   3 部分成功（有机器人失败）

示例:
  # 连通性自检
  robot doctor

  # 先铺 100 个账号，再让 100 个机器人并发登录（全并发，不做 ramp）
  robot signup --start 1 --count 100 --yes
  robot run --robots 100

  # 100 个机器人，每秒起 20 个，登录后保持 60 秒、每秒发一条业务消息
  robot run --robots 100 --ramp 20 --duration 60s --msg 10001 --body '{"n":"r{i}"}'

  # 只看登录链路、登录成功即退，结果给脚本判定
  robot run --robots 50 --json
  if ($LASTEXITCODE -ne 0) { "有机器人失败" }

提示: 网关默认有「消息级限流」（单连接 64 帧/秒）与「连接级排队」
      （max_conns / queue_cap）。机器人打满时看到的失败多半是这两处在起作用，
      不是登录链路的问题 —— 报告里的 errors 归类能帮你区分。
`

// env 一次调用的运行上下文。
type env struct {
	cfg     *config.Config
	jsonOut bool
	ctx     context.Context
}

// Run 解析并执行命令行，返回进程退出码。
func Run(args []string) int {
	cfgPath, jsonOut, rest := extractGlobalFlags(args)

	if len(rest) == 0 {
		// --json 下必须也是结构化输出：调用方可能是"拼参数"的脚本，
		// 它只会去解析 JSON，给一段纯文本 usage 会让它直接解析失败。
		if jsonOut {
			return usageErrJSON("缺少子命令（可用：run / signup / doctor）")
		}
		fmt.Print(usageText)
		return ExitUsage
	}
	cmd, cmdArgs := rest[0], rest[1:]

	switch cmd {
	case "help", "-h", "--help", "?":
		fmt.Print(usageText)
		return ExitOK
	case "version", "-v", "--version":
		fmt.Println("robot (clover-server-tools)")
		return ExitOK
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		// 写不出默认配置不阻断本次运行，只提示。
		fmt.Fprintf(os.Stderr, "警告: %v\n", err)
	}
	if cfg == nil {
		cfg = config.Default()
	}

	// Ctrl+C → 取消 ctx → 各机器人收尾退出 → Run 返回并把已有结果汇总成报告。
	// 这样"压测中途打断"仍然能拿到一份部分报告，而不是什么都没有。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e := &env{cfg: cfg, jsonOut: jsonOut, ctx: ctx}

	switch cmd {
	case "run":
		return e.cmdRun(cmdArgs)
	case "signup":
		return e.cmdSignup(cmdArgs)
	case "doctor":
		return e.cmdDoctor(cmdArgs)
	default:
		if e.jsonOut {
			return e.usageErr(fmt.Sprintf("未知命令 %q（可用：run / signup / doctor）", cmd))
		}
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", cmd)
		fmt.Print(usageText)
		return ExitUsage
	}
}

// usageErrJSON 在 env 尚未构造时（命令行解析阶段）输出结构化的用法错误。
// 单独抽出来是因为 "缺少子命令" 这条分支发生在构造 env 之前，
// 用不了 env.usageErr —— 但契约要求它同样得是 JSON。
func usageErrJSON(msg string) int {
	printJSON(map[string]any{"ok": false, "error": msg, "usage": true})
	return ExitUsage
}

// extractGlobalFlags 摘出全局参数（-config / --json），返回其余参数。
func extractGlobalFlags(args []string) (cfgPath string, jsonOut bool, rest []string) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-config" || a == "--config" || a == "-c":
			if i+1 < len(args) {
				cfgPath = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-config="):
			cfgPath = strings.TrimPrefix(a, "-config=")
		case strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(a, "--config=")
		case a == "--json" || a == "-json":
			jsonOut = true
		default:
			out = append(out, a)
		}
	}
	return cfgPath, jsonOut, out
}

// emit 输出结果：--json 写 JSON 到 stdout；否则写人类可读文案。
func (e *env) emit(v any, human string) {
	if e.jsonOut {
		printJSON(v)
		return
	}
	if human != "" {
		fmt.Print(human)
	}
}

// fail 报告运行失败：--json 下输出结构化错误，统一返回 ExitError。
func (e *env) fail(err error) int {
	if e.jsonOut {
		printJSON(map[string]any{"ok": false, "error": err.Error()})
	} else {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
	}
	return ExitError
}

// usageErr 报告用法错误。
func (e *env) usageErr(msg string) int {
	if e.jsonOut {
		printJSON(map[string]any{"ok": false, "error": msg, "usage": true})
	} else {
		fmt.Fprintln(os.Stderr, "用法错误: "+msg)
	}
	return ExitUsage
}

// printJSON 以缩进 JSON 输出到 stdout（结果通道）。
func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "输出 JSON 失败: %v\n", err)
	}
}

// isInteractive 判断 stdin 是否连在终端上。
func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// askConfirm 交互式确认；非交互环境且未带 --yes 时直接以 ExitUsage 拒绝。
//
// 与 manager 同一条理由：自动化场景下"挂在提示符上直到超时"比"明确拒绝执行"
// 危险得多。返回 (是否继续, 退出码)，只有码为 ExitOK 时才继续。
func (e *env) askConfirm(prompt string, assumeYes bool) (bool, int) {
	if assumeYes {
		return true, ExitOK
	}
	if !isInteractive() {
		return false, e.usageErr("非交互环境：会往服务端写数据的操作需显式加 --yes")
	}
	fmt.Printf("%s [y/N] ", prompt)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false, ExitOK
	}
	s := strings.ToLower(strings.TrimSpace(sc.Text()))
	if s == "y" || s == "yes" {
		return true, ExitOK
	}
	if e.jsonOut {
		printJSON(map[string]any{"ok": false, "cancelled": true})
	} else {
		fmt.Println("已取消。")
	}
	return false, ExitOK
}

// newFlagSet 构造一个静默的子命令 FlagSet（错误信息由我们自己渲染成结构化输出）。
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// stringList 支持重复出现的字符串参数（如 --setup 可以写多次）。
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
