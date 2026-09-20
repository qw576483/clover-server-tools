// Package client 实现 clover 网关命令行调试客户端的连接与线协议。
//
// 与 clover-server-engine/pkg/transport/net（QUIC）、clover-server-engine/pkg/shared/proto 的约定保持一致，但本包
// 直接内联实现，使 msg-client 不依赖 clover-server-engine 整个模块 —— 可独立编译。
//
// 连接流程（原生客户端）：
//  1. 检测网络环境
//  2. UDP 探测（可选）
//  3. QUIC 连接尝试
//  4. TCP 连接尝试（回退）
//
// 传输层帧格式：
//
//	QUIC: 无传输层帧头，直接 [4B requestID][4B msgID][body]
//	TCP:  [1B type][4B len][payload]，其中 payload = [4B requestID][4B msgID][body]
//
// requestID≠0 为请求/回包，=0 为推送。
package client

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"msg-client/internal/util"
	"github.com/quic-go/quic-go"
)

const (
	requestIDLen = 4
	msgIDLen     = 4
	headerLen    = requestIDLen + msgIDLen // 8
	maxMsgSize   = 1 << 20                // 单帧 payload 上限 1MB，防损坏数据
	dialTimeout  = 5 * time.Second
	// 真实 QUIC 握手探测超时：仅用于判定 QUIC 是否可用，必须远短于
	// dialTimeout，避免 QUIC 未启动时白等 5 秒才回退 TCP。
	probeTimeout = 2 * time.Second
)

// TCP 帧类型（与 clover-server-engine/pkg/transport/net/tcp/codec.go 对齐）
const (
	tcpFrameTypeData    byte = 0
	tcpFrameTypePing    byte = 1
	tcpFrameTypePong    byte = 2
	tcpFrameTypeMigrate byte = 3
)

// 裸 UDP 通道路由魔数：与引擎 demux.RawUDPMagic(0x55) 一致。
// 只有 QUIC 不可用（走 TCP 可靠连接）时，不可靠位置数据才经共享 UDP 端口的该魔数发送。
const rawUDPMagic byte = 0x55

// 客户端 TCP 心跳 ping 间隔：远小于服务端读空闲超时（2×30s），
// 挂机不发业务消息时也能通过周期 ping 刷新服务端读 deadline，避免被当成死连接断开。
const tcpHeartbeatInterval = 15 * time.Second

// 常驻裸 UDP 绑定的周期重发间隔：服务端按「最后一帧绑定即最新」登记，
// 周期性重发 EMsgBindUDP 帧（兼 NAT 保活），避免网关长时间不推导致绑定被清理/映射老化。
const udpKeepAlive = 10 * time.Second

// 引擎级内核通用 opcode（来自 clover-server-engine/pkg/shared/proto，全局区间约定）：
//
//	[1,      …] 引擎 C2S：   EMsgLogin=2 / EMsgResumeSession=3（1 号原为 EMsgSignup，已作废保留）
//	[4001,   …] 引擎推送：   EPushPlayerFullSync=4001 / EPushAlert=4002 / EPushDataSync=4003
//	[10001,  …] 业务 C2S：    由 demo 的 def/msg.go 定义（MsgCreatePlayer=10001 …）
//	[20001,  …] 业务回包：    由 demo 的 def/reply.go 定义（MsgCreatePlayerReply=20001 …）
//	[30001,  …] 业务推送：    由 demo 的 def/push.go 定义（MsgSettingsSync=30001 …）
//
// 业务消息号必须 >= InternalMsgMax+1（即 >= 10001）。
// 回包约定：普通回包按 requestID 配对（msgID 恒为 0），错误回包使用特殊值 EMsgError=0xFFFFFFFF。
const (
	// 号位 1 曾用于注册（EMsgSignup）：注册已完全移到账号服 HTTP
	// （signup 命令 → POST {账号服}/auth/signup），游戏服不接收注册报文。
	// 号位作废但保留不复用，与引擎 pkg/shared/proto/msg.go 的说明一致。
	EMsgLogin           uint32 = 2
	EMsgResumeSession   uint32 = 3 // 断线重连后用 session_token 恢复会话（引擎 proto.EMsgResumeSession 对齐）
	EMsgBindUDP         uint32 = 5 // TCP/WS 玩家凭令牌上报常驻裸 UDP 端点（引擎 proto.EMsgBindUDP 对齐）
	EMsgUDPBindGrant    uint32 = 6 // 网关下发的不可靠通道绑定令牌（登录后先收到本帧，再上报 EMsgBindUDP）
	EPushPlayerFullSync uint32 = 4001
	EPushAlert          uint32 = 4002
	EMsgError           uint32 = 0xFFFFFFFF
	InternalMsgMax      uint32 = 10000
)

