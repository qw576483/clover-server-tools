// Package cli 实现 manager 的命令行：看节点 / 看状态 / 操作节点 / 滚动发布编排。
//
// 定位：引擎的**控制台**，不是进程管理器。它不负责「起新进程」——那是 k8s /
// systemd 的活；rollout 通过 --launch 把这一步转交出去，自己只做引擎懂的那一段
// （连接级 drain：迁移 / 强踢 / 切上游 / 自退）。
//
// # 面向 AI / 脚本的自动化契约（改这个包前先读）
//
// AI 要能「无人值守地验证集群与发布流程」，因此本工具保证：
//
//  1. **机器可读**：所有子命令支持 `--json`，把**结果**以单个 JSON 对象写到 stdout。
//     人类可读的表格 / 文案只在非 `--json` 时出现。
//  2. **错误也结构化**：`--json` 下失败同样输出 JSON（`{"ok":false,"error":"..."}`），
//     不会把解析方逼到 stderr 上抓字符串。
//  3. **绝不阻塞等输入**：stdin 不是终端（AI / CI）时，破坏性命令必须显式 `--yes`，
//     否则立即以 ExitUsage 失败 —— "挂着等确认"比"拒绝执行"危险得多（会拖到超时）。
//  4. **退出码可判定**：固定语义见下方 Exit* 常量，脚本不必解析文案即可分支。
//  5. **流分离**：结果走 stdout，诊断 / 进度走 stderr（人类模式下才写 stdout）。
//  6. `doctor` 是「一条命令看清集群」的入口，适合开工自检与收尾验证。
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"manager/internal/config"
	"manager/internal/registry"
)

// 退出码：脚本与 AI 靠它判断成败，不解析文案。
const (
	// ExitOK 成功。
	ExitOK = 0
	// ExitError 运行失败：连不上 etcd、admin 端点报错、操作被拒。
	ExitError = 1
	// ExitUsage 用法错误：参数缺失，或非交互环境下破坏性操作未带 --yes。
	ExitUsage = 2
	// ExitPartial 部分成功：集群存在不可达节点（doctor / status --all），
	// 或 rollout 在处理某个节点时失败。
	ExitPartial = 3
)

// usageText 顶层帮助。
const usageText = `manager —— clover 集群进程编排工具

用法:
  manager [-config <path>] [--json] <命令> [参数...]

查看:
  nodes                               列出全部节点（节点目录 + 服务实例）
  status <node>|--all                 查看节点状态（存活 / 路由数 / drain 进度）
  doctor [--tag t]                    集群体检：一条命令输出汇总（AI 自检入口）
  drain-status <node>                 查询灰度下线进度

操作:
  drain <node> [参数]                 对节点发起灰度下线
  drain-cancel <node>                 取消灰度下线（回滚）
  wait <node> [--timeout 30m]         阻塞等待 drain 收敛（脚本 / AI 编排用）
  upstream <node> [--set <addr>]      查看 / 切换网关默认上游
  shutdown <node>                     请求节点优雅退出

编排:
  rollout [参数]                      滚动发布：对新进程就绪的旧节点逐个 drain

通用参数:
  -config <path>       配置文件路径（默认 ./config.yaml，缺失自动生成）
  --json               机器可读输出（所有子命令均支持）
  --addr <host:port>   直接指定 admin 地址，跳过节点目录解析
  --yes                跳过确认（非交互环境必填）

退出码:
  0 成功   1 运行失败   2 用法错误   3 部分成功（有节点不可达 / 编排中途失败）

示例:
  manager doctor --json
  manager nodes --json
  manager drain 127.0.0.1:8011 --target 127.0.0.1:8012 --stop-after --yes --wait --json
  manager rollout --nodes 127.0.0.1:8011 --target 127.0.0.1:8012 \
      --launch "systemctl start clover-game-new" --stop-after --yes --json

提示: 节点能否被远程管理，取决于它的 admin.listen_addr 是否可达（默认只绑回环）。
`

