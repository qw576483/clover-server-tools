package load

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/qw576483/clover-server-tools/robot/internal/authclient"
	"github.com/qw576483/clover-server-tools/robot/internal/client"
)

// maxPending 是「已发出、等回包」的请求数上限。
//
// 对端若从不回包（例如发的业务消息号没有注册 handler），不设上限的话这张表
// 会随发送数无限增长。压测跑几十分钟就能吃光内存。
const maxPending = 2048

// SetupStep 登录成功后按顺序发一次的消息（如「创建角色」）。
type SetupStep struct {
	MsgID uint32
	Body  string
}

// RobotConfig 单个机器人的运行参数。
type RobotConfig struct {
	Index    int    // 0-based 序号，用于账号编号与 {i} 占位
	Account  string // 账号
	Password string // 密码

	Gateway      string
	GatewayTCP   string
	AuthAddr     string
	Transport    client.Transport
	DialTimeout  time.Duration
	ProbeTimeout time.Duration

	// LoginTimeout 「连上 → 登录完成」的总超时。
	LoginTimeout time.Duration

	// Deadline 全局压测截止时刻。所有机器人共用同一个窗口，
	// 保证「全部上线之后」有一段稳定的并发期 —— 这才是压测要测的东西。
	Deadline time.Time

	// Hold 为 true：登录后保持在线到 Deadline。
	// 为 false（duration=0）：登录成功即退出（纯登录压测）。
	Hold bool

	// RequireFullSync 要求必须收到 EPushPlayerFullSync 才算成功。
	//
	// 默认 false，因为**账号没有角色时引擎本就不推全量同步**：
	// 「进游戏服（登录成功且已有角色）或创建角色成功后」才推。
	// 把没有角色当成登录失败，会让一次正常的登录压测全红。
	// 需要验证完整链路（登录 + 建角 + 进游戏）时再打开它。
	RequireFullSync bool

	// Signup 登录失败（账号不存在）时自动去账号服注册。
	Signup bool

	// 登录成功后周期发送的业务消息（MsgID=0 表示不发）。
	MsgID       uint32
	MsgBody     string // 支持 {i} 占位，替换为机器人序号
	MsgInterval time.Duration
	MsgOnce     bool // 只发一次
	Unreliable  bool // 走不可靠通道（QUIC Datagram / 裸 UDP）

	// Setup 登录成功后按顺序发一次的初始化消息。
	Setup []SetupStep
}

// bot 单个机器人的运行态。
type bot struct {
	cfg  RobotConfig
	st   *Stats
	auth *authclient.Client

	cl         *client.Client
	pending    map[uint32]time.Time
	loginReqID uint32
	token      string
	onlineAt   time.Time

	// loginReply 是登录回包的解析结果：handleReply 写、finishLogin 读。
	// 用字段中转而不是直接返回，是因为 handleFrame 还要兼顾「不是登录回包」的
	// 那些帧，返回值只承担「是不是登录回包」这一个语义。
	loginReply *loginReply
}

// RunRobot 跑完一个机器人的完整生命周期并返回统计。
//
// 不返回 error：失败信息记在 Stats 里，由 runner 汇总成「哪个阶段失败了多少个」。
func RunRobot(ctx context.Context, cfg RobotConfig, auth *authclient.Client) *Stats {
	b := &bot{
		cfg:     cfg,
		st:      newStats(cfg.Index, cfg.Account),
		auth:    auth,
		pending: make(map[uint32]time.Time),
	}
	b.run(ctx)

	// 收尾取一次丢帧计数：它不是运行中的实时量，而是"这次连接一共丢了多少"。
	// 正常压测里应当恒为 0；不为 0 说明本工具的消费端跟不上，报告的 RTT 会偏低。
	if b.cl != nil {
		b.st.Dropped = int64(b.cl.Dropped())
	}
	return b.st
}

// run 按四个阶段推进：连上 → 换 token → 登录 → 在线。
// 每阶段失败即返回，统计里带上阶段信息。
func (b *bot) run(ctx context.Context) {
	if !b.connect() {
		return
	}
	defer b.cl.Close()

	if !b.fetchToken() {
		return
	}

	if !b.login(ctx) {
		return
	}

	b.serve(ctx)
}

// connect 阶段一：建立到网关的长连接。
func (b *bot) connect() bool {
	t0 := time.Now()
	cl, err := client.Dial(client.Options{
		Gateway:      b.cfg.Gateway,
		GatewayTCP:   b.cfg.GatewayTCP,
		Transport:    b.cfg.Transport,
		DialTimeout:  b.cfg.DialTimeout,
		ProbeTimeout: b.cfg.ProbeTimeout,
	})
	b.st.ConnectMS = msSince(t0)
	if err != nil {
		b.st.ConnectErr = "connect: " + err.Error()
		b.st.Err(b.st.ConnectErr)
		return false
	}
	b.cl = cl
	b.st.ConnectOK = true
	b.st.Transport = cl.TransportType().String()
	return true
}