// 客户端 → 逻辑服登录请求（与引擎 proto.ELoginRequest 对齐）。
// 只有 token：账号密码只发给账号服，永不进入游戏长连接。
type ELoginRequest struct {
	Token string `json:"token"`
}

// 逻辑服 → 客户端登录回包（与引擎 proto.ELoginReply 对齐）。
type ELoginReply struct {
	Owner   string `json:"owner"`
	Token   string `json:"token,omitempty"`
	Success bool   `json:"success,omitempty"`
	Err     string `json:"err,omitempty"`
}

// 通用错误回包（与引擎 proto.EErrorReply 对齐）。
// Code 是机器可读错误码（400/401/403/404/429/500）：网关登录门禁拒绝回 401、
// 逻辑服业务钩子拒绝回 403。此前这里漏了该字段，工具只能看文案、无法按码分支。
type EErrorReply struct {
	Err  string `json:"err"`
	Code int32  `json:"code,omitempty"`
}

// 封装客户端 ↔ 网关帧：4B requestID + 4B msgID + body。
func EncodeClientFrame(requestID, msgID uint32, body []byte) []byte {
	buf := make([]byte, headerLen+len(body))
	binary.BigEndian.PutUint32(buf[:requestIDLen], requestID)
	binary.BigEndian.PutUint32(buf[requestIDLen:requestIDLen+msgIDLen], msgID)
	copy(buf[headerLen:], body)
	return buf
}

// 解析客户端 ↔ 网关帧。
func DecodeClientFrame(p []byte) (requestID, msgID uint32, body []byte, err error) {
	if len(p) < headerLen {
		return 0, 0, nil, errors.New("proto: frame too short")
	}
	requestID = binary.BigEndian.Uint32(p[:requestIDLen])
	msgID = binary.BigEndian.Uint32(p[requestIDLen : requestIDLen+msgIDLen])
	body = make([]byte, len(p)-headerLen)
	copy(body, p[headerLen:])
	return requestID, msgID, body, nil
}

// 一条从服务端收到的消息（已解开传输层帧与客户端帧）。
type Incoming struct {
	RequestID uint32 // 请求关联 ID（回包非 0，推送为 0）
	MsgID     uint32
	Body      []byte
	Err       error // 非 nil 表示连接已断开或解析出错
}

// 传输层类型
type Transport int

const (
	TransportQUIC Transport = iota
	TransportTCP
)

// 返回传输层名称
func (t Transport) String() string {
	switch t {
	case TransportQUIC:
		return "QUIC"
	case TransportTCP:
		return "TCP"
	default:
		return "unknown"
	}
}