// env 一次调用的运行上下文；registry 懒连接（只给 --addr 的命令不需要 etcd）。
type env struct {
	cfg      *config.Config
	ctx      context.Context
	jsonOut  bool
	registry *registry.Registry
}

// Run 解析并执行命令行，返回进程退出码。
func Run(args []string) int {
	cfgPath, jsonOut, rest := extractGlobalFlags(args)

	if len(rest) == 0 {
		fmt.Print(usageText)
		return ExitUsage
	}
	cmd, cmdArgs := rest[0], rest[1:]

	switch cmd {
	case "help", "-h", "--help", "?":
		fmt.Print(usageText)
		return ExitOK
	case "version", "-v", "--version":
		fmt.Println("manager (clover-server-tools)")
		return ExitOK
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		// 写不出默认配置不阻断本次运行，只提示。
		fmt.Fprintf(os.Stderr, "警告: %v\n", err)
	}
	e := &env{cfg: cfg, ctx: context.Background(), jsonOut: jsonOut}
	defer e.close()

	switch cmd {
	case "nodes":
		return e.cmdNodes(cmdArgs)
	case "status":
		return e.cmdStatus(cmdArgs)
	case "doctor":
		return e.cmdDoctor(cmdArgs)
	case "drain":
		return e.cmdDrain(cmdArgs)
	case "drain-status":
		return e.cmdDrainStatus(cmdArgs)
	case "drain-cancel":
		return e.cmdDrainCancel(cmdArgs)
	case "wait":
		return e.cmdWait(cmdArgs)
	case "upstream":
		return e.cmdUpstream(cmdArgs)
	case "shutdown":
		return e.cmdShutdown(cmdArgs)
	case "rollout":
		return e.cmdRollout(cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", cmd)
		fmt.Print(usageText)
		return ExitUsage
	}
}

// extractGlobalFlags 摘出全局参数（-config / --json），返回 (配置路径, 是否 JSON, 其余参数)。
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

// reg 懒连接 etcd；同一进程内复用。
func (e *env) reg() (*registry.Registry, error) {
	if e.registry != nil {
		return e.registry, nil
	}
	r, err := registry.New(e.cfg.Etcd.Endpoints, e.cfg.Etcd.Username, e.cfg.Etcd.Password, e.cfg.EtcdDialTimeout())
	if err != nil {
		return nil, err
	}
	e.registry = r
	return r, nil
}

// close 释放 etcd 连接。
func (e *env) close() {
	if e.registry != nil {
		_ = e.registry.Close()
	}
}

// emit 输出结果：--json 写 JSON 到 stdout；否则写人类可读文案。
// 参数二为人类可读文本（--json 模式下忽略）。
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

// failResult 输出一个「带错误的完整结果对象」并返回 ExitError。
// 与 fail 的区别：fail 只输出错误本身，适合"还没产生任何结果"的场景；
// failResult 用于已经构造出结果（如 rollout 的逐步计划）时，让调用方
// 既能拿到结构化错误、又能看到已经完成到哪一步 —— 且**只输出一个 JSON 对象**。
func (e *env) failResult(v any, err error) int {
	if e.jsonOut {
		printJSON(v)
	} else {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
	}
	return ExitError
}

// usageErr 报告用法错误（参数缺失 / 未授权的破坏性操作）。
func (e *env) usageErr(msg string) int {
	if e.jsonOut {
		printJSON(map[string]any{"ok": false, "error": msg, "usage": true})
	} else {
		fmt.Fprintln(os.Stderr, "用法错误: "+msg)
	}
	return ExitUsage
}

// parseArgs 解析子命令参数：先把位置参数与 flags 分离，再交给 flag.Parse。
//
// 为什么要自己分离：Go 的 flag 包遇到**第一个非 flag 参数就停止解析**，
// 而本工具希望 `drain <node> --target x` 这种「节点在前」的写法也能用，
// 否则 node 后面的所有 flag 都会被当成位置参数静默丢弃。
// 返回 (位置参数, 是否解析成功)。
func parseArgs(fs *flag.FlagSet, args []string) ([]string, bool) {
	needsValue := make(map[string]bool)
	fs.VisitAll(func(f *flag.Flag) {
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			return
		}
		needsValue[f.Name] = true
	})

	var pos, flags []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue // --key=value：值已内联，不吞下一个参数
		}
		if needsValue[name] && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	if err := fs.Parse(flags); err != nil {
		return nil, false
	}
	return pos, true
}