// fetchToken 阶段二：向账号服 HTTP 换 JWT。
//
// 账号密码只发给账号服，永不进入游戏长连接 —— 这是引擎唯一的登录模式，
// 游戏服不接收账号密码，也没有注册入口。
func (b *bot) fetchToken() bool {
	b.st.AuthTried = true

	t0 := time.Now()
	res, err := b.auth.Login(b.cfg.Account, b.cfg.Password)
	b.st.AuthHTTPMS = msSince(t0)
	if err != nil {
		b.st.AuthErr = "auth: " + err.Error()
		b.st.Err(b.st.AuthErr)
		return false
	}
	if res.Success && res.Token != "" {
		b.token = res.Token
		return true
	}

	// 密码错 / 账号不存在：只有后者能靠注册救回来，但账号服的响应不区分，
	// 所以 --signup 一律先试着注册一次（注册成功即签发 token，也顺带完成铺数据）。
	if !b.cfg.Signup {
		b.st.AuthErr = "auth: " + nonEmpty(res.Err, "success=false")
		b.st.Err(b.st.AuthErr)
		return false
	}

	b.st.SignupTried = true
	t1 := time.Now()
	sr, serr := b.auth.Signup(b.cfg.Account, b.cfg.Password)
	b.st.AuthHTTPMS += msSince(t1)
	if serr != nil {
		b.st.AuthErr = "signup: " + serr.Error()
		b.st.Err(b.st.AuthErr)
		return false
	}
	if !sr.Success || sr.Token == "" {
		b.st.AuthErr = "signup: " + nonEmpty(sr.Err, "success=false")
		b.st.Err(b.st.AuthErr)
		return false
	}
	b.token = sr.Token
	return true
}

// login 阶段三：长连接发 EMsgLogin{token}，等登录回包。
//
// 成功的判定是「回包 success=true」而**不是** EPushPlayerFullSync：
// 回包 success 意味着账号已通过验证、网关会话已绑定 owner（门禁随之放行）；
// 全量同步是否下发取决于业务（账号有没有角色），那是另一个指标。
func (b *bot) login(ctx context.Context) bool {
	body, err := json.Marshal(map[string]string{"token": b.token})
	if err != nil {
		b.st.LoginErr = "login: 编码登录请求失败: " + err.Error()
		b.st.Err(b.st.LoginErr)
		return false
	}

	t0 := time.Now()
	reqID, err := b.cl.Send(client.EMsgLogin, body)
	if err != nil {
		b.st.LoginErr = "login: 发送失败: " + err.Error()
		b.st.Err(b.st.LoginErr)
		return false
	}
	b.loginReqID = reqID
	b.pending[reqID] = t0

	timer := time.NewTimer(b.cfg.LoginTimeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			b.st.LoginErr = "login: 已取消"
			return false

		case <-timer.C:
			b.st.LoginErr = "login: 超时（" + b.cfg.LoginTimeout.String() + " 内未收到登录回包）"
			b.st.Err(b.st.LoginErr)
			return false

		case inc, ok := <-b.cl.Recv():
			if !ok {
				b.failLogin("连接被服务端关闭（未收到登录回包）", true)
				return false
			}
			if inc.Err != nil {
				b.failLogin(inc.Err.Error(), isServerClose(inc.Err))
				return false
			}
			if !b.handleFrame(inc, t0) {
				continue // 不是登录回包（推送 / 其它回包），继续等
			}
			return b.finishLogin()
		}
	}
}

// failLogin 记录一次登录阶段的失败。
//
// serverClosed 标记「连接是服务端主动关的」——报告据此给出准入限流的提示
// （见 RenderReport 里那段 ⚠）。不计 Disconnects：连接还没进入在线态就断了，
// 那是登录失败，不是会话中断，混在一起会让「断线数」这个指标失去意义。
func (b *bot) failLogin(reason string, serverClosed bool) {
	b.st.LoginErr = "login: " + reason
	if serverClosed {
		b.st.ServerClosed = true
	}
	b.st.Err(b.st.LoginErr)
}