// 一条到网关的连接。
type Client struct {
	// QUIC 相关
	quicConn quic.Connection
	stream   quic.Stream
	// TCP 相关
	conn net.Conn
	// 共享 UDP 端口（QUIC 端口）：QUIC 不可用、走 TCP 可靠连接时，
	// 不可靠位置数据回退裸 UDP（0x55 魔数）发往该地址。
	udpAddr string
	// 常驻裸 UDP socket（仅 TCP 可靠连接模式，登录成功后 BindUDP 建立）：
	//   - 上行：send_pos 复用该 socket，不再每次新建（用后即关会导致服务端无法回推）；
	//   - 下行：后台读协程（readLoopUDP）接收网关经 0x55 魔数下发的不可靠推送。
	udpConn net.Conn
	// 本常驻 UDP 通道的绑定令牌（网关 EMsgUDPBindGrant 下发），用于 EMsgBindUDP 上报帧。
	// 登录成功后须等收到令牌帧再 BindUDP：网关凭令牌校验来源，不自报账号，防推送劫持。
	udpToken string
	// 关闭常驻 UDP 通道的通知（关闭 udpConn + 退出保活/读协程）。
	udpStop chan struct{}
	// 保活协程是否已启动（重复登录重绑时只保留一个保活循环）。
	udpRunning bool
	// 通用
	transport      Transport
	mu             sync.Mutex // 保护写操作，避免 Send 与读并发交错
	recv           chan Incoming
	requestID      atomic.Uint32 // 请求关联 ID 自增计数器
	recvBuf        []byte        // 粘包缓冲区（TCP 用）
	done           chan struct{}
	disconnectOnce sync.Once // 确保断线事件只投递一次；recv 不由读协程关闭
}

// 拨号建立到网关的连接，实现原生客户端连接流程：
//  1. 检测网络环境
//  2. UDP 探测（可选）
//  3. QUIC 连接尝试
//  4. TCP 连接尝试（回退到 TCP 端口）
func Dial(addr string, tcpAddr string) (*Client, error) {
	// 1. 检测网络环境：尝试解析 UDP 地址
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		// 无法解析为 UDP 地址，直接使用 TCP（无 UDP 地址，位置回退裸 UDP 不可用）
		fmt.Printf("%s %s\n", util.Yellow("无法解析 UDP 地址，直接使用 TCP:"), err)
		return dialTCP(tcpAddr, "")
	}

	// 2+3. UDP 探测 + QUIC 连接尝试：
	// 用真实 QUIC 握手作为"UDP 是否可用"的判定。仅用 net.DialUDP 会误判——
	// 裸 UDP 端口(8003)即使 QUIC 未启动也在监听，导致探测"成功"后 dialQUIC
	// 又要白等 dialTimeout(5s) 才回退。这里用短超时尝试真实 QUIC 握手：
	// 一旦成功复用连接，失败快速判定 QUIC 不可达并立即回退 TCP，不白等。
	fmt.Printf("%s %s\n", util.Cyan("探测 UDP/QUIC 连通性:"), addr)
	client, err := probeQUIC(udpAddr.String())
	if err == nil {
		fmt.Printf("%s %s\n", util.Green("QUIC 连接成功:"), addr)
		return client, nil
	}
	fmt.Printf("%s %s: %v\n", util.Yellow("QUIC 不可用，回退到 TCP:"), addr, err)

	// 4. TCP 连接尝试（回退到 TCP 端口）；保留 udpAddr 供不可靠位置数据回退裸 UDP。
	fmt.Printf("%s %s\n", util.Cyan("尝试 TCP 连接:"), tcpAddr)
	return dialTCP(tcpAddr, udpAddr.String())
}

// 用短超时尝试真实 QUIC 握手，判定 QUIC 是否可用。
// 成功即复用返回的连接；失败（QUIC 服务未启动/证书不可用等）快速返回错误供回退 TCP。
func probeQUIC(addr string) (*Client, error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"clover-quic"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	conn, err := quic.DialAddr(ctx, addr, tlsConf, &quic.Config{
		MaxIdleTimeout: 30 * time.Second,
		// 连接级保活：空闲时由 quic-go 周期性发送 PING（10s），避免被服务端
		// idle 超时（timeout: no recent network activity）回收，交互工具无需主动发包。
		KeepAlivePeriod: 10 * time.Second,
		EnableDatagrams: true, // 接收网关对 QUIC 连接的不可靠推送（Datagram）；两端必须都开启
	})
	if err != nil {
		return nil, err
	}

	// 主动打开双向流（QUIC 客户端开流方向）；服务器端用 conn.AcceptStream 等客户端开流。
	// 之前误用 AcceptStream 等服务器开流 → 双向互相等待，dialTimeout 后超时回退 TCP。
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, err
	}

	cl := &Client{
		quicConn:  conn,
		stream:    stream,
		transport: TransportQUIC,
		udpAddr:   addr, // QUIC 即共享 UDP 端口，不可靠数据报直接用 QUIC Datagram
		recv:      make(chan Incoming, 64),
		done:      make(chan struct{}),
	}
	go cl.readLoopQUIC()
	go cl.readLoopDatagram() // 接收网关对 QUIC 连接的不可靠推送（Datagram）
	return cl, nil
}

