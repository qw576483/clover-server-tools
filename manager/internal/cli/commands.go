package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/qw576483/clover-server-tools/manager/internal/adminclient"
	"github.com/qw576483/clover-server-tools/manager/internal/registry"
)

// waitPollInterval 轮询 drain 状态的默认间隔。
const waitPollInterval = 2 * time.Second

// newFlagSet 构造子命令参数集。
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// note 输出人类可读的进度 / 提示到 stderr。
//
// --json 模式下**完全静默**：调用方（AI / CI）只需读 stdout 的单个 JSON 对象即可，
// 不必担心进度行混进结果；有进度需求的场景改用 wait --json 拿最终状态。
func (e *env) note(format string, args ...any) {
	if e.jsonOut {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// nodeOrOverride 取节点记录：给了 --addr 就不必查 etcd。
func (e *env) nodeOrOverride(id, override string) (registry.Node, error) {
	if override != "" {
		return registry.Node{ID: id}, nil
	}
	if id == "" {
		return registry.Node{}, fmt.Errorf("缺少节点 ID")
	}
	r, err := e.reg()
	if err != nil {
		return registry.Node{}, err
	}
	return r.FindNode(e.ctx, id)
}

// clientFor 按「节点 + 可选 --addr 覆盖」构造 admin 客户端。
// 令牌取自配置的 admin.token（空 = 该部署未启用鉴权，不发令牌头）。
func (e *env) clientFor(node registry.Node, override string) (*adminclient.Client, error) {
	addr := resolveAdminAddr(node, e.cfg.Admin.DefaultPort, override)
	if addr == "" {
		return nil, fmt.Errorf("无法解析节点 %q 的 admin 地址（未上报且无法从节点 ID 推断），请用 --addr 指定", node.ID)
	}
	return adminclient.New(addr, e.cfg.AdminTimeout(), e.cfg.AdminToken()), nil
}

// ---------------------------------------------------------------------------
// nodes
// ---------------------------------------------------------------------------

// nodesReport nodes 命令的 JSON 载荷。
type nodesReport struct {
	OK        bool                `json:"ok"`
	Nodes     []registry.Node     `json:"nodes"`
	Instances []registry.Instance `json:"instances"`
}

func (e *env) cmdNodes(args []string) int {
	fs := newFlagSet("nodes")
	if _, ok := parseArgs(fs, args); !ok {
		return ExitUsage
	}
	r, err := e.reg()
	if err != nil {
		return e.fail(err)
	}
	nodes, err := r.Nodes(e.ctx)
	if err != nil {
		return e.fail(err)
	}
	instances, err := r.Instances(e.ctx)
	if err != nil {
		return e.fail(err)
	}

	if e.jsonOut {
		e.emit(nodesReport{OK: true, Nodes: nodes, Instances: instances}, "")
		return ExitOK
	}

	fmt.Printf("== 节点目录 (clover/nodes) · %d 个 ==\n", len(nodes))
	if len(nodes) == 0 {
		fmt.Println("  (空 —— 节点未接入 etcd，或尚未注册)")
	} else {
		rows := make([][]string, 0, len(nodes))
		for _, n := range nodes {
			rows = append(rows, []string{n.ID, orDash(n.Type), strings.Join(n.Tags, ","), orDash(n.Admin)})
		}
		printTable(os.Stdout, []string{"NODE ID", "TYPE", "TAGS", "ADMIN"}, rows)
	}

	fmt.Printf("\n== 服务实例 (clover/services) · %d 个 ==\n", len(instances))
	if len(instances) == 0 {
		fmt.Println("  (空 —— 未配 etcd 服务发现，或实例未注册)")
	} else {
		rows := make([][]string, 0, len(instances))
		for _, in := range instances {
			rows = append(rows, []string{in.Role, in.Key, in.Addr})
		}
		printTable(os.Stdout, []string{"ROLE", "INSTANCE", "ADDR"}, rows)
	}
	return ExitOK
}

// ---------------------------------------------------------------------------
// status / doctor：节点状态抓取（两者共用 gatherStatus）
// ---------------------------------------------------------------------------

// nodeStatus 单节点的运行状态快照。
type nodeStatus struct {
	Node      registry.Node `json:"node"`
	Admin     string        `json:"admin"`
	Alive     bool          `json:"alive"`
	Routes    int           `json:"routes"`
	Draining  bool          `json:"draining"`
	Phase     string        `json:"phase,omitempty"`
	Remaining int           `json:"remaining"`
	Migrated  int           `json:"migrated"`
	Kicked    int           `json:"kicked"`
	Error     string        `json:"error,omitempty"`
	// DrainError 节点存活但 drain 状态查询失败的原因（最典型：admin.token 不匹配被 401）。
	// 不并入 Error：Error 表示「这个节点整体不可用」，而这里是「存活、但有一项信息拿不到」。
	DrainError string                   `json:"drain_error,omitempty"`
	Drain      *adminclient.DrainStatus `json:"drain,omitempty"`
}

func (e *env) cmdStatus(args []string) int {
	fs := newFlagSet("status")
	all := fs.Bool("all", false, "查看全部节点")
	addr := fs.String("addr", "", "直接指定 admin 地址")
	pos, ok := parseArgs(fs, args)
	if !ok {
		return ExitUsage
	}
	nodeID := firstArg(pos)

	var targets []registry.Node
	switch {
	case *addr != "":
		targets = []registry.Node{{ID: nodeID}}
	case *all:
		r, err := e.reg()
		if err != nil {
			return e.fail(err)
		}
		nodes, err := r.Nodes(e.ctx)
		if err != nil {
			return e.fail(err)
		}
		targets = nodes
	case nodeID != "":
		n, err := e.nodeOrOverride(nodeID, "")
		if err != nil {
			return e.fail(err)
		}
		targets = []registry.Node{n}
	default:
		return e.usageErr("manager status <node>|--all [--addr host:port]")
	}

	if len(targets) == 0 {
		if e.jsonOut {
			e.emit(map[string]any{"ok": true, "nodes": []nodeStatus{}}, "")
		} else {
			fmt.Println("没有已注册的节点。")
		}
		return ExitOK
	}

	results := e.gatherAll(targets, *addr)

	if e.jsonOut {
		down := countDown(results)
		unavailable := countDrainUnavailable(results)
		e.emit(map[string]any{
			"ok": down == 0 && unavailable == 0, "nodes": results,
			"down": down, "drain_unavailable": unavailable,
		}, "")
		if down > 0 || unavailable > 0 {
			return ExitPartial
		}
		return ExitOK
	}

	rows := make([][]string, 0, len(results))
	var errs []string
	for _, st := range results {
		alive := "DOWN"
		if st.Alive {
			alive = "ok"
		}
		drain, phase, remain := "未进行", "-", "-"
		if st.Drain != nil {
			if st.Draining {
				drain = "进行中"
			} else if st.Phase == "done" {
				drain = "已结束"
			}
			phase = orDash(st.Phase)
			remain = fmt.Sprintf("%d", st.Remaining)
		}
		rows = append(rows, []string{
			orDash(st.Node.ID), orDash(st.Admin), alive,
			fmt.Sprintf("%d", st.Routes), drain, phase, remain,
		})
		if st.Error != "" {
			errs = append(errs, fmt.Sprintf("  %s @ %s: %s", st.Node.ID, st.Admin, st.Error))
		}
		// 存活但 drain 状态取不到：此前会静默显示成「未进行」，看起来像节点没在下线。
		if st.DrainError != "" {
			errs = append(errs, fmt.Sprintf("  %s @ %s: drain 状态查询失败: %s", st.Node.ID, st.Admin, st.DrainError))
		}
	}
	printTable(os.Stdout, []string{"NODE ID", "ADMIN", "ALIVE", "ROUTES", "DRAIN", "PHASE", "REMAIN"}, rows)
	if len(errs) > 0 {
		fmt.Println("\n连接失败明细:")
		for _, s := range errs {
			fmt.Println(s)
		}
		return ExitPartial
	}
	return ExitOK
}

// doctorReport doctor 命令的 JSON 载荷（AI 自检入口）。
type doctorReport struct {
	OK        bool                `json:"ok"`
	Etcd      doctorEtcd          `json:"etcd"`
	Summary   doctorSummary       `json:"summary"`
	Nodes     []nodeStatus        `json:"nodes"`
	Instances []registry.Instance `json:"instances"`
	Error     string              `json:"error,omitempty"`
}

// doctorEtcd etcd 连通性。
type doctorEtcd struct {
	Endpoints []string `json:"endpoints"`
	OK        bool     `json:"ok"`
	Error     string   `json:"error,omitempty"`
}

// doctorSummary 汇总计数。
type doctorSummary struct {
	Nodes int `json:"nodes"`
	Alive int `json:"alive"`
	Down  int `json:"down"`
	// DrainUnavailable 存活但 drain 状态读不到的节点数（最典型成因：admin.token 不匹配被 401）。
	// 与 Down 分开计数：这类节点「活着」，只是信息缺一块，不该混进不可达数里。
	DrainUnavailable int `json:"drain_unavailable"`
	Draining         int `json:"draining"`
	Instances        int `json:"instances"`
}

// cmdDoctor 集群体检：一条命令给出「etcd 通不通 + 每个节点活不活 + 是否有正在 drain 的节点」。
// 这是给 AI 用的自检入口 —— 输出稳定、退出码可判定，无需再组合多条命令。
func (e *env) cmdDoctor(args []string) int {
	fs := newFlagSet("doctor")
	tag := fs.String("tag", "", "只体检带该 tag 的节点")
	if _, ok := parseArgs(fs, args); !ok {
		return ExitUsage
	}

	rep := doctorReport{
		Etcd:      doctorEtcd{Endpoints: e.cfg.Etcd.Endpoints},
		Nodes:     []nodeStatus{},
		Instances: []registry.Instance{},
	}

	r, err := e.reg()
	if err != nil {
		rep.Error = err.Error()
		e.emit(rep, "")
		return ExitError
	}
	rep.Etcd.OK = true

	nodes, err := r.Nodes(e.ctx)
	if err != nil {
		rep.Error = err.Error()
		e.emit(rep, "")
		return ExitError
	}
	instances, err := r.Instances(e.ctx)
	if err != nil {
		rep.Error = err.Error()
		e.emit(rep, "")
		return ExitError
	}
	rep.Instances = instances

	if *tag != "" {
		filtered := make([]registry.Node, 0, len(nodes))
		for _, n := range nodes {
			for _, t := range n.Tags {
				if t == *tag {
					filtered = append(filtered, n)
					break
				}
			}
		}
		nodes = filtered
	}

	rep.Nodes = e.gatherAll(nodes, "")
	rep.Summary.Nodes = len(rep.Nodes)
	rep.Summary.Instances = len(instances)
	for _, st := range rep.Nodes {
		if st.Alive {
			rep.Summary.Alive++
		} else {
			rep.Summary.Down++
		}
		if st.Draining {
			rep.Summary.Draining++
		}
		if st.Alive && st.DrainError != "" {
			rep.Summary.DrainUnavailable++
		}
	}
	// drain 状态读不到也是「体检没通过」：否则令牌配错时 doctor 会报 ok=true，
	// 而 drain 进度整列是空的 —— 这正是上一版静默 401 的模样。
	rep.OK = rep.Summary.Down == 0 && rep.Summary.DrainUnavailable == 0

	if e.jsonOut {
		e.emit(rep, "")
		if !rep.OK {
			return ExitPartial
		}
		return ExitOK
	}

	fmt.Printf("集群体检: etcd=%v (%s)\n", rep.Etcd.OK, strings.Join(rep.Etcd.Endpoints, ","))
	// 打印「带不带令牌」而不是令牌本身：令牌是凭据，任何输出里都不该出现它的值。
	if e.cfg.AdminToken() != "" {
		fmt.Println("admin 鉴权: 已配置 admin.token，每个 admin 请求带 " + adminclient.TokenHeader + " 头")
	} else {
		fmt.Println("admin 鉴权: 未配置 admin.token（不发令牌头；被管节点若启用了令牌会被 401 拒绝）")
	}
	fmt.Printf("节点 %d 个：存活 %d / 不可达 %d / 正在 drain %d / drain 状态不可读 %d；服务实例 %d 个\n\n",
		rep.Summary.Nodes, rep.Summary.Alive, rep.Summary.Down, rep.Summary.Draining,
		rep.Summary.DrainUnavailable, rep.Summary.Instances)
	if len(rep.Nodes) > 0 {
		rows := make([][]string, 0, len(rep.Nodes))
		for _, st := range rep.Nodes {
			alive := "DOWN"
			if st.Alive {
				alive = "ok"
			}
			rows = append(rows, []string{
				orDash(st.Node.ID), orDash(st.Node.Type), strings.Join(st.Node.Tags, ","),
				orDash(st.Admin), alive, fmt.Sprintf("%d", st.Routes),
			})
		}
		printTable(os.Stdout, []string{"NODE ID", "TYPE", "TAGS", "ADMIN", "ALIVE", "ROUTES"}, rows)
	}
	// drain 状态读不到的节点逐条列出（人类模式下必须能一眼看到成因，不能只有一个计数）。
	for _, st := range rep.Nodes {
		if st.Alive && st.DrainError != "" {
			fmt.Printf("  ⚠ %s @ %s: drain 状态不可读: %s\n", orDash(st.Node.ID), orDash(st.Admin), st.DrainError)
		}
	}
	if rep.Summary.Down > 0 || rep.Summary.DrainUnavailable > 0 {
		return ExitPartial
	}
	return ExitOK
}

// gatherAll 并发抓取多个节点的状态。
func (e *env) gatherAll(targets []registry.Node, overrideAddr string) []nodeStatus {
	results := make([]nodeStatus, len(targets))
	var wg sync.WaitGroup
	for i, n := range targets {
		wg.Add(1)
		go func(i int, n registry.Node) {
			defer wg.Done()
			results[i] = e.gatherStatus(n, overrideAddr)
		}(i, n)
	}
	wg.Wait()
	return results
}

// gatherStatus 抓一个节点的存活与 drain 状态。
func (e *env) gatherStatus(node registry.Node, overrideAddr string) nodeStatus {
	st := nodeStatus{Node: node}
	st.Admin = resolveAdminAddr(node, e.cfg.Admin.DefaultPort, overrideAddr)
	if st.Admin == "" {
		st.Error = "无法解析 admin 地址"
		return st
	}
	c := adminclient.New(st.Admin, e.cfg.AdminTimeout(), e.cfg.AdminToken())
	ping, err := c.Ping(e.ctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Alive = true
	st.Routes = len(ping.Routes)
	if ds, derr := c.DrainStatus(e.ctx); derr == nil {
		st.Drain = ds
		st.Draining = ds.Draining
		st.Phase = ds.Phase
		st.Remaining = ds.Remaining
		st.Migrated = ds.Migrated
		st.Kicked = ds.Kicked
	} else {
		// 非预期分支必须留痕：节点活着但 drain 状态取不到（此前唯一成因是令牌 401，
		// 那会让 drain 进度整列显示为空却不报错，看起来像「没在下线」）。
		// 这里把原因带进结果对象，doctor/status 都能看到。
		st.DrainError = derr.Error()
	}
	return st
}

// countDown 统计结果里不可达的节点数。
func countDown(results []nodeStatus) int {
	n := 0
	for _, st := range results {
		if !st.Alive {
			n++
		}
	}
	return n
}

// countDrainUnavailable 统计「存活但 drain 状态读不到」的节点数
// （最典型成因：admin.token 不匹配被 401）。与 countDown 分开，
// 因为它不代表节点不可用，只代表少了一栏信息 —— 但同样必须让退出码变 3。
func countDrainUnavailable(results []nodeStatus) int {
	n := 0
	for _, st := range results {
		if st.Alive && st.DrainError != "" {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// drain / drain-status / drain-cancel / wait
// ---------------------------------------------------------------------------

// drainAction drain 与 drain-cancel 的统一 JSON 载荷。
type drainAction struct {
	OK      bool                      `json:"ok"`
	Action  string                    `json:"action"`
	Node    string                    `json:"node"`
	Admin   string                    `json:"admin"`
	Request *adminclient.DrainRequest `json:"request,omitempty"`
	Started *adminclient.DrainStatus  `json:"started,omitempty"`
	Final   *drainOutcome             `json:"final,omitempty"`
}

func (e *env) cmdDrain(args []string) int {
	fs := newFlagSet("drain")
	mode := fs.String("mode", "hybrid", "grace | migrate | hybrid")
	target := fs.String("target", "", "新进程逻辑服地址（migrate/hybrid 必填）")
	grace := fs.String("grace", "", "迁移 / 等待自然退出的总时长，如 5m（缺省由引擎兜 5m）")
	hardTimeout := fs.String("hard-timeout", "", "强踢阶段硬上限，如 2m（缺省由引擎兜 2m）")
	fs.StringVar(hardTimeout, "hard_timeout", "", "同 --hard-timeout")
	stopAfter := fs.Bool("stop-after", false, "归零后自动停机（滚动重启置 true）")
	fs.BoolVar(stopAfter, "stop_after", false, "同 --stop-after")
	wait := fs.Bool("wait", false, "轮询到编排结束再返回（自动化推荐）")
	timeout := fs.Duration("timeout", 0, "配合 --wait：等待上限，缺省按 grace+hard_timeout 估算")
	addr := fs.String("addr", "", "直接指定 admin 地址")
	yes := fs.Bool("yes", false, "跳过确认")
	pos, ok := parseArgs(fs, args)
	if !ok {
		return ExitUsage
	}
	nodeID := firstArg(pos)
	if nodeID == "" && *addr == "" {
		return e.usageErr("manager drain <node> [--target addr] [--mode hybrid] ...")
	}
	if *mode != "grace" && *target == "" {
		return e.usageErr("mode=migrate/hybrid 必须给 --target（新进程地址）")
	}

	node, err := e.nodeOrOverride(nodeID, *addr)
	if err != nil {
		return e.fail(err)
	}
	c, err := e.clientFor(node, *addr)
	if err != nil {
		return e.fail(err)
	}

	if !*yes {
		cont, rc := e.askConfirm(fmt.Sprintf("对 %s (%s) 发起 drain（mode=%s target=%s stop_after=%v）?",
			nodeID, c.Addr(), *mode, orDash(*target), *stopAfter))
		if !cont {
			return rc
		}
	}

	req := adminclient.DrainRequest{
		Mode:        *mode,
		Target:      *target,
		Grace:       *grace,
		HardTimeout: *hardTimeout,
		StopAfter:   *stopAfter,
	}
	st, err := c.Drain(e.ctx, req)
	if err != nil {
		return e.fail(err)
	}

	out := drainAction{OK: true, Action: "drain", Node: nodeID, Admin: c.Addr(), Request: &req, Started: st}
	if *wait {
		limit := *timeout
		if limit <= 0 {
			limit = drainWaitLimit(*grace, *hardTimeout)
		}
		res := e.waitFor(c, nodeID, limit)
		out.Final = &res
		out.OK = res.OK
	}
	e.emit(out, "")
	if !e.jsonOut {
		// 人类模式：直接用发起时的状态摘要，避免 JSON 结构对人不友好。
		if out.Final != nil {
			fmt.Printf("drain 完成: node=%s phase=%s migrated=%d kicked=%d remaining=%d\n",
				nodeID, orDash(out.Final.Phase), out.Final.Migrated, out.Final.Kicked, out.Final.Remaining)
		} else {
			fmt.Printf("已发起: node=%s admin=%s mode=%s target=%s phase=%s\n",
				nodeID, c.Addr(), st.Mode, orDash(st.Target), orDash(st.Phase))
		}
	}
	if !out.OK {
		return ExitError
	}
	return ExitOK
}

func (e *env) cmdDrainStatus(args []string) int {
	fs := newFlagSet("drain-status")
	addr := fs.String("addr", "", "直接指定 admin 地址")
	pos, ok := parseArgs(fs, args)
	if !ok {
		return ExitUsage
	}
	nodeID := firstArg(pos)
	if nodeID == "" && *addr == "" {
		return e.usageErr("manager drain-status <node>")
	}
	node, err := e.nodeOrOverride(nodeID, *addr)
	if err != nil {
		return e.fail(err)
	}
	c, err := e.clientFor(node, *addr)
	if err != nil {
		return e.fail(err)
	}
	st, err := c.DrainStatus(e.ctx)
	if err != nil {
		return e.fail(err)
	}
	if e.jsonOut {
		e.emit(map[string]any{"ok": true, "node": nodeID, "admin": c.Addr(), "status": st}, "")
		return ExitOK
	}
	printDrainStatus(nodeID, c.Addr(), st)
	return ExitOK
}

func (e *env) cmdDrainCancel(args []string) int {
	fs := newFlagSet("drain-cancel")
	addr := fs.String("addr", "", "直接指定 admin 地址")
	yes := fs.Bool("yes", false, "跳过确认")
	pos, ok := parseArgs(fs, args)
	if !ok {
		return ExitUsage
	}
	nodeID := firstArg(pos)
	if nodeID == "" && *addr == "" {
		return e.usageErr("manager drain-cancel <node>")
	}
	node, err := e.nodeOrOverride(nodeID, *addr)
	if err != nil {
		return e.fail(err)
	}
	c, err := e.clientFor(node, *addr)
	if err != nil {
		return e.fail(err)
	}
	if !*yes {
		cont, rc := e.askConfirm(fmt.Sprintf("取消 %s (%s) 的 drain（回滚）?", nodeID, c.Addr()))
		if !cont {
			return rc
		}
	}
	if err := c.CancelDrain(e.ctx); err != nil {
		return e.fail(err)
	}
	e.emit(drainAction{OK: true, Action: "drain-cancel", Node: nodeID, Admin: c.Addr()},
		fmt.Sprintf("已取消 drain: node=%s admin=%s\n", nodeID, c.Addr()))
	return ExitOK
}

// cmdWait 阻塞等待 drain 收敛。脚本 / AI 编排里用来"等上一个节点归零"。
func (e *env) cmdWait(args []string) int {
	fs := newFlagSet("wait")
	timeout := fs.Duration("timeout", 30*time.Minute, "等待上限")
	interval := fs.Duration("interval", waitPollInterval, "轮询间隔")
	addr := fs.String("addr", "", "直接指定 admin 地址")
	pos, ok := parseArgs(fs, args)
	if !ok {
		return ExitUsage
	}
	nodeID := firstArg(pos)
	if nodeID == "" && *addr == "" {
		return e.usageErr("manager wait <node> [--timeout 30m]")
	}
	node, err := e.nodeOrOverride(nodeID, *addr)
	if err != nil {
		return e.fail(err)
	}
	c, err := e.clientFor(node, *addr)
	if err != nil {
		return e.fail(err)
	}
	res := e.waitWithInterval(c, nodeID, *timeout, *interval)
	e.emit(res, "")
	if !e.jsonOut {
		if res.OK {
			fmt.Printf("已收敛: node=%s phase=%s migrated=%d kicked=%d remaining=%d (%.1fs)\n",
				nodeID, orDash(res.Phase), res.Migrated, res.Kicked, res.Remaining, float64(res.ElapsedMS)/1000)
		}
	}
	if !res.OK {
		return ExitError
	}
	return ExitOK
}

// printDrainStatus 打印一次 drain 状态（人类模式）。
func printDrainStatus(nodeID, admin string, st *adminclient.DrainStatus) {
	state := "未进行"
	if st.Draining {
		state = "进行中"
	} else if st.Phase == "done" {
		state = "已结束"
	}
	fmt.Printf("node=%s admin=%s\n", nodeID, admin)
	fmt.Printf("  状态: %s  phase=%s  mode=%s  target=%s\n", state, orDash(st.Phase), orDash(st.Mode), orDash(st.Target))
	fmt.Printf("  进度: remaining=%d migrated=%d kicked=%d\n", st.Remaining, st.Migrated, st.Kicked)
	if !st.StartedAt.IsZero() {
		fmt.Printf("  开始: %s  截止: %s\n", st.StartedAt.Format(time.RFC3339), st.Deadline.Format(time.RFC3339))
	}
}

// ---------------------------------------------------------------------------
// upstream / shutdown
// ---------------------------------------------------------------------------

func (e *env) cmdUpstream(args []string) int {
	fs := newFlagSet("upstream")
	set := fs.String("set", "", "切换默认上游到 host:port")
	addr := fs.String("addr", "", "直接指定 admin 地址")
	pos, ok := parseArgs(fs, args)
	if !ok {
		return ExitUsage
	}
	nodeID := firstArg(pos)
	if nodeID == "" && *addr == "" {
		return e.usageErr("manager upstream <node> [--set host:port]")
	}
	node, err := e.nodeOrOverride(nodeID, *addr)
	if err != nil {
		return e.fail(err)
	}
	c, err := e.clientFor(node, *addr)
	if err != nil {
		return e.fail(err)
	}

	if *set == "" {
		cur, err := c.Upstream(e.ctx)
		if err != nil {
			return e.fail(err)
		}
		e.emit(map[string]any{"ok": true, "action": "upstream-get", "node": nodeID, "admin": c.Addr(), "upstream": cur},
			fmt.Sprintf("node=%s admin=%s upstream=%s\n", nodeID, c.Addr(), orDash(cur)))
		return ExitOK
	}
	cur, err := c.SetUpstream(e.ctx, *set)
	if err != nil {
		return e.fail(err)
	}
	e.emit(map[string]any{"ok": true, "action": "upstream-set", "node": nodeID, "admin": c.Addr(), "upstream": cur},
		fmt.Sprintf("已切换: node=%s admin=%s upstream=%s\n", nodeID, c.Addr(), orDash(cur)))
	return ExitOK
}

func (e *env) cmdShutdown(args []string) int {
	fs := newFlagSet("shutdown")
	addr := fs.String("addr", "", "直接指定 admin 地址")
	yes := fs.Bool("yes", false, "跳过确认")
	pos, ok := parseArgs(fs, args)
	if !ok {
		return ExitUsage
	}
	nodeID := firstArg(pos)
	if nodeID == "" && *addr == "" {
		return e.usageErr("manager shutdown <node>")
	}
	node, err := e.nodeOrOverride(nodeID, *addr)
	if err != nil {
		return e.fail(err)
	}
	c, err := e.clientFor(node, *addr)
	if err != nil {
		return e.fail(err)
	}
	if !*yes {
		cont, rc := e.askConfirm(fmt.Sprintf("请求 %s (%s) 优雅退出?", nodeID, c.Addr()))
		if !cont {
			return rc
		}
	}
	if err := c.Shutdown(e.ctx); err != nil {
		return e.fail(err)
	}
	e.emit(map[string]any{"ok": true, "action": "shutdown", "node": nodeID, "admin": c.Addr()},
		fmt.Sprintf("已请求退出: node=%s admin=%s\n", nodeID, c.Addr()))
	return ExitOK
}

// ---------------------------------------------------------------------------
// rollout：滚动发布编排
// ---------------------------------------------------------------------------

// rolloutResult rollout 的 JSON 载荷（AI 据此逐步核对）。
type rolloutResult struct {
	OK     bool          `json:"ok"`
	Mode   string        `json:"mode"`
	Target string        `json:"target"`
	Launch bool          `json:"launch"`
	Steps  []rolloutStep `json:"steps"`
	Error  string        `json:"error,omitempty"`
}

// rolloutStep 单个节点的编排结果。
type rolloutStep struct {
	Index     int    `json:"index"`
	Node      string `json:"node"`
	OK        bool   `json:"ok"`
	Launched  bool   `json:"launched,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Migrated  int    `json:"migrated"`
	Kicked    int    `json:"kicked"`
	Remaining int    `json:"remaining"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Error     string `json:"error,omitempty"`
}

func (e *env) cmdRollout(args []string) int {
	fs := newFlagSet("rollout")
	nodesFlag := fs.String("nodes", "", "逗号分隔的旧节点 ID（与 --tag 二选一）")
	tag := fs.String("tag", "", "按 tag 过滤旧节点")
	target := fs.String("target", "", "新进程逻辑服地址（migrate/hybrid 必填）")
	mode := fs.String("mode", "hybrid", "grace | migrate | hybrid")
	grace := fs.String("grace", "", "迁移 / 等待自然退出的总时长，如 5m")
	hardTimeout := fs.String("hard-timeout", "", "强踢阶段硬上限，如 2m")
	fs.StringVar(hardTimeout, "hard_timeout", "", "同 --hard-timeout")
	stopAfter := fs.Bool("stop-after", false, "每个旧节点归零后自动停机")
	fs.BoolVar(stopAfter, "stop_after", false, "同 --stop-after")
	interval := fs.Duration("interval", 3*time.Second, "两个节点之间的间隔")
	timeout := fs.Duration("timeout", 0, "单节点等待上限，缺省按 grace+hard_timeout 估算")
	launch := fs.String("launch", "", "每轮开始前执行的起新进程命令（交给 systemd / k8s / ssh）")
	addr := fs.String("addr", "", "直接指定 admin 地址（仅单节点时有意义）")
	dryRun := fs.Bool("dry-run", false, "只打印将要执行的动作，不真正下发")
	fs.BoolVar(dryRun, "dry_run", false, "同 --dry-run")
	yes := fs.Bool("yes", false, "跳过确认")
	if _, ok := parseArgs(fs, args); !ok {
		return ExitUsage
	}

	if *mode != "grace" && *target == "" {
		return e.usageErr("mode=migrate/hybrid 必须给 --target（新进程地址）")
	}

	targets, err := e.rolloutTargets(*nodesFlag, *tag, *addr)
	if err != nil {
		return e.fail(err)
	}
	if len(targets) == 0 {
		return e.usageErr("没有匹配的节点（用 --nodes 或 --tag 指定）")
	}

	plan := rolloutResult{OK: true, Mode: *mode, Target: *target, Launch: *launch != "", Steps: []rolloutStep{}}
	for i, n := range targets {
		plan.Steps = append(plan.Steps, rolloutStep{Index: i + 1, Node: n.ID, OK: true})
	}

	// 计划只写给人类看。--json 下必须保证整个命令**只输出一个 JSON 对象**
	// （最终结果），否则解析方拿到两段 JSON 会直接失败。
	if !e.jsonOut {
		fmt.Printf("滚动发布计划: %d 个节点，mode=%s target=%s stop_after=%v launch=%v\n",
			len(targets), *mode, orDash(*target), *stopAfter, *launch != "")
		for _, s := range plan.Steps {
			fmt.Printf("  [%d] %s\n", s.Index, s.Node)
		}
	}

	if *dryRun {
		// --dry-run 也必须给出可解析的结果：JSON 下回吐计划对象，
		// 人类模式下给一句提示。**任何返回路径都不能让 stdout 为空**，
		// 否则调用方解析 JSON 会直接拿到 null。
		if e.jsonOut {
			e.emit(plan, "")
		} else {
			fmt.Println("\n--dry-run: 未执行任何操作。")
		}
		return ExitOK
	}
	if !*yes {
		cont, rc := e.askConfirm("确认按上述顺序执行?")
		if !cont {
			return rc
		}
	}

	limit := *timeout
	if limit <= 0 {
		limit = drainWaitLimit(*grace, *hardTimeout)
	}

	for i := range targets {
		n := targets[i]

		// 起新进程交给外部（k8s / systemd / ssh）——引擎的 drain 只处理存量连接。
		if *launch != "" {
			if !e.jsonOut {
				fmt.Printf("\n== [%d/%d] %s ==\n", i+1, len(targets), n.ID)
				fmt.Printf("  launch: %s\n", *launch)
			}
			if err := runShell(*launch); err != nil {
				plan.Steps[i].OK = false
				plan.Steps[i].Error = "launch 命令失败: " + err.Error()
				plan.OK = false
				plan.Error = plan.Steps[i].Error
				return e.failResult(plan, fmt.Errorf("launch 命令失败，编排中止于节点 %s: %w", n.ID, err))
			}
			plan.Steps[i].Launched = true
		} else if !e.jsonOut {
			fmt.Printf("\n== [%d/%d] %s ==\n", i+1, len(targets), n.ID)
		}

		c, cerr := e.clientFor(n, *addr)
		if cerr != nil {
			plan.Steps[i].OK = false
			plan.Steps[i].Error = cerr.Error()
			plan.OK = false
			plan.Error = cerr.Error()
			return e.failResult(plan, cerr)
		}
		if _, derr := c.Drain(e.ctx, adminclient.DrainRequest{
			Mode:        *mode,
			Target:      *target,
			Grace:       *grace,
			HardTimeout: *hardTimeout,
			StopAfter:   *stopAfter,
		}); derr != nil {
			plan.Steps[i].OK = false
			plan.Steps[i].Error = derr.Error()
			plan.OK = false
			plan.Error = derr.Error()
			return e.failResult(plan, fmt.Errorf("drain %s 失败，编排中止: %w", n.ID, derr))
		}

		res := e.waitWithInterval(c, n.ID, limit, waitPollInterval)
		plan.Steps[i].Phase = res.Phase
		plan.Steps[i].Migrated = res.Migrated
		plan.Steps[i].Kicked = res.Kicked
		plan.Steps[i].Remaining = res.Remaining
		plan.Steps[i].ElapsedMS = res.ElapsedMS
		if !res.OK {
			plan.Steps[i].OK = false
			plan.Steps[i].Error = res.Error
			plan.OK = false
			plan.Error = res.Error
			return e.failResult(plan, fmt.Errorf("等待 %s 收敛失败，编排中止: %s", n.ID, res.Error))
		}

		if i != len(targets)-1 && *interval > 0 {
			e.note("  等待 %s 后处理下一个...", *interval)
			time.Sleep(*interval)
		}
	}

	e.emit(plan, "\n滚动发布完成。\n")
	return ExitOK
}

// rolloutTargets 解析待处理的旧节点：--nodes 显式列表优先，其次 --tag 过滤，再次 --addr 单节点。
func (e *env) rolloutTargets(nodesFlag, tag, addr string) ([]registry.Node, error) {
	if ids := sortStringsUnique(splitCSV(nodesFlag)); len(ids) > 0 {
		out := make([]registry.Node, 0, len(ids))
		for _, id := range ids {
			n, err := e.nodeOrOverride(id, "")
			if err != nil {
				return nil, err
			}
			out = append(out, n)
		}
		return out, nil
	}
	if addr != "" {
		return []registry.Node{{ID: ""}}, nil
	}
	if tag == "" {
		return nil, fmt.Errorf("需要 --nodes 或 --tag 指定待处理的旧节点")
	}
	r, err := e.reg()
	if err != nil {
		return nil, err
	}
	all, err := r.Nodes(e.ctx)
	if err != nil {
		return nil, err
	}
	out := make([]registry.Node, 0, len(all))
	for _, n := range all {
		for _, t := range n.Tags {
			if t == tag {
				out = append(out, n)
				break
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 等待收敛
// ---------------------------------------------------------------------------

// drainOutcome 等待结果（JSON 可直接消费）。
type drainOutcome struct {
	OK        bool   `json:"ok"`
	Node      string `json:"node"`
	Phase     string `json:"phase,omitempty"`
	Migrated  int    `json:"migrated"`
	Kicked    int    `json:"kicked"`
	Remaining int    `json:"remaining"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Error     string `json:"error,omitempty"`
}

// waitFor 用默认间隔等待收敛。
func (e *env) waitFor(c *adminclient.Client, node string, limit time.Duration) drainOutcome {
	return e.waitWithInterval(c, node, limit, waitPollInterval)
}

// waitWithInterval 轮询节点 drain 状态直到编排结束。
// 超时 / 轮询报错都转成结构化结果（不 panic、不阻塞），由调用方决定退出码。
func (e *env) waitWithInterval(c *adminclient.Client, node string, limit, interval time.Duration) drainOutcome {
	if interval <= 0 {
		interval = waitPollInterval
	}
	start := time.Now()
	out := drainOutcome{Node: node}
	deadline := start.Add(limit)
	tk := time.NewTicker(interval)
	defer tk.Stop()

	lastPhase := "\x00"
	for {
		st, err := c.DrainStatus(e.ctx)
		if err != nil {
			out.Error = err.Error()
			out.ElapsedMS = time.Since(start).Milliseconds()
			return out
		}
		out.Phase, out.Migrated, out.Kicked, out.Remaining = st.Phase, st.Migrated, st.Kicked, st.Remaining
		if st.Phase != lastPhase {
			e.note("  %s: phase=%s remaining=%d migrated=%d kicked=%d",
				node, orDash(st.Phase), st.Remaining, st.Migrated, st.Kicked)
			lastPhase = st.Phase
		}
		if !st.Draining {
			out.OK = true
			out.ElapsedMS = time.Since(start).Milliseconds()
			return out
		}
		if time.Now().After(deadline) {
			out.Error = fmt.Sprintf("等待超时（%s），仍处于 draining，remaining=%d", limit, st.Remaining)
			out.ElapsedMS = time.Since(start).Milliseconds()
			return out
		}
		select {
		case <-e.ctx.Done():
			out.Error = "已中断"
			out.ElapsedMS = time.Since(start).Milliseconds()
			return out
		case <-tk.C:
		}
	}
}

// drainWaitLimit 估算等待上限：宽限 + 硬超时 + 缓冲；解析不出时用 30m。
func drainWaitLimit(grace, hardTimeout string) time.Duration {
	g, errG := time.ParseDuration(grace)
	h, errH := time.ParseDuration(hardTimeout)
	if errG != nil && errH != nil {
		return 30 * time.Minute
	}
	if errG != nil {
		g = 5 * time.Minute
	}
	if errH != nil {
		h = 2 * time.Minute
	}
	return g + h + 2*time.Minute
}

// runShell 以系统 shell 执行一条命令，stdout / stderr 直通。
func runShell(cmd string) error {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd", "/C", cmd)
	} else {
		c = exec.Command("sh", "-c", cmd)
	}
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// orDash 空串显示为 "-"。
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
