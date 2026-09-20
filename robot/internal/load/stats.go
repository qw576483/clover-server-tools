package load

import "time"

// Stats 单个机器人的运行统计。
//
// **由该机器人自己的 goroutine 独占写入**，因此字段全部无锁。
// 跨机器人的汇总在 runner 侧完成（见 Summary）。
//
// 不返回 error 给上层的原因：压测里「有几个失败、失败在哪个阶段」比
// 「第一个失败是什么」重要得多，所以失败信息一律记进这里。
type Stats struct {
	Index   int
	Account string

	// ---- 阶段一：建立长连接 ----
	ConnectOK  bool
	ConnectErr string
	ConnectMS  int64 // 拨号耗时
	Transport  string

	// ---- 阶段二：账号服 HTTP 换 token ----
	AuthTried   bool
	AuthErr     string
	AuthHTTPMS  int64 // 账号服那一段的耗时
	SignupTried bool

	// ---- 阶段三：长连接登录 ----
	LoginOK  bool
	LoginErr string
	LoginMS  int64 // EMsgLogin 发出 → 登录回包 的往返
	Owner    string
	// ServerClosed 标记登录阶段「连接被服务端主动关闭」。
	// 这类失败最常见的原因是网关的连接级准入（每秒新建连接数超限 / 队列满），
	// 报告据此给出排查提示，避免被误读成登录链路故障。
	ServerClosed bool

	// ---- 阶段四：在线 ----
	FullSync    bool
	PlayerID    string
	OnlineMS    int64
	Disconnects int64

	// ---- 流量 ----
	Sent       int64
	Recv       int64
	SendFail   int64
	Unmatched  int64 // 收到回包但 requestID 配不上（多为超时后被丢弃的请求）
	PendingOvf int64 // 等待回包表溢出被清理的条数
	Dropped    int64 // 接收通道满而被客户端丢掉的帧数（正常应为 0）

	RTT Dist

	// ErrKinds 失败原因归类计数（"connect: i/o timeout" → n）。
	ErrKinds map[string]int64
}

// newStats 构造初始统计。
func newStats(index int, account string) *Stats {
	return &Stats{
		Index:    index,
		Account:  account,
		RTT:      *NewDist(defaultDistLimit),
		ErrKinds: make(map[string]int64),
	}
}

// Err 记一次失败原因。
func (s *Stats) Err(kind string) {
	if kind == "" {
		return
	}
	s.ErrKinds[kind]++
}

// FailStage 返回该机器人失败发生在哪个阶段；全成功返回空串。
// 报告里的 failures 明细用它标注 stage，便于一眼看出是连不上还是登录不过。
func (s *Stats) FailStage() string {
	switch {
	case !s.ConnectOK:
		return "connect"
	case !s.LoginOK && s.AuthErr != "":
		return "auth"
	case !s.LoginOK:
		return "login"
	default:
		return ""
	}
}

// FailReason 返回该机器人的失败原因；全成功返回空串。
func (s *Stats) FailReason() string {
	switch s.FailStage() {
	case "connect":
		return s.ConnectErr
	case "auth":
		return s.AuthErr
	case "login":
		return s.LoginErr
	default:
		return ""
	}
}

// markOnline 记录「进入在线态」的时刻，并返回一个结算函数。
//
// 这样写是为了让在线时长的结算只有一处：无论机器人是从 deadline 退出、
// 从 ctx 取消退出，还是被断线踢出，都走同一个结算点，不会漏记或重复记。
func (s *Stats) markOnline(now time.Time) func() {
	start := now
	done := false
	return func() {
		if done {
			return
		}
		done = true
		if d := time.Since(start); d > 0 {
			s.OnlineMS = d.Milliseconds()
		}
	}
}