// firstArg 取第一个位置参数；没有则返回空串。
func firstArg(pos []string) string {
	if len(pos) == 0 {
		return ""
	}
	return pos[0]
}

// resolveAdminAddr 决定对某节点下发控制指令时使用的 admin 地址。
//
// 优先级与归一化规则：
//  1. override（--addr）最高，直接照用；
//  2. 节点自报的 admin 地址（clover/nodes 的 admin 字段）；
//  3. 兜底：节点 ID 的 host + 配置的 default_port（仅老版本节点未上报时）。
//
// 「回环 / 通配」主机对远端工具没有意义，一律换成节点 ID 里的主机名——
// 这样跨机管理能否成功，取决于节点是否把 admin.listen_addr 配成了内网可达地址。
func resolveAdminAddr(node registry.Node, defaultPort int, override string) string {
	if override != "" {
		return override
	}
	nodeHost, _ := splitHostPort(node.ID)

	if node.Admin != "" {
		host, port := splitHostPort(node.Admin)
		if port == "" {
			port = strconv.Itoa(defaultPort)
		}
		if isLocalOnlyHost(host) && nodeHost != "" && !isLocalOnlyHost(nodeHost) {
			host = nodeHost
		}
		if host == "" {
			host = "127.0.0.1"
		}
		return net.JoinHostPort(host, port)
	}

	if nodeHost == "" {
		return ""
	}
	return net.JoinHostPort(nodeHost, strconv.Itoa(defaultPort))
}

// splitHostPort 拆 host:port；拆不出时把整串当 host，端口留空。
func splitHostPort(addr string) (host, port string) {
	if addr == "" {
		return "", ""
	}
	if h, p, err := net.SplitHostPort(addr); err == nil {
		return h, p
	}
	return addr, ""
}

// isLocalOnlyHost 判断主机名是否只对本机有意义（回环 / 通配 / 空）。
func isLocalOnlyHost(host string) bool {
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "", "0.0.0.0", "::", "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// printTable 以对齐表格输出到 w。
func printTable(w io.Writer, headers []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
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
// AI / CI 环境下为 false，此时破坏性操作必须走 --yes（见 confirm）。
func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// confirm 交互式确认。
//
// 非交互（stdin 不是终端）时**不再询问**：返回错误，由调用方转成 ExitUsage。
// 自动化场景下"阻塞在提示符上直到超时"比"明确拒绝执行"危险得多。
func confirm(prompt string) (bool, error) {
	if !isInteractive() {
		return false, fmt.Errorf("非交互环境：破坏性操作需显式加 --yes")
	}
	fmt.Printf("%s [y/N] ", prompt)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false, nil
	}
	s := strings.ToLower(strings.TrimSpace(sc.Text()))
	return s == "y" || s == "yes", nil
}

// askConfirm 是 confirm 的调用侧封装：处理"非交互未授权"与"用户拒绝"两种情况。
// 返回 (是否继续, 退出码)。退出码为 ExitOK 时才继续。
func (e *env) askConfirm(prompt string) (bool, int) {
	ok, err := confirm(prompt)
	if err != nil {
		return false, e.usageErr(err.Error())
	}
	if !ok {
		if e.jsonOut {
			printJSON(map[string]any{"ok": false, "cancelled": true})
		} else {
			fmt.Println("已取消。")
		}
		return false, ExitOK
	}
	return true, ExitOK
}

// sortStringsUnique 去重 + 排序（rollout 的节点集合用）。
func sortStringsUnique(in []string) []string {
	set := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		set[s] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// splitCSV 切分逗号分隔列表。
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, ",")
}