// tcpTLSMode 本进程探测到的「网关 TCP 口是否走 TLS」：0=未探测，1=明文，2=TLS。
//
// 为什么需要：网关配了 tls_cert 后 TCP 口默认也被 TLS 包起来（server.yaml 的
// gateway.tcp_tls_disabled=false），只有显式设 true 才保留明文入口 —— 两种部署都存在。
// 探测结论只算一次：逐条连接"先试 TLS 再回退"会让每次重连都白等一个探测超时。
var tcpTLSMode atomic.Int32

// 尝试 TCP 连接。udpAddr 供 QUIC 不可用时不可靠位置数据回退裸 UDP。
//
// 首次连接时自动判定服务端 TCP 口是 TLS 还是明文（结论打印在连接日志里，见 tcpTLSMode），
// 之后复用该结论，避免每次重连都先失败一次 TLS 握手。
func dialTCP(addr string, udpAddr string) (*Client, error) {
	switch tcpTLSMode.Load() {
	case 2:
		return dialTCPMode(addr, udpAddr, true)
	case 1:
		return dialTCPMode(addr, udpAddr, false)
	}
	// 首次：先按 TLS 拨（网关配了 tls_cert 时的默认形态），失败再按明文拨。
	if cl, err := dialTCPMode(addr, udpAddr, true); err == nil {
		tcpTLSMode.Store(2)
		fmt.Printf("%s %s\n", util.Cyan("TCP 接入使用 TLS:"), addr)
		return cl, nil
	}
	cl, err := dialTCPMode(addr, udpAddr, false)
	if err != nil {
		return nil, err
	}
	tcpTLSMode.Store(1)
	fmt.Printf("%s %s\n", util.Yellow("TCP 接入为明文（服务端 tcp_tls_disabled=true）:"), addr)
	return cl, nil
}

// dialTCPMode 按指定模式拨号并组装连接（useTLS=true 时先完成 TLS 握手）。
func dialTCPMode(addr string, udpAddr string, useTLS bool) (*Client, error) {
	conn, err := dialTCPConn(addr, useTLS)
	if err != nil {
		return nil, err
	}

	cl := &Client{
		conn:      conn,
		transport: TransportTCP,
		udpAddr:   udpAddr,
		recv:      make(chan Incoming, 64),
		recvBuf:   make([]byte, 0, 65536),
		done:      make(chan struct{}),
	}
	go cl.readLoopTCP()
	go cl.tcpHeartbeatLoop()
	return cl, nil
}