// isServerClose 判断错误是不是「服务端主动关闭了连接」。
//
// 为什么要单独识别：压测里这类失败最常见的原因**不是**登录链路本身，而是
// 网关的连接级准入 —— 每秒新建连接数超限（MaxConnsPerSec）或等候队列已满时，
// 网关在首帧就 Close 掉连接（服务端日志表现为
// gwcore "admission rejected ... (rate/queue)"）。
// 不区分的话，报告里只有一句 "connection reset"，看的人会去翻登录逻辑，白费时间。
//
// 判定只能靠错误文本：连接层把"谁关的"信息压缩成了不同传输层的各自表述
// （QUIC 是 application error，TCP 是 EOF / connection reset），
// 没有统一的错误类型可用。
func isServerClose(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, kw := range []string{
		"application error", // QUIC: 对端用应用错误码关闭
		"connection reset",
		"broken pipe",
		"eof",
		"closed",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// finishLogin 解析已经配对上的登录回包，判定成败。
func (b *bot) finishLogin() bool {
	// 回包内容由 handleFrame 暂存，见那里对 loginReply 的处理。
	rep := b.loginReply
	b.loginReply = nil
	if rep == nil {
		b.st.LoginErr = "login: 回包无法解析"
		b.st.Err(b.st.LoginErr)
		return false
	}
	if rep.isError {
		b.st.LoginErr = "login: 被拒绝 " + rep.errText
		b.st.Err(b.st.LoginErr)
		return false
	}
	if !rep.success {
		b.st.LoginErr = "login: " + nonEmpty(rep.errText, "success=false")
		b.st.Err(b.st.LoginErr)
		return false
	}
	b.st.LoginOK = true
	b.st.Owner = rep.owner
	b.st.LoginMS = rep.rttMS
	return true
}

// serve 阶段四：在线。发初始化消息、按需周期发消息，直到到点 / 断线 / 取消。
func (b *bot) serve(ctx context.Context) {
	b.onlineAt = time.Now()
	settle := b.st.markOnline(b.onlineAt)
	defer settle()

	// 登录后先发一次 setup（如建角）。必须等登录成功后再发：
	// 网关的登录门禁只放行白名单消息，会话绑定 owner 之前发业务消息会被 401 拒掉。
	b.sendSetup()

	// 纯登录压测：登录成功即可收工。
	if !b.cfg.Hold && !b.cfg.RequireFullSync {
		return
	}

	var (
		sendC <-chan time.Time
		sendT *time.Timer
		syncC <-chan time.Time
		syncT *time.Timer
		deadC <-chan time.Time
		deadT *time.Timer
	)

	if b.cfg.Hold && b.cfg.MsgID != 0 {
		sendT = time.NewTimer(b.cfg.MsgInterval)
		sendC = sendT.C
		defer sendT.Stop()
	}
	// 只要求全量同步、不保持在线时，给一个等待窗口；超时不算失败（见 RobotConfig）。
	if !b.cfg.Hold {
		syncT = time.NewTimer(b.cfg.LoginTimeout)
		syncC = syncT.C
		defer syncT.Stop()
	}
	if b.cfg.Hold && !b.cfg.Deadline.IsZero() {
		if d := time.Until(b.cfg.Deadline); d > 0 {
			deadT = time.NewTimer(d)
			deadC = deadT.C
			defer deadT.Stop()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-deadC:
			return

		case <-syncC:
			return

		case <-sendC:
			b.sendOnce()
			if b.cfg.MsgOnce {
				sendC = nil
				continue
			}
			sendT.Reset(b.cfg.MsgInterval)

		case inc, ok := <-b.cl.Recv():
			if !ok {
				b.st.Disconnects++
				return
			}
			if inc.Err != nil {
				b.st.Disconnects++
				b.st.Err("session: " + inc.Err.Error())
				return
			}
			b.handleFrame(inc, time.Time{})
			if !b.cfg.Hold && b.cfg.RequireFullSync && b.st.FullSync {
				return
			}
		}
	}
}

// loginReply 登录回包解析后的中间结果（handleFrame 写、finishLogin 读）。
type loginReply struct {
	success bool
	owner   string
	errText string
	isError bool
	rttMS   int64
}

// handleFrame 处理一帧下发：先记 RTT，再按类型分派。
//
// sentAt 非零时表示当前处于登录阶段，该次回包的时间戳用它（登录请求在
// pending 里也有一条，但这里显式传入是为了拿到精确的往返起点）。
// 返回 true 表示「这就是本次登录的回包」，登录阶段据此结束等待。
func (b *bot) handleFrame(inc client.Incoming, sentAt time.Time) bool {
	b.st.Recv++

	// 回包：正常回包 msgID=0，错误回包 msgID=0xFFFFFFFF，都按 requestID 配对。
	if inc.MsgID == 0 || inc.MsgID == client.EMsgError {
		return b.handleReply(inc, sentAt)
	}

	// 推送。
	switch inc.MsgID {
	case client.EPushPlayerFullSync:
		b.st.FullSync = true
		var fs struct {
			PlayerID     string `json:"player_id"`
			SessionToken string `json:"session_token"`
		}
		if json.Unmarshal(inc.Body, &fs) == nil {
			b.st.PlayerID = fs.PlayerID
		}
	case client.EMsgUDPBindGrant:
		// 网关下发的不可靠通道绑定令牌。本工具**刻意不做** EMsgBindUDP 绑定：
		// 那会给每个机器人多一个常驻 socket + 一个保活协程，N 上去就是纯浪费。
		// QUIC 模式的不可靠上行直接走 Datagram，不需要绑定。
	case client.EPushAlert:
		// 系统弹窗推送：仅计数（Recv 已在上面加过）。
	default:
		// 其它推送（含业务推送）：仅计数。
	}
	return false
}

// handleReply 处理一条回包：配对 requestID、记 RTT，并识别登录回包。
func (b *bot) handleReply(inc client.Incoming, sentAt time.Time) bool {
	if inc.RequestID == 0 {
		b.st.Unmatched++
		return false
	}

	// 往返起点：登录阶段用调用方传入的精确时刻，其余从 pending 表取。
	if sentAt.IsZero() {
		t, ok := b.pending[inc.RequestID]
		if !ok {
			b.st.Unmatched++
			return false
		}
		sentAt = t
	}
	delete(b.pending, inc.RequestID)
	b.st.RTT.Add(msSince(sentAt))

	if inc.RequestID != b.loginReqID || b.loginReqID == 0 {
		return false
	}

	// 这就是登录回包。
	rep := &loginReply{rttMS: msSince(sentAt)}
	if inc.MsgID == client.EMsgError {
		var e client.EErrorReply
		if json.Unmarshal(inc.Body, &e) == nil {
			rep.errText = nonEmpty(e.Err, "code="+strconv.Itoa(int(e.Code)))
		} else {
			rep.errText = "无法解析错误回包"
		}
		rep.isError = true
	} else {
		var lr client.ELoginReply
		if json.Unmarshal(inc.Body, &lr) == nil {
			rep.success = lr.Success
			rep.owner = lr.Owner
			rep.errText = lr.Err
		}
	}
	b.loginReply = rep
	return true
}

// sendSetup 登录成功后按顺序发一次初始化消息。
func (b *bot) sendSetup() {
	for _, step := range b.cfg.Setup {
		body := []byte(b.expand(step.Body))
		reqID, err := b.cl.Send(step.MsgID, body)
		if err != nil {
			b.st.SendFail++
			b.st.Err("setup: " + err.Error())
			continue
		}
		b.st.Sent++
		b.remember(reqID)
	}
}

// sendOnce 发一条周期消息。
func (b *bot) sendOnce() {
	body := []byte(b.expand(b.cfg.MsgBody))

	if b.cfg.Unreliable {
		if err := b.cl.SendUnreliable(b.cfg.MsgID, body); err != nil {
			b.st.SendFail++
			b.st.Err("send: " + err.Error())
			return
		}
		b.st.Sent++
		return
	}

	reqID, err := b.cl.Send(b.cfg.MsgID, body)
	if err != nil {
		b.st.SendFail++
		b.st.Err("send: " + err.Error())
		return
	}
	b.st.Sent++
	b.remember(reqID)
}

// remember 登记一条待回包请求，必要时先给表封顶。
func (b *bot) remember(reqID uint32) {
	if len(b.pending) >= maxPending {
		b.evictPending()
	}
	b.pending[reqID] = time.Now()
}

// evictPending 在等待表满时清掉约一半。
// map 迭代序随机，不追求精确淘汰最早的那批 —— 它的作用只是给内存封顶；
// 真正有诊断价值的是 st.PendingOvf 这个计数（不为 0 说明对端大量不回包）。
func (b *bot) evictPending() {
	n := 0
	for k := range b.pending {
		delete(b.pending, k)
		n++
		if n >= maxPending/2 {
			break
		}
	}
	b.st.PendingOvf += int64(n)
}

// expand 把 {i} 替换成机器人序号。
//
// 批量机器人常常要发同一条业务消息（如「创建角色」带 name），彼此必须不同，
// 否则服务端侧会撞唯一键 —— 这类失败在压测报告里会表现成一片 500，
// 很难查，所以在工具层直接提供占位符。
func (b *bot) expand(s string) string {
	if s == "" {
		return "{}"
	}
	if !strings.Contains(s, "{i}") {
		return s
	}
	return strings.ReplaceAll(s, "{i}", strconv.Itoa(b.cfg.Index))
}

// msSince 返回自 t 起经过的毫秒数。
func msSince(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	d := time.Since(t)
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

// nonEmpty 返回 s，为空时返回 fallback。压测报告里"失败但没原因"是最难查的。
func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
