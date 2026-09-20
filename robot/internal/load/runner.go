package load

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/qw576483/clover-server-tools/robot/internal/authclient"
	"github.com/qw576483/clover-server-tools/robot/internal/client"
)

// maxFailures 报告里保留的失败明细条数上限。
// 1000 个机器人全挂时，明细本身没有信息量（看错误归类就够了），
// 但把 1000 条塞进 JSON 会让报告难读也难传。
const maxFailures = 50

// RunConfig 一次压测的全部参数（已由 cli 层把配置文件与命令行合并完毕）。
type RunConfig struct {
	Robots        int
	AccountPrefix string
	AccountStart  int
	Password      string

	Gateway    string
	GatewayTCP string
	AuthAddr   string
	Transport  client.Transport

	// RampPerSec 每秒启动多少个机器人；0 = 不限（全并发，即"惊群"）。
	RampPerSec int

	// Duration 登录成功后的保持在线时长；0 = 登录成功即退出（纯登录压测）。
	Duration time.Duration

	MsgID       uint32
	MsgBody     string
	MsgInterval time.Duration
	MsgOnce     bool
	Unreliable  bool

	Signup          bool
	RequireFullSync bool
	Setup           []SetupStep

	DialTimeout  time.Duration
	ProbeTimeout time.Duration
	LoginTimeout time.Duration
}

// Account 返回第 i 个机器人（0-based）使用的账号。
func (c RunConfig) Account(i int) string {
	return c.AccountPrefix + strconv.Itoa(c.AccountStart+i)
}

// RunMeta 本次运行的元信息。
type RunMeta struct {
	Robots      int    `json:"robots"`
	Transport   string `json:"transport"`
	Gateway     string `json:"gateway"`
	GatewayTCP  string `json:"gateway_tcp"`
	AuthAddr    string `json:"auth_addr"`
	Duration    string `json:"duration"`
	Hold        bool   `json:"hold"`
	RampPerSec  int    `json:"ramp_per_sec"`
	MsgID       uint32 `json:"msg_id,omitempty"`
	MsgInterval string `json:"msg_interval,omitempty"`
	Unreliable  bool   `json:"unreliable,omitempty"`
	MsgOnce     bool   `json:"msg_once,omitempty"`

	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	WallMS     int64  `json:"wall_ms"`
	// Deadline 全局截止时刻。所有机器人共用同一个窗口，
	// 因此"全部上线之后"有一段稳定的并发期 —— 这是压测要测的东西。
	Deadline string `json:"deadline,omitempty"`
}

// ConnectStat 连接阶段的统计。
type ConnectStat struct {
	Attempted int64    `json:"attempted"`
	OK        int64    `json:"ok"`
	Failed    int64    `json:"failed"`
	LatencyMS DistSnap `json:"latency_ms"`
	// ByTransport 实际落到各传输层的机器人数（auto 模式下能看出 QUIC 有没有生效）。
	ByTransport map[string]int64 `json:"by_transport,omitempty"`
}

// LoginStat 登录阶段的统计。
//
// 登录由两段组成，耗时**分开**统计：混在一起就分不清是账号服慢还是游戏服慢。
type LoginStat struct {
	// Attempted 走完连接、有资格登录的机器人数（= ConnectStat.OK）。
	Attempted int64 `json:"attempted"`
	OK        int64 `json:"ok"`
	// Failed 含「账号服换 token 失败」与「长连接登录失败」两种，见 failures[].stage。
	Failed      int64 `json:"failed"`
	SignupTried int64 `json:"signup_tried"`
	// ServerClosed 登录阶段「连接被服务端主动关闭」的机器人数。
	// 不为 0 时优先怀疑网关的连接级准入（MaxConnsPerSec / QueueCap），
	// 而不是登录链路 —— 调小 --ramp 可立刻验证。
	ServerClosed int64 `json:"server_closed"`
	// LatencyMS 长连接 EMsgLogin 发出 → 登录回包的往返。
	LatencyMS DistSnap `json:"latency_ms"`
	// AuthHTTPMS 账号服 HTTP 换 token 那一段。
	AuthHTTPMS DistSnap `json:"auth_http_ms"`
}

// SessionStat 在线阶段的统计。
type SessionStat struct {
	// FullSync 收到 EPushPlayerFullSync 的机器人数。
	// **不参与成败判定**：账号没有角色时引擎本就不推（见 RobotConfig.RequireFullSync）。
	FullSync    int64    `json:"full_sync"`
	Disconnects int64    `json:"disconnects"`
	OnlineMS    DistSnap `json:"online_ms"`
}