// dialTCPConn 拨 TCP，必要时完成 TLS 握手。
// TLS 探测超时收紧到 probeTimeout：明文服务端会让这次握手一直等到超时，
// 而它只影响进程内首次连接（之后按 tcpTLSMode 直接选路）。
func dialTCPConn(addr string, useTLS bool) (net.Conn, error) {
	if !useTLS {
		return net.DialTimeout("tcp", addr, dialTimeout)
	}
	probe := dialTimeout
	if probe > probeTimeout {
		probe = probeTimeout
	}
	d := &net.Dialer{Timeout: probe}
	return tls.DialWithDialer(d, "tcp", addr, &tls.Config{
		// 与 QUIC 路径同款：本地 mkcert / 自签证书场景不打断联调。
		// 生产网关请用受信任证书（本开关只影响这个开发者工具，不影响产品客户端）。
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
}

// 周期发送 TCP ping 控制帧，刷新服务端读空闲超时。
// 服务端 readLoop 以 2×心跳为读 deadline，客户端挂机不发业务消息会超时被断开；
// 主动周期 ping 即保活。与 Send 共用 c.mu 串行化物理写，避免帧交错。
func (c *Client) tcpHeartbeatLoop() {
	t := time.NewTicker(tcpHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			c.mu.Lock()
			if c.conn != nil {
				if err := writeTCPFrame(c.conn, tcpFrameTypePing, nil); err != nil {
					c.closeLocked()
				}
			}
			c.mu.Unlock()
		}
	}
}

// 返回当前使用的传输层类型
func (c *Client) TransportType() Transport {
	return c.transport
}

// 返回服务端消息通道（后台协程持续推送）。
func (c *Client) Recv() <-chan Incoming { return c.recv }

// 发送一条客户端消息（直接写 [4B requestID][4B msgID][body]），返回本次请求的 requestID。
func (c *Client) Send(msgID uint32, body []byte) (uint32, error) {
	requestID := c.requestID.Add(1)
	frame := EncodeClientFrame(requestID, msgID, body)

	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return 0, errors.New("客户端已断开")
	default:
	}

	switch c.transport {
	case TransportQUIC:
		if c.stream == nil {
			return 0, errors.New("未连接")
		}
		// QUIC stream 是字节流，需要长度前缀解决粘包：
		// [4B 大端 len][客户端帧]
		err := writeQUICFrame(c.stream, frame)
		if err != nil {
			c.closeLocked()
		}
		return requestID, err

	case TransportTCP:
		if c.conn == nil {
			return 0, errors.New("未连接")
		}
		// TCP 需要加传输层帧头：[1B type][4B len][payload]
		err := writeTCPFrame(c.conn, tcpFrameTypeData, frame)
		if err != nil {
			c.closeLocked()
		}
		return requestID, err

	default:
		return 0, errors.New("未知传输层")
	}
}

// 在持有 c.mu 时关闭连接。写失败、心跳失败和显式 Close
// 都通过这里收敛，避免重复关闭和锁重入死锁。
func (c *Client) closeLocked() {
	select {
	case <-c.done:
		return
	default:
		close(c.done)
	}
	switch c.transport {
	case TransportQUIC:
		if c.quicConn != nil {
			_ = c.quicConn.CloseWithError(0, "")
		}
	case TransportTCP:
		if c.conn != nil {
			_ = c.conn.Close()
		}
	}
	c.closeUDPLocked()
}

// 发送一条不可靠消息（位置同步等高频数据，可丢包，无回包，requestID 固定 0）。
// QUIC 连接走 QUIC Datagram；QUIC 不可用（TCP 可靠连接）时回退裸 UDP（首字节 0x55 魔数），
// 与引擎 demux 对共享端口的分发规则对齐。
func (c *Client) SendUnreliable(msgID uint32, body []byte) error {
	frame := EncodeClientFrame(0, msgID, body)
	switch c.transport {
	case TransportQUIC:
		if c.quicConn == nil {
			return errors.New("未连接")
		}
		return c.quicConn.SendDatagram(frame)
	default:
		if c.udpAddr == "" {
			return errors.New("无 UDP 地址，无法走裸 UDP 位置通道")
		}
		buf := make([]byte, 1+len(frame))
		buf[0] = rawUDPMagic
		copy(buf[1:], frame)

		c.mu.Lock()
		defer c.mu.Unlock()
		// 登录后已建常驻 UDP 通道：复用该 socket 上行（服务端才能把该来源地址
		// 登记为 owner 的不可靠推送端点并回推）。
		if c.udpConn != nil {
			_, err := c.udpConn.Write(buf)
			return err
		}
		// 未登录/未绑定：临时 socket 上行一次（用后即关，仅单向上行可用）。
		raddr, err := net.ResolveUDPAddr("udp", c.udpAddr)
		if err != nil {
			return err
		}
		pc, err := net.DialUDP("udp", nil, raddr)
		if err != nil {
			return err
		}
		defer pc.Close()
		_, err = pc.Write(buf)
		return err
	}
}

