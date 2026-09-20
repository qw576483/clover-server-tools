package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"robot/internal/authclient"
	"robot/internal/client"
	"robot/internal/load"
)

// gatewayRateLimit 网关的默认消息级限流：单连接 64 帧/秒（突发 128）。
// 见引擎 bootstrap.go 的 ratelimit.GCRAPolicy(64, 128)。
// 间隔低于此值时给出提示 —— 否则用户会把"被限流"误读成"服务端扛不住"。
const gatewayRateLimitPerSec = 64

// ---------- run ----------

// cmdRun 批量机器人压测。
func (e *env) cmdRun(args []string) int {
	fs := newFlagSet("run")
	d := e.cfg.Load

	robots := fs.Int("robots", d.Robots, "机器人数量")
	prefix := fs.String("prefix", d.AccountPrefix, "账号前缀")
	start := fs.Int("start", d.AccountStart, "账号序号起点（含）")
	password := fs.String("password", d.Password, "统一密码")
	transport := fs.String("transport", d.Transport, "传输层：auto|quic|tcp")
	ramp := fs.Int("ramp", d.RampPerSec, "每秒启动多少个机器人；0=不限（全并发）")
	duration := fs.Duration("duration", e.cfg.Duration(), "登录后保持在线时长；0=登录成功即退")
	msgID := fs.Uint("msg", 0, "登录后周期发送的消息号；0=不发（只挂着）")
	body := fs.String("body", "{}", "周期消息体，支持 {i} 占位（替换为机器人序号）")
	interval := fs.Duration("interval", e.cfg.MsgInterval(), "周期消息发送间隔")
	once := fs.Bool("once", false, "周期消息只发一次")
	unreliable := fs.Bool("unreliable", false, "走不可靠通道发送（QUIC Datagram / 裸 UDP）")
	doSignup := fs.Bool("signup", false, "账号不存在时自动注册（会往账号库写数据）")
	requireSync := fs.Bool("require-fullsync", false, "要求必须收到全量同步（EPushPlayerFullSync）才算成功")
	loginTimeout := fs.Duration("login-timeout", e.cfg.LoginTimeout(), "单个机器人「连上→登录完成」的总超时")
	dialTimeout := fs.Duration("dial-timeout", 5*time.Second, "单次拨号超时")
	probeTimeout := fs.Duration("probe-timeout", 2*time.Second, "QUIC 探测超时（auto 模式快速失败用）")
	assumeYes := fs.Bool("yes", false, "跳过确认（非交互环境配合 --signup 使用）")
	var setups stringList
	fs.Var(&setups, "setup", "登录后发送一次的消息，格式 <消息号>:<JSON>，可重复")

	if err := fs.Parse(args); err != nil {
		return e.usageErr(err.Error())
	}

	tp, err := client.ParseTransport(*transport)
	if err != nil {
		return e.usageErr(err.Error())
	}
	if *robots <= 0 {
		return e.usageErr("--robots 必须大于 0")
	}
	if *start <= 0 {
		return e.usageErr("--start 必须大于 0")
	}
	if strings.TrimSpace(*password) == "" {
		return e.usageErr("--password 不能为空")
	}
	if *duration < 0 {
		return e.usageErr("--duration 不能为负")
	}
	if *ramp < 0 {
		return e.usageErr("--ramp 不能为负")
	}
	if *interval <= 0 {
		return e.usageErr("--interval 必须大于 0")
	}
	if *msgID > 0 && uint32(*msgID) <= client.InternalMsgMax {
		return e.usageErr(fmt.Sprintf("--msg 是业务消息号，必须 >= %d（引擎保留 [1,%d]）",
			client.InternalMsgMax+1, client.InternalMsgMax))
	}

	steps, err := parseSetup(setups)
	if err != nil {
		return e.usageErr(err.Error())
	}

	cfg := load.RunConfig{
		Robots:          *robots,
		AccountPrefix:   *prefix,
		AccountStart:    *start,
		Password:        *password,
		Gateway:         e.cfg.Gateway,
		GatewayTCP:      e.cfg.GatewayTCP,
		AuthAddr:        e.cfg.AuthAddr,
		Transport:       tp,
		RampPerSec:      *ramp,
		Duration:        *duration,
		MsgID:           uint32(*msgID),
		MsgBody:         *body,
		MsgInterval:     *interval,
		MsgOnce:         *once,
		Unreliable:      *unreliable,
		Signup:          *doSignup,
		RequireFullSync: *requireSync,
		Setup:           steps,
		DialTimeout:     *dialTimeout,
		ProbeTimeout:    *probeTimeout,
		LoginTimeout:    *loginTimeout,
	}

	// 批量注册是往账号库写数据，按自动化契约必须先确认。
	if cfg.Signup {
		ok, code := e.askConfirm(
			fmt.Sprintf("将尝试注册/登录 %d 个账号（前缀 %q，起始序号 %d），确认继续？",
				cfg.Robots, cfg.AccountPrefix, cfg.AccountStart), *assumeYes)
		if !ok {
			return code
		}
	}

	if !e.jsonOut {
		e.printRunHeader(cfg)
	}

	// 间隔低于网关默认限流阈值时提醒：否则"被限流"会被误读成"服务端扛不住"。
	if cfg.MsgID != 0 && !cfg.MsgOnce {
		if perSec := 1.0 / cfg.MsgInterval.Seconds(); perSec > gatewayRateLimitPerSec {
			fmt.Fprintf(os.Stderr,
				"提示: --interval %s 约合 %.0f 帧/秒/连接，高于网关默认限流 %d 帧/秒，\n"+
					"      超出部分会被拒（报告里表现为 send_failed 或 unmatched），不代表服务端容量上限。\n",
				cfg.MsgInterval, perSec, gatewayRateLimitPerSec)
		}
	}

	var bar *progress
	if !e.jsonOut {
		bar = newProgress()
	}

	rep := load.Run(e.ctx, cfg, func(done, total int) {
		if bar != nil {
			bar.update(done, total)
		}
	})
	if bar != nil {
		bar.finish()
	}

	e.emit(rep, renderReport(rep))
	return exitFor(rep)
}