// TrafficStat 流量统计。
type TrafficStat struct {
	Sent       int64    `json:"sent"`
	Recv       int64    `json:"recv"`
	SendFailed int64    `json:"send_failed"`
	Unmatched  int64    `json:"unmatched_reply"`
	PendingOvf int64    `json:"pending_overflow"`
	Dropped    int64    `json:"dropped"`
	PerSec     float64  `json:"msg_per_sec"`
	RTTMS      DistSnap `json:"rtt_ms"`
}

// Failure 单个机器人的失败明细。
type Failure struct {
	Index   int    `json:"index"`
	Account string `json:"account"`
	Stage   string `json:"stage"` // connect | auth | login
	Error   string `json:"error"`
}

// Report 一次压测的结果（--json 时原样输出到 stdout）。
type Report struct {
	OK       bool             `json:"ok"`
	Run      RunMeta          `json:"run"`
	Connect  ConnectStat      `json:"connection"`
	Login    LoginStat        `json:"login"`
	Session  SessionStat      `json:"session"`
	Traffic  TrafficStat      `json:"traffic"`
	Errors   map[string]int64 `json:"errors,omitempty"`
	Failures []Failure        `json:"failures,omitempty"`
	// FailuresTruncated 为 true 表示失败明细被截断（只看 Errors 归类即可）。
	FailuresTruncated bool `json:"failures_truncated,omitempty"`
}

// Online 返回压测结束时仍成功登录的机器人数。
func (r *Report) Online() int64 { return r.Login.OK }

// Run 执行一次压测并汇总报告。
//
// onDone 在每台机器人结束时回调一次（已结束数, 总数），用于打进度；
// 传 nil 表示不需要进度。
func Run(ctx context.Context, cfg RunConfig, onDone func(done, total int)) *Report {
	started := time.Now()

	// 全局截止时刻 = 启动耗时 + 保持时长。
	//
	// 为什么把它算进来：ramp 会持续一段时间，若截止时刻从"开跑"起算，
	// 排在后面的机器人在线时间会被 ramp 吃掉，压测窗口名不副实。
	// 加上 ramp 之后，所有机器人都能在"全部上线"之后拥有完整的 duration。
	rampSpan := time.Duration(0)
	if cfg.RampPerSec > 0 && cfg.Robots > 1 {
		rampSpan = time.Duration(float64(cfg.Robots-1) / float64(cfg.RampPerSec) * float64(time.Second))
	}
	deadline := started.Add(rampSpan + cfg.Duration)

	// 所有机器人共用一个账号服客户端：HTTP 连接池已在 authclient.New 里调大。
	auth := authclient.New(cfg.AuthAddr)

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		all []*Stats
	)

launch:
	for i := 0; i < cfg.Robots; i++ {
		// 限速启动：把 i 换算成"应当在第几秒启动"，到点再 go。
		if cfg.RampPerSec > 0 && i > 0 {
			at := started.Add(time.Duration(float64(i) / float64(cfg.RampPerSec) * float64(time.Second)))
			if d := time.Until(at); d > 0 {
				select {
				case <-ctx.Done():
					break launch // 被提前取消：后面还没启动的机器人不再启动
				case <-time.After(d):
				}
			}
		}

		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			st := RunRobot(ctx, RobotConfig{
				Index:           idx,
				Account:         cfg.Account(idx),
				Password:        cfg.Password,
				Gateway:         cfg.Gateway,
				GatewayTCP:      cfg.GatewayTCP,
				AuthAddr:        cfg.AuthAddr,
				Transport:       cfg.Transport,
				DialTimeout:     cfg.DialTimeout,
				ProbeTimeout:    cfg.ProbeTimeout,
				LoginTimeout:    cfg.LoginTimeout,
				Deadline:        deadline,
				Hold:            cfg.Duration > 0,
				RequireFullSync: cfg.RequireFullSync,
				Signup:          cfg.Signup,
				MsgID:           cfg.MsgID,
				MsgBody:         cfg.MsgBody,
				MsgInterval:     cfg.MsgInterval,
				MsgOnce:         cfg.MsgOnce,
				Unreliable:      cfg.Unreliable,
				Setup:           cfg.Setup,
			}, auth)

			mu.Lock()
			all = append(all, st)
			done := len(all)
			mu.Unlock()
			if onDone != nil {
				onDone(done, cfg.Robots)
			}
		}(i)
	}

	wg.Wait()
	finished := time.Now()

	return summarize(cfg, all, started, finished, deadline)
}