// 收到网关 EMsgUDPBindGrant 令牌后，建立常驻裸 UDP 通道（仅 TCP 可靠连接
// 模式需要；QUIC 模式不可靠通道直接走 QUIC Datagram，无需绑定）。
//
// 动作：
//  1. 新建常驻 UDP socket，并把本 socket 的来源地址凭 token 经 EMsgBindUDP 帧上报给网关，
//     登记为该 owner 的不可靠推送端点；
//  2. 启动后台读协程 readLoopUDP，接收网关经 0x55 魔数下发的不可靠推送并投递 recv；
//  3. 周期性重发绑定帧（udpKeepAlive），实现「最新状态」语义与 NAT 保活。
//
// token 来自网关在登录绑定成功时下发的 EMsgUDPBindGrant 帧，为一次性会话凭据；
// 重复调用（重新登录）会先重建旧的常驻通道。
func (c *Client) BindUDP(token string) error {
	if c.transport != TransportTCP {
		return nil // QUIC 模式无需绑定
	}
	if c.udpAddr == "" {
		return errors.New("无 UDP 地址，无法建立常驻 UDP 通道")
	}
	if token == "" {
		return errors.New("UDP 绑定令牌为空（未收到网关 EMsgUDPBindGrant?）")
	}
	select {
	case <-c.done:
		return errors.New("客户端已关闭")
	default:
	}
	raddr, err := net.ResolveUDPAddr("udp", c.udpAddr)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.udpConn != nil {
		_ = c.udpConn.Close() // 令旧读协程退出（其 socket 已关）
	}
	pc, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return err
	}
	c.udpConn = pc
	c.udpToken = token
	if c.udpStop == nil {
		c.udpStop = make(chan struct{})
	}
	c.sendUDPBindFrame() // 立即上报一次
	// 读协程每次重绑都重启（旧协程随旧 socket 关闭而退出），
	// 保活协程只启动一个：重绑仅换 socket，无需重复保活循环。
	if !c.udpRunning {
		c.udpRunning = true
		go c.udpKeepAliveLoop()
	}
	go c.readLoopUDP(pc)
	return nil
}

// 发送一条 EMsgBindUDP 绑定帧：体为网关下发的绑定令牌
// （EMsgUDPBindGrant），而非自报账号——网关凭令牌鉴权登记来源端点（requestID 固定 0，网关不回包）。
// 需持有 c.mu 或确认非并发写。
func (c *Client) sendUDPBindFrame() {
	if c.udpConn == nil || c.udpToken == "" {
		return
	}
	frame := EncodeClientFrame(0, EMsgBindUDP, []byte(c.udpToken))
	buf := make([]byte, 1+len(frame))
	buf[0] = rawUDPMagic
	copy(buf[1:], frame)
	if _, err := c.udpConn.Write(buf); err != nil {
		fmt.Printf("%s 常驻 UDP 上报绑定: %v\n", util.Yellow("警告:"), err)
	}
}

// 周期性重发 EMsgBindUDP 绑定帧（兼 NAT 保活）。
func (c *Client) udpKeepAliveLoop() {
	t := time.NewTicker(udpKeepAlive)
	defer t.Stop()
	for {
		select {
		case <-c.udpStop:
			return
		case <-t.C:
			c.mu.Lock()
			c.sendUDPBindFrame()
			c.mu.Unlock()
		}
	}
}

// 投递断线事件，通知 pump 当前连接已经结束。
// recv 不关闭，因为 UDP/Datagram 读协程可能仍在退出路径中投递消息。
func (c *Client) notifyDisconnected() {
	c.disconnectOnce.Do(func() {
		// 断线事件必须进入 recv，不能用 default 丢弃，否则 pump 会继续把
		// 已失效的 Client 当成在线连接，用户仍可发送消息到旧连接。
		c.recv <- Incoming{Err: errors.New("连接已断开")}
	})
}