// printRunHeader 在人类模式下先说明"要往哪压"，避免报告出来才发现地址填错了。
func (e *env) printRunHeader(cfg load.RunConfig) {
	fmt.Fprintf(os.Stderr, "robot: %d 个机器人 → %s (tcp %s)，账号服 %s\n",
		cfg.Robots, cfg.Gateway, cfg.GatewayTCP, cfg.AuthAddr)
	fmt.Fprintf(os.Stderr, "       传输 %s，ramp %s，保持 %s\n",
		cfg.Transport, rampText(cfg.RampPerSec), cfg.Duration)
}

func rampText(perSec int) string {
	if perSec <= 0 {
		return "不限（全并发）"
	}
	return strconv.Itoa(perSec) + "/s"
}

// exitFor 把报告映射成退出码。
func exitFor(rep *load.Report) int {
	total := rep.Connect.Attempted
	failed := rep.Connect.Failed + rep.Login.Failed
	switch {
	case failed == 0:
		return ExitOK
	case total > 0 && failed >= total:
		return ExitError // 全军覆没：环境没搭好，不是"部分成功"
	default:
		return ExitPartial
	}
}

// parseSetup 解析 --setup 的 <消息号>:<JSON> 形式。
func parseSetup(items []string) ([]load.SetupStep, error) {
	out := make([]load.SetupStep, 0, len(items))
	for _, it := range items {
		i := strings.Index(it, ":")
		if i <= 0 {
			return nil, fmt.Errorf("--setup 需要 <消息号>:<JSON> 形式，收到 %q", it)
		}
		id, err := strconv.ParseUint(strings.TrimSpace(it[:i]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("--setup 的消息号不是数字: %q", it[:i])
		}
		body := strings.TrimSpace(it[i+1:])
		if body == "" {
			body = "{}"
		}
		if !json.Valid([]byte(body)) {
			return nil, fmt.Errorf("--setup 的 JSON 不合法: %q", body)
		}
		out = append(out, load.SetupStep{MsgID: uint32(id), Body: body})
	}
	return out, nil
}

// ---------- signup ----------

// cmdSignup 批量注册账号（压测前铺数据）。
func (e *env) cmdSignup(args []string) int {
	fs := newFlagSet("signup")
	d := e.cfg.Load

	prefix := fs.String("prefix", d.AccountPrefix, "账号前缀")
	start := fs.Int("start", d.AccountStart, "起始序号（含）")
	count := fs.Int("count", 10, "注册数量")
	password := fs.String("password", d.Password, "统一密码")
	concurrency := fs.Int("concurrency", d.SignupConcurrency, "并发数")
	assumeYes := fs.Bool("yes", false, "跳过确认（非交互环境必填）")

	if err := fs.Parse(args); err != nil {
		return e.usageErr(err.Error())
	}
	if *count <= 0 {
		return e.usageErr("--count 必须大于 0")
	}
	if *start <= 0 {
		return e.usageErr("--start 必须大于 0")
	}
	if *concurrency <= 0 {
		return e.usageErr("--concurrency 必须大于 0")
	}
	if strings.TrimSpace(*password) == "" {
		return e.usageErr("--password 不能为空")
	}

	ok, code := e.askConfirm(fmt.Sprintf("将向账号服注册 %d 个账号（%s%d … %s%d），确认继续？",
		*count, *prefix, *start, *prefix, *start+*count-1), *assumeYes)
	if !ok {
		return code
	}

	auth := authclient.New(e.cfg.AuthAddr)
	res := signupBatch(e.ctx, auth, *prefix, *start, *count, *password, *concurrency)

	e.emit(res, res.text())
	return res.ExitCode()
}

// signupResult 批量注册结果。
type signupResult struct {
	OK       bool             `json:"ok"`
	AuthAddr string           `json:"auth_addr"`
	Prefix   string           `json:"prefix"`
	Start    int              `json:"start"`
	Count    int              `json:"count"`
	Created  int              `json:"created"`
	Existed  int              `json:"existed"`
	Failed   int              `json:"failed"`
	Errors   map[string]int64 `json:"errors,omitempty"`
	// FailedAccounts 前若干条失败账号（便于按账号排查）。
	FailedAccounts []string `json:"failed_accounts,omitempty"`
}

// ExitCode 部分失败返回 ExitPartial，全失败返回 ExitError。
func (r *signupResult) ExitCode() int {
	switch {
	case r.Failed == 0:
		return ExitOK
	case r.Created+r.Existed == 0:
		return ExitError
	default:
		return ExitPartial
	}
}

func (r *signupResult) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n注册完成：新建 %d / 已存在 %d / 失败 %d（共 %d，账号服 %s）\n",
		r.Created, r.Existed, r.Failed, r.Count, r.AuthAddr)
	if len(r.Errors) > 0 {
		b.WriteString("失败原因：\n")
		for _, k := range sortedKeys(r.Errors) {
			fmt.Fprintf(&b, "  %-40s %d\n", k, r.Errors[k])
		}
	}
	return b.String()
}