// summarize 把各机器人的统计汇总成报告。
func summarize(cfg RunConfig, all []*Stats, started, finished time.Time, deadline time.Time) *Report {
	rep := &Report{
		OK: true,
		Run: RunMeta{
			Robots:     cfg.Robots,
			Transport:  cfg.Transport.String(),
			Gateway:    cfg.Gateway,
			GatewayTCP: cfg.GatewayTCP,
			AuthAddr:   cfg.AuthAddr,
			Duration:   cfg.Duration.String(),
			Hold:       cfg.Duration > 0,
			RampPerSec: cfg.RampPerSec,
			MsgID:      cfg.MsgID,
			Unreliable: cfg.Unreliable,
			MsgOnce:    cfg.MsgOnce,
			StartedAt:  started.Format(time.RFC3339Nano),
			FinishedAt: finished.Format(time.RFC3339Nano),
			WallMS:     finished.Sub(started).Milliseconds(),
		},
		Errors:  make(map[string]int64),
		Connect: ConnectStat{ByTransport: make(map[string]int64)},
	}

	if cfg.Duration > 0 {
		rep.Run.Deadline = deadline.Format(time.RFC3339Nano)
		rep.Run.MsgInterval = cfg.MsgInterval.String()
	}

	var (
		connect      Dist
		loginLat     Dist
		authHTTP     Dist
		onlineMS     Dist
		rtt          Dist
		sent         int64
		recv         int64
		sendFail     int64
		unmatched    int64
		pendingOvf   int64
		dropped      int64
		signupTried  int64
		disconnects  int64
		fullSync     int64
		failures     []Failure
		failuresMore bool
	)

	for _, s := range all {
		if s == nil {
			continue
		}

		// ---- 连接 ----
		rep.Connect.Attempted++
		if s.ConnectOK {
			rep.Connect.OK++
			connect.Add(s.ConnectMS)
			if s.Transport != "" {
				rep.Connect.ByTransport[s.Transport]++
			}
		} else {
			rep.Connect.Failed++
		}

		// ---- 登录（只有连上的机器人才走得到这一步）----
		if s.ConnectOK {
			rep.Login.Attempted++
			authHTTP.Add(s.AuthHTTPMS)
			if s.SignupTried {
				signupTried++
			}
			if s.LoginOK {
				rep.Login.OK++
				loginLat.Add(s.LoginMS)
			} else {
				rep.Login.Failed++
				if s.ServerClosed {
					rep.Login.ServerClosed++
				}
			}
			// 注意：连上的机器人必定走过 auth 阶段（哪怕它失败了），
			// 所以 auth_http_ms 的样本数与 login.attempted 对齐。
		}

		// ---- 在线 ----
		if s.FullSync {
			fullSync++
		}
		disconnects += s.Disconnects
		if s.OnlineMS > 0 {
			onlineMS.Add(s.OnlineMS)
		}

		// ---- 流量 ----
		sent += s.Sent
		recv += s.Recv
		sendFail += s.SendFail
		unmatched += s.Unmatched
		pendingOvf += s.PendingOvf
		dropped += s.Dropped
		rtt.Merge(&s.RTT)

		// ---- 错误归类 ----
		for k, v := range s.ErrKinds {
			rep.Errors[k] += v
		}

		// ---- 失败明细 ----
		if stage := s.FailStage(); stage != "" {
			if len(failures) < maxFailures {
				failures = append(failures, Failure{
					Index:   s.Index,
					Account: s.Account,
					Stage:   stage,
					Error:   s.FailReason(),
				})
			} else {
				failuresMore = true
			}
		}
	}

	rep.Connect.LatencyMS = connect.Snapshot()
	rep.Login.LatencyMS = loginLat.Snapshot()
	rep.Login.AuthHTTPMS = authHTTP.Snapshot()
	rep.Login.SignupTried = signupTried
	rep.Session = SessionStat{
		FullSync:    fullSync,
		Disconnects: disconnects,
		OnlineMS:    onlineMS.Snapshot(),
	}
	rep.Traffic = TrafficStat{
		Sent:       sent,
		Recv:       recv,
		SendFailed: sendFail,
		Unmatched:  unmatched,
		PendingOvf: pendingOvf,
		Dropped:    dropped,
		RTTMS:      rtt.Snapshot(),
	}
	if wall := finished.Sub(started).Seconds(); wall > 0 {
		rep.Traffic.PerSec = float64(sent) / wall
	}

	// 失败明细按序号排序，方便对着账号列表看。
	sort.Slice(failures, func(i, j int) bool { return failures[i].Index < failures[j].Index })
	if len(failures) > 0 {
		rep.Failures = failures
		rep.FailuresTruncated = failuresMore
	}
	if len(rep.Errors) == 0 {
		rep.Errors = nil
	}

	rep.OK = rep.Connect.Failed == 0 && rep.Login.Failed == 0
	return rep
}