// 接收网关经共享 UDP 端口下发的不可靠推送：
// 首字节 0x55 魔数（demux 分包规则），剥掉后剩余即客户端帧 [4B requestID][4B msgID][body]，
// 解析后投递 recv（与可靠通道同管）。
// 直接读入本次绑定创建的 socket conn（避免与重绑换 socket 竞争字段）。
func (c *Client) readLoopUDP(conn net.Conn) {
	buf := make([]byte, 65536)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			select {
			case <-c.done:
			case <-c.udpStop:
			default:
				fmt.Printf("%s 常驻 UDP 读协程退出: %v\n", util.Yellow("提示:"), err)
			}
			return
		}
		if n < 1 || buf[0] != rawUDPMagic {
			continue // 非法/非本客户端数据报（0x55 全接到共享端口），忽略
		}
		p := buf[1:n]
		if len(p) < headerLen {
			continue
		}
		requestID := binary.BigEndian.Uint32(p[:requestIDLen])
		msgID := binary.BigEndian.Uint32(p[requestIDLen : requestIDLen+msgIDLen])
		body := make([]byte, len(p)-headerLen)
		copy(body, p[headerLen:])
		select {
		case c.recv <- Incoming{RequestID: requestID, MsgID: msgID, Body: body}:
		default:
		}
	}
}

// 关闭连接（幂等：二次调用直接返回）。
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return nil // 已关闭过，避免重复关 channel panic
	default:
		close(c.done)
	}
	switch c.transport {
	case TransportQUIC:
		if c.quicConn != nil {
			_ = c.quicConn.CloseWithError(0, "")
		}
	case TransportTCP:
		if c.conn != nil {
			_ = c.conn.Close()
		}
	}
	c.closeUDPLocked()
	return nil
}

// 关闭常驻裸 UDP 通道：关闭 udpConn 令 readLoopUDP 退出、关闭 udpStop 令
// udpKeepAliveLoop 退出。幂等（close 前用 select 探测，重复 close 不会 panic），需持有 c.mu。
// 连接断开（readLoop 退出）与显式 Close 都会调用，避免保活协程拿着已失效的令牌继续上报
// 被网关拒绝（udp bind invalid token）。
func (c *Client) closeUDPLocked() {
	if c.udpConn != nil {
		_ = c.udpConn.Close()
		c.udpConn = nil
	}
	if c.udpStop != nil {
		select {
		case <-c.udpStop:
		default:
			close(c.udpStop)
		}
		c.udpStop = nil
	}
}

// 持续读取 QUIC stream；数据帧解开后投递 recv。
// QUIC stream 是字节流，对端已加 [4B 大端 len] 前缀，这里按长度读取完整客户端帧。
func (c *Client) readLoopQUIC() {
	defer c.notifyDisconnected()
	defer func() {
		c.mu.Lock()
		c.closeUDPLocked()
		c.mu.Unlock()
	}()
	for {
		select {
		case <-c.done:
			return
		default:
		}

		frame, err := readQUICFrame(c.stream)
		if err != nil {
			if err != io.EOF {
				select {
				case c.recv <- Incoming{Err: err}:
				default:
				}
			}
			return
		}
		if len(frame) == 0 {
			continue
		}

		// 现在 frame 是一条完整客户端帧，直接解析。
		c.mu.Lock()
		c.recvBuf = append(c.recvBuf, frame...)
		c.parseClientFrames()
		c.mu.Unlock()
	}
}

// 接收 QUIC 不可靠通道（Datagram）下发的数据报：网关对 QUIC 连接的
// 不可靠推送即走 Datagram，解析客户端帧后投递 recv（与可靠通道同管）。
func (c *Client) readLoopDatagram() {
	for {
		select {
		case <-c.done:
			return
		default:
		}
		p, err := c.quicConn.ReceiveDatagram(context.Background())
		if err != nil {
			return
		}
		if len(p) < headerLen {
			continue
		}
		requestID := binary.BigEndian.Uint32(p[:requestIDLen])
		msgID := binary.BigEndian.Uint32(p[requestIDLen : requestIDLen+msgIDLen])
		body := make([]byte, len(p)-headerLen)
		copy(body, p[headerLen:])
		select {
		case c.recv <- Incoming{RequestID: requestID, MsgID: msgID, Body: body}:
		default:
		}
	}
}