// signupBatch 并发注册 count 个账号。
//
// "已存在"与"失败"分开计数：注册已存在的账号会返回 success=false，
// 但这属于**预期结果**（重复铺数据不该报错），不能和真正的失败混在一起。
func signupBatch(ctx context.Context, auth *authclient.Client,
	prefix string, start, count int, password string, concurrency int) *signupResult {

	res := &signupResult{
		AuthAddr: auth.BaseURL(),
		Prefix:   prefix,
		Start:    start,
		Count:    count,
		Errors:   make(map[string]int64),
	}

	type job struct{ idx int }
	jobs := make(chan job)
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				account := prefix + strconv.Itoa(start+j.idx)
				r, err := auth.Signup(account, password)
				mu.Lock()
				switch {
				case err != nil:
					res.Failed++
					res.Errors[shortErr(err.Error())]++
					if len(res.FailedAccounts) < 20 {
						res.FailedAccounts = append(res.FailedAccounts, account)
					}
				case r.Success:
					res.Created++
				case isAlreadyExists(r.Err):
					res.Existed++
				default:
					res.Failed++
					res.Errors[shortErr(r.Err)]++
					if len(res.FailedAccounts) < 20 {
						res.FailedAccounts = append(res.FailedAccounts, account)
					}
				}
				mu.Unlock()
			}
		}()
	}

	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			res.OK = res.Failed == 0
			return res
		case jobs <- job{idx: i}:
		}
	}
	close(jobs)
	wg.Wait()

	res.OK = res.Failed == 0
	return res
}

// isAlreadyExists 判断账号服返回的是不是"账号已存在"。
//
// 按**文案**判断是不得已：账号服的注册失败与登录失败共用 {success,err} 结构，
// 没有机器可读的原因码。这里只做归一化匹配，匹配不上就归到"失败"——
// 宁可多报一个失败，也不要把真实故障悄悄算成"已存在"。
func isAlreadyExists(errText string) bool {
	s := strings.ToLower(errText)
	for _, kw := range []string{"exist", "已存在", "已注册", "duplicate", "重复"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// shortErr 把错误文案压短，便于聚合成"错误归类"。
func shortErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

// sortedKeys 稳定输出 map 的键，避免报告每次顺序不同。
func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------- doctor ----------

// doctorResult 自检结果。
type doctorResult struct {
	OK      bool         `json:"ok"`
	Gateway *probeResult `json:"gateway_quic"`
	TCP     *probeResult `json:"gateway_tcp"`
	Auth    *probeResult `json:"auth"`
}

// probeResult 单个探测点的结果。
type probeResult struct {
	Addr      string `json:"addr"`
	Reachable bool   `json:"reachable"`
	LatencyMS int64  `json:"latency_ms"`
	Detail    string `json:"detail,omitempty"`
}

// cmdDoctor 连通性自检：一条命令看清"压测目标是否就绪"。
func (e *env) cmdDoctor(args []string) int {
	fs := newFlagSet("doctor")
	if err := fs.Parse(args); err != nil {
		return e.usageErr(err.Error())
	}

	res := &doctorResult{
		Gateway: probeQUIC(e.cfg.Gateway, e.cfg.ProbeTimeout()),
		TCP:     probeTCP(e.cfg.GatewayTCP),
		Auth:    probeAuth(e.cfg.AuthAddr),
	}
	res.OK = res.Gateway.Reachable && res.TCP.Reachable && res.Auth.Reachable

	e.emit(res, res.text())

	if res.OK {
		return ExitOK
	}
	// 与 manager 的 doctor 同义：有探测点不通 → 部分成功(3)，
	// 让脚本能区分"全挂"和"挂了一部分"。
	return ExitPartial
}

func (r *doctorResult) text() string {
	var b strings.Builder
	b.WriteString("\n连通性自检\n")
	write := func(name string, p *probeResult) {
		mark := "OK  "
		if !p.Reachable {
			mark = "FAIL"
		}
		line := fmt.Sprintf("  %-4s %-14s %-24s %dms", mark, name, p.Addr, p.LatencyMS)
		if p.Detail != "" {
			line += "  " + p.Detail
		}
		b.WriteString(line + "\n")
	}
	write("QUIC", r.Gateway)
	write("TCP", r.TCP)
	write("账号服", r.Auth)
	if !r.OK {
		b.WriteString("\n有探测点不可达。常见原因：\n")
		b.WriteString("  · 网关未启动，或端口与 config.yaml 不一致\n")
		b.WriteString("  · QUIC 不通但 TCP 通 —— 用 --transport tcp 仍可压测\n")
		b.WriteString("  · 账号服未启动：登录链路必然全失败（游戏服只在启动时不依赖它）\n")
	}
	return b.String()
}

// probeQUIC 用真实 QUIC 握手探测网关 UDP 入口。
func probeQUIC(addr string, timeout time.Duration) *probeResult {
	r := &probeResult{Addr: addr}
	if addr == "" {
		r.Detail = "未配置"
		return r
	}
	t0 := time.Now()
	cl, err := client.Dial(client.Options{Gateway: addr, Transport: client.TransportQUIC, ProbeTimeout: timeout})
	r.LatencyMS = time.Since(t0).Milliseconds()
	if err != nil {
		r.Detail = shortErr(err.Error())
		return r
	}
	_ = cl.Close()
	r.Reachable = true
	return r
}

// probeTCP 探测网关 TCP 入口。
func probeTCP(addr string) *probeResult {
	r := &probeResult{Addr: addr}
	if addr == "" {
		r.Detail = "未配置"
		return r
	}
	t0 := time.Now()
	cl, err := client.Dial(client.Options{GatewayTCP: addr, Transport: client.TransportTCP, DialTimeout: 3 * time.Second})
	r.LatencyMS = time.Since(t0).Milliseconds()
	if err != nil {
		r.Detail = shortErr(err.Error())
		return r
	}
	_ = cl.Close()
	r.Reachable = true
	return r
}

// probeAuth 探测账号服 /auth/health。
func probeAuth(addr string) *probeResult {
	r := &probeResult{Addr: addr}
	if addr == "" {
		r.Detail = "未配置"
		return r
	}
	t0 := time.Now()
	h, err := authclient.New(addr).Health()
	r.LatencyMS = time.Since(t0).Milliseconds()
	if err != nil {
		r.Detail = shortErr(err.Error())
		return r
	}
	r.Reachable = true
	r.Detail = "service=" + h.Service
	return r
}

// ---------- progress ----------

// progress 人类模式下的进度显示，**写 stderr**。
//
// 走 stderr 是刻意的：stdout 要留给最终报告（--json 时是那一个 JSON 对象），
// 进度混进去会让 `robot run --json | jq` 直接解析失败。
type progress struct {
	mu   sync.Mutex
	last time.Time
}

func newProgress() *progress { return &progress{} }

func (p *progress) update(done, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 限流到 5 次/秒：几百个机器人时每次结束都打会把终端刷爆。
	if done < total && time.Since(p.last) < 200*time.Millisecond {
		return
	}
	p.last = time.Now()
	fmt.Fprintf(os.Stderr, "\r[进度] 已结束 %d/%d", done, total)
}

func (p *progress) finish() {
	fmt.Fprintln(os.Stderr)
}