// 持续读取 TCP 连接；先解传输层帧，再解客户端帧。
func (c *Client) readLoopTCP() {
	defer c.notifyDisconnected()
	defer func() {
		c.mu.Lock()
		c.closeUDPLocked()
		c.mu.Unlock()
	}()
	for {
		select {
		case <-c.done:
			return
		default:
		}

		// 读取 TCP 传输层帧：[1B type][4B len][payload]
		typ, payload, err := readTCPFrame(c.conn, maxMsgSize)
		if err != nil {
			if err != io.EOF {
				select {
				case c.recv <- Incoming{Err: err}:
				default:
				}
			}
			return
		}

		// 只处理数据帧，忽略 ping/pong/migrate 等控制帧
		if typ != tcpFrameTypeData {
			continue
		}

		// payload 就是客户端帧：[4B requestID][4B msgID][body]
		c.mu.Lock()
		c.recvBuf = append(c.recvBuf, payload...)
		c.parseClientFrames()
		c.mu.Unlock()
	}
}

// 从 recvBuf 中解析所有完整的客户端帧（需持有 c.mu）。
//
// 帧格式 [4B requestID][4B msgID][body] 无 body 长度字段，因此无法精确判断帧边界。
// 服务器端同样假设每次 Read 返回一个完整帧（quic-go / TCP codec 保证）。
// 此处按「剩余全部数据 = 一个帧」处理，同时加安全上限防止损坏数据导致无限等待。
func (c *Client) parseClientFrames() {
	for len(c.recvBuf) >= headerLen {
		// 安全上限：单帧不超过 maxMsgSize，超出则视为损坏数据，清空缓冲区。
		if len(c.recvBuf) > maxMsgSize {
			c.recvBuf = c.recvBuf[:0]
			return
		}
		requestID := binary.BigEndian.Uint32(c.recvBuf[:requestIDLen])
		msgID := binary.BigEndian.Uint32(c.recvBuf[requestIDLen : requestIDLen+msgIDLen])
		body := make([]byte, len(c.recvBuf)-headerLen)
		copy(body, c.recvBuf[headerLen:])
		c.recvBuf = c.recvBuf[:0]

		select {
		case c.recv <- Incoming{RequestID: requestID, MsgID: msgID, Body: body}:
		default:
		}
	}
}

// 写入一个 TCP 传输层帧：[1B type][4B len][payload]
func writeTCPFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > math.MaxUint32 {
		return errors.New("tcp: payload too large")
	}
	var hdr [1 + 4]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// 从 TCP 连接读取一个传输层帧。
func readTCPFrame(r io.Reader, maxMsgSize int) (byte, []byte, error) {
	var th [1]byte
	if _, err := io.ReadFull(r, th[:]); err != nil {
		return 0, nil, err
	}
	var lh [4]byte
	if _, err := io.ReadFull(r, lh[:]); err != nil {
		return 0, nil, err
	}
	n32 := binary.BigEndian.Uint32(lh[:])
	limit := maxMsgSize
	if limit <= 0 || limit > 10<<20 {
		limit = 10 << 20 // hardMaxMsgSize = 10MB
	}
	if n32 > uint32(limit) {
		return 0, nil, errors.New("tcp: frame too large")
	}
	n := int(n32)
	buf := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, nil, err
		}
	}
	return th[0], buf, nil
}

// 尽量把 JSON body 美化打印；不是 JSON 则原样返回。
func TryPretty(b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(b)
	}
	return string(out)
}

const quicFrameLenSize = 4
const maxQUICFrameSize = 10 << 20 // 10MB

// 在 QUIC stream 上写入带长度前缀的客户端帧：
// [4B 大端 len][frame]，解决 QUIC stream 字节流无消息边界导致的粘包。
func writeQUICFrame(w io.Writer, frame []byte) error {
	if len(frame) > maxQUICFrameSize {
		return errors.New("quic: frame too large")
	}
	var hdr [quicFrameLenSize]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(frame) == 0 {
		return nil
	}
	_, err := w.Write(frame)
	return err
}

// 从 QUIC stream 读取一条带长度前缀的客户端帧。
func readQUICFrame(r io.Reader) ([]byte, error) {
	var hdr [quicFrameLenSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxQUICFrameSize {
		return nil, errors.New("quic: frame too large")
	}
	if n == 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
