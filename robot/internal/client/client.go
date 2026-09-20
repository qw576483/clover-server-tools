// Package client 实现 robot 到 clover 网关的连接与线协议。
//
// 与 msg-client/internal/client 同源，但按「压测」这个用途做了三处裁剪，
// 每一处都是刻意的，改之前先读这里的理由：
//
//  1. **全程静默**。msg-client 是交互工具，连不上就打一行彩色提示；
//     robot 会在同一进程里开成百上千条连接，任何一行的打印都会变成刷屏。
//     因此本包不引用终端包、不打印，错误一律作为返回值交上去。
//
//  2. **不做 EMsgBindUDP 绑定**。msg-client 收到 EMsgUDPBindGrant 后会建一个
//     常驻裸 UDP socket 并周期保活 —— 那是「人盯着一个客户端联调」的用法。
//     压测下每个机器人都多一个 socket + 一个保活协程，N 一上来就是纯粹的资源浪费。
//     QUIC 模式的不可靠上行直接走 Datagram，压根不需要绑定；TCP 模式下本包
//     只在需要时懒建一个裸 UDP socket 做**单向上行**（见 SendUnreliable）。
//
//  3. **接收通道满时计数而非沉默丢弃**。压测报告的「丢帧数」本身是有效指标；
//     静默 default 丢弃会让 RTT 统计悄悄失真。
//
// 传输层帧格式（与引擎 pkg/transport/net 一致）：
//
//	QUIC: stream 上 [4B 大端 len][客户端帧]（字节流需要长度前缀解决粘包）
//	TCP:  [1B type][4B len][payload]，payload 即客户端帧
//
// 客户端帧：[4B 大端 requestID][4B 大端 msgID][body]。
// requestID != 0 为请求/回包，== 0 为推送。
package client

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	requestIDLen = 4
	msgIDLen     = 4
	headerLen    = requestIDLen + msgIDLen // 8
	maxMsgSize   = 1 << 20                 // 单帧 payload 上限 1MB，防损坏数据
	dialTimeout  = 5 * time.Second

	// probeTimeout 真实 QUIC 握手探测超时。必须远短于 dialTimeout：
	// QUIC 没起时不能白等 5 秒才回退 TCP，否则 N 个机器人的启动会被逐条拖慢。
	probeTimeout = 2 * time.Second

	// recvBuffer 接收通道缓冲。压测下消费端（robot 的事件循环）只做解析与计数，
	// 极少积压；给足缓冲是为了让「瞬时突发回包」不至于撞上丢帧路径。
	recvBuffer = 4096

	// tcpHeartbeatInterval TCP 心跳间隔。服务端读 deadline 是 2× 心跳，
	// 机器人挂机不发业务消息时必须靠它保活，否则会被当成死连接断开。
	tcpHeartbeatInterval = 15 * time.Second
)

// TCP 帧类型（与引擎 pkg/transport/net/tcp/codec.go 对齐）。
const (
	tcpFrameTypeData byte = 0
	tcpFrameTypePing byte = 1
	tcpFrameTypePong byte = 2
)

// rawUDPMagic 裸 UDP 分包魔数，与引擎 demux.RawUDPMagic(0x55) 一致。
const rawUDPMagic byte = 0x55

// 引擎内核 opcode（与 clover-server-engine/internal/shared/proto 对齐）。
//
// 号位 1 曾为 EMsgSignup：注册已完全移到账号服 HTTP，号位作废但保留，不复用。
const (
	EMsgLogin           uint32 = 2
	EMsgResumeSession   uint32 = 3
	EMsgBindUDP         uint32 = 5
	EMsgUDPBindGrant    uint32 = 6
	EPushPlayerFullSync uint32 = 4001
	EPushAlert          uint32 = 4002
	EMsgError           uint32 = 0xFFFFFFFF
	InternalMsgMax      uint32 = 10000
)

// ELoginReply 登录回包体（与引擎 proto.ELoginReply 对齐）。
//
// SessionKey 字段引擎**恒定不填**（网关未接 WithExtractSessionKey），
// 因此本包不做任何通道加解密 —— 别看到字段名就去实现 AES-GCM。
type ELoginReply struct {
	Owner   string `json:"owner"`
	Token   string `json:"token,omitempty"`
	Success bool   `json:"success,omitempty"`
	Err     string `json:"err,omitempty"`
}

// EErrorReply 通用错误回包体（与引擎 proto.EErrorReply 对齐）。
// Code 为机器可读错误码（401 未认证 / 403 被拒 / 429 频率超限 / 500 内部错误）。
type EErrorReply struct {
	Err  string `json:"err"`
	Code int32  `json:"code,omitempty"`
}

// Transport 传输层选择。
type Transport int

const (
	// TransportAuto 先试 QUIC，失败回退 TCP（与真实客户端一致）。
	TransportAuto Transport = iota
	// TransportQUIC 强制 QUIC。
	TransportQUIC
	// TransportTCP 强制 TCP。
	TransportTCP
)

func (t Transport) String() string {
	switch t {
	case TransportQUIC:
		return "quic"
	case TransportTCP:
		return "tcp"
	default:
		return "auto"
	}
}

// ParseTransport 解析传输层名（auto / quic / tcp）。
func ParseTransport(s string) (Transport, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return TransportAuto, nil
	case "quic", "udp":
		return TransportQUIC, nil
	case "tcp":
		return TransportTCP, nil
	default:
		return TransportAuto, fmt.Errorf("未知传输层 %q（可选 auto / quic / tcp）", s)
	}
}

// EncodeClientFrame 组一条客户端帧：[4B requestID][4B msgID][body]。
func EncodeClientFrame(requestID, msgID uint32, body []byte) []byte {
	buf := make([]byte, headerLen+len(body))
	binary.BigEndian.PutUint32(buf[:requestIDLen], requestID)
	binary.BigEndian.PutUint32(buf[requestIDLen:requestIDLen+msgIDLen], msgID)
	copy(buf[headerLen:], body)
	return buf
}

// Incoming 一条从服务端收到的消息（已解开传输层帧与客户端帧）。
type Incoming struct {
	RequestID uint32
	MsgID     uint32
	Body      []byte
	Err       error // 非 nil 表示连接已断开或解析出错
}

// Options 拨号参数。
type Options struct {
	// Gateway QUIC/UDP 地址；TransportTCP 时可不填。
	Gateway string
	// GatewayTCP TCP 地址；TransportQUIC 时可不填。
	GatewayTCP string
	// Transport 传输层选择。
	Transport Transport
	// DialTimeout 单次拨号超时（TCP 与 QUIC 共用）；<=0 用 5s。
	DialTimeout time.Duration
	// ProbeTimeout QUIC 探测超时（auto 模式下快速失败用）；<=0 用 2s。
	ProbeTimeout time.Duration
}

// Client 一条到网关的连接。
type Client struct {
	quicConn quic.Connection
	stream   quic.Stream

	conn net.Conn

	// transport 实际生效的传输层（auto 会落到 quic 或 tcp）。
	transport Transport

	// udpAddr QUIC/UDP 端口地址。TCP 模式下不可靠上行（裸 UDP 0x55）发往这里。
	udpAddr string
	// udpUpConn 懒创建的裸 UDP 上行 socket（仅 TCP 模式使用）。
	// 只发不收：本工具不启用 EMsgBindUDP，服务端不会把该端点登记为可回推端点。
	udpUpConn net.Conn

	mu      sync.Mutex
	recv    chan Incoming
	done    chan struct{}
	recvBuf []byte

	requestID      atomic.Uint32
	disconnectOnce sync.Once
	dropped        atomic.Uint64
}

// Dial 建立到网关的连接。
//
// auto 模式与真实客户端一致：先用**真实 QUIC 握手**判定 QUIC 是否可用
// （只用 net.DialUDP 会误判——裸 UDP 端口在 QUIC 未启动时照样在监听），
// 失败则快速回退 TCP。
func Dial(opt Options) (*Client, error) {
	if opt.DialTimeout <= 0 {
		opt.DialTimeout = dialTimeout
	}
	if opt.ProbeTimeout <= 0 {
		opt.ProbeTimeout = probeTimeout
	}

	switch opt.Transport {
	case TransportQUIC:
		if opt.Gateway == "" {
			return nil, errors.New("未配置网关 QUIC 地址")
		}
		return dialQUIC(opt.Gateway, opt.ProbeTimeout)
	case TransportTCP:
		if opt.GatewayTCP == "" {
			return nil, errors.New("未配置网关 TCP 地址")
		}
		return dialTCP(opt.GatewayTCP, opt.Gateway, opt.DialTimeout)
	}

	// auto：QUIC 优先，失败回退 TCP。
	if opt.Gateway != "" {
		if c, err := dialQUIC(opt.Gateway, opt.ProbeTimeout); err == nil {
			return c, nil
		}
	}
	if opt.GatewayTCP == "" {
		return nil, errors.New("QUIC 不可用，且未配置网关 TCP 地址")
	}
	return dialTCP(opt.GatewayTCP, opt.Gateway, opt.DialTimeout)
}

// dialQUIC 尝试 QUIC 连接；addr 同时是不可靠数据报（Datagram）的通道。
func dialQUIC(addr string, timeout time.Duration) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, err := quic.DialAddr(ctx, addr, &tls.Config{
		InsecureSkipVerify: true, // 与 msg-client 一致：网关自签证书，靠 pin/环境约束
		NextProtos:         []string{"clover-quic"},
	}, &quic.Config{
		MaxIdleTimeout: 60 * time.Second,
		// 连接级保活：空闲时由 quic-go 周期发 PING，避免被服务端 idle 超时回收。
		KeepAlivePeriod: 10 * time.Second,
		EnableDatagrams: true, // 接收 / 发送不可靠数据报；两端必须都开
	})
	if err != nil {
		return nil, err
	}

	// 客户端主动开流（服务端用 AcceptStream 等它）。
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, err
	}

	c := &Client{
		quicConn:  conn,
		stream:    stream,
		transport: TransportQUIC,
		udpAddr:   addr,
		recv:      make(chan Incoming, recvBuffer),
		done:      make(chan struct{}),
	}
	go c.readLoopQUIC()
	go c.readLoopDatagram()
	return c, nil
}

// tcpTLSMode 本进程探测到的「网关 TCP 口是否走 TLS」：0=未探测，1=明文，2=TLS。
//
// 为什么必须"只探测一次"：压测会开成千上万条连接，逐条先试 TLS 再回退会让服务端
// 每条连接都吃一次握手失败（日志噪音），客户端还要白等一个探测超时。
var tcpTLSMode atomic.Int32

// tlsProbeTimeout TLS 探测的握手超时：明文服务端会让握手等到超时，收紧到 2s。
const tlsProbeTimeout = 2 * time.Second

// dialTCP 尝试 TCP 连接；udpAddr 供不可靠上行回退裸 UDP。
//
// 网关 TCP 口是否走 TLS 由 server.yaml 的 gateway.tcp_tls_disabled 决定（配了 tls_cert
// 时默认走 TLS），本工具在进程内首次连接时判定一次并复用结论（见 tcpTLSMode）。
func dialTCP(addr, udpAddr string, timeout time.Duration) (*Client, error) {
	switch tcpTLSMode.Load() {
	case 2:
		return dialTCPMode(addr, udpAddr, timeout, true)
	case 1:
		return dialTCPMode(addr, udpAddr, timeout, false)
	}
	// 首次：先按 TLS 拨，失败再按明文拨（两种部署都要能压）。
	if c, err := dialTCPMode(addr, udpAddr, timeout, true); err == nil {
		tcpTLSMode.Store(2)
		return c, nil
	}
	c, err := dialTCPMode(addr, udpAddr, timeout, false)
	if err != nil {
		return nil, err
	}
	tcpTLSMode.Store(1)
	return c, nil
}

// dialTCPMode 按指定模式拨号并组装连接（useTLS=true 时先完成 TLS 握手）。
func dialTCPMode(addr, udpAddr string, timeout time.Duration, useTLS bool) (*Client, error) {
	conn, err := dialTCPConn(addr, timeout, useTLS)
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn:      conn,
		transport: TransportTCP,
		udpAddr:   udpAddr,
		recv:      make(chan Incoming, recvBuffer),
		done:      make(chan struct{}),
	}
	go c.readLoopTCP()
	go c.tcpHeartbeatLoop()
	return c, nil
}

// dialTCPConn 拨 TCP，必要时完成 TLS 握手（与 dialQUIC 同款：本地/内网自签证书不打断压测）。
func dialTCPConn(addr string, timeout time.Duration, useTLS bool) (net.Conn, error) {
	if !useTLS {
		return net.DialTimeout("tcp", addr, timeout)
	}
	probe := timeout
	if probe <= 0 || probe > tlsProbeTimeout {
		probe = tlsProbeTimeout
	}
	d := &net.Dialer{Timeout: probe}
	return tls.DialWithDialer(d, "tcp", addr, &tls.Config{
		InsecureSkipVerify: true, // 与 dialQUIC 一致：压测面向本地/内网网关
		MinVersion:         tls.VersionTLS12,
	})
}

// TransportType 返回实际生效的传输层。
func (c *Client) TransportType() Transport { return c.transport }

// Recv 返回服务端消息通道（后台读协程持续投递）。
func (c *Client) Recv() <-chan Incoming { return c.recv }

// Dropped 返回因接收通道满而被丢弃的帧数。
// 正常压测里这个值应当恒为 0；不为 0 说明消费端跟不上，报告的 RTT 会失真。
func (c *Client) Dropped() uint64 { return c.dropped.Load() }

// Send 发送一条可靠消息，返回本次请求的 requestID（供回包配对）。
func (c *Client) Send(msgID uint32, body []byte) (uint32, error) {
	requestID := c.requestID.Add(1)
	frame := EncodeClientFrame(requestID, msgID, body)

	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return 0, errors.New("连接已关闭")
	default:
	}

	switch c.transport {
	case TransportQUIC:
		if c.stream == nil {
			return 0, errors.New("连接已关闭")
		}
		if err := writeQUICFrame(c.stream, frame); err != nil {
			c.closeLocked()
			return 0, err
		}
	case TransportTCP:
		if c.conn == nil {
			return 0, errors.New("连接已关闭")
		}
		if err := writeTCPFrame(c.conn, tcpFrameTypeData, frame); err != nil {
			c.closeLocked()
			return 0, err
		}
	default:
		return 0, errors.New("未知传输层")
	}
	return requestID, nil
}

// SendUnreliable 发送一条不可靠消息（位置同步等高频数据，可丢包、无回包）。
//
// QUIC：走 Datagram。
// TCP ：回退裸 UDP（首字节 0x55 魔数，与引擎 demux 的分发规则对齐）。
//
// **只做单向上行**：本工具不启用 EMsgBindUDP，服务端不会把这个来源地址登记成
// 可回推端点，故下行不可靠推送收不到。压测关心的是"服务端吃不吃得下这个量"，
// 上行足够；要收下行就用 QUIC 或 msg-client。
func (c *Client) SendUnreliable(msgID uint32, body []byte) error {
	frame := EncodeClientFrame(0, msgID, body)

	if c.transport == TransportQUIC {
		if c.quicConn == nil {
			return errors.New("连接已关闭")
		}
		return c.quicConn.SendDatagram(frame)
	}

	if c.udpAddr == "" {
		return errors.New("未配置网关 UDP 地址，无法在 TCP 模式下走裸 UDP 上行")
	}
	buf := make([]byte, 1+len(frame))
	buf[0] = rawUDPMagic
	copy(buf[1:], frame)

	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return errors.New("连接已关闭")
	default:
	}
	// 懒建一次并复用：每次现建现关会给每个机器人制造大量短命 socket。
	if c.udpUpConn == nil {
		raddr, err := net.ResolveUDPAddr("udp", c.udpAddr)
		if err != nil {
			return err
		}
		conn, err := net.DialUDP("udp", nil, raddr)
		if err != nil {
			return err
		}
		c.udpUpConn = conn
	}
	_, err := c.udpUpConn.Write(buf)
	return err
}

// Close 关闭连接（幂等）。
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
	return nil
}

// closeLocked 关闭底层连接与裸 UDP socket，需持有 c.mu。
func (c *Client) closeLocked() {
	select {
	case <-c.done:
		return // 已关过，避免重复 close panic
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
	if c.udpUpConn != nil {
		_ = c.udpUpConn.Close()
		c.udpUpConn = nil
	}
}

// notifyDisconnected 投递断线事件（只投一次）。
// recv 不关闭：Datagram / UDP 侧可能仍在退出路径上投递，关掉会 panic。
func (c *Client) notifyDisconnected(err error) {
	c.disconnectOnce.Do(func() {
		if err == nil {
			err = io.EOF
		}
		select {
		case c.recv <- Incoming{Err: err}:
		case <-c.done:
		}
	})
}

// tcpHeartbeatLoop 周期发 TCP ping 控制帧，刷新服务端读 deadline。
// 与 Send 共用 c.mu 串行化物理写，避免帧交错。
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

// readLoopQUIC 持续读取 QUIC stream，按长度前缀切出完整客户端帧。
func (c *Client) readLoopQUIC() {
	defer c.notifyDisconnected(nil)
	for {
		select {
		case <-c.done:
			return
		default:
		}
		frame, err := readQUICFrame(c.stream)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.notifyDisconnected(err)
			}
			return
		}
		if len(frame) == 0 {
			continue
		}
		c.deliver(frame)
	}
}

// readLoopDatagram 接收 QUIC 不可靠通道（Datagram）下发的数据报。
func (c *Client) readLoopDatagram() {
	for {
		select {
		case <-c.done:
			return
		default:
		}
		if c.quicConn == nil {
			return
		}
		p, err := c.quicConn.ReceiveDatagram(context.Background())
		if err != nil {
			return
		}
		c.deliver(p)
	}
}

// readLoopTCP 持续读取 TCP 传输层帧，取出 payload（客户端帧）后投递。
func (c *Client) readLoopTCP() {
	defer c.notifyDisconnected(nil)
	for {
		select {
		case <-c.done:
			return
		default:
		}
		typ, payload, err := readTCPFrame(c.conn, maxMsgSize)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.notifyDisconnected(err)
			}
			return
		}
		if typ != tcpFrameTypeData {
			continue // 忽略 ping / pong 等控制帧
		}
		c.deliver(payload)
	}
}

// deliver 把一条完整客户端帧投递到 recv。
//
// 帧格式 [4B requestID][4B msgID][body] 没有 body 长度字段，无法精确切分边界，
// 因此与 msg-client / 服务端一样按「一次读到的内容 = 一个完整帧」处理
// （quic-go 的长度前缀与 TCP codec 保证了这一点）。
func (c *Client) deliver(frame []byte) {
	if len(frame) < headerLen {
		return
	}
	requestID := binary.BigEndian.Uint32(frame[:requestIDLen])
	msgID := binary.BigEndian.Uint32(frame[requestIDLen : requestIDLen+msgIDLen])
	body := make([]byte, len(frame)-headerLen)
	copy(body, frame[headerLen:])

	select {
	case c.recv <- Incoming{RequestID: requestID, MsgID: msgID, Body: body}:
	default:
		// 满则丢弃并计数：报告里的 dropped 是有效指标，静默丢弃会让 RTT 悄悄失真。
		c.dropped.Add(1)
	}
}

// writeTCPFrame 写一个 TCP 传输层帧：[1B type][4B len][payload]。
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

// readTCPFrame 读一个 TCP 传输层帧。
func readTCPFrame(r io.Reader, maxSize int) (byte, []byte, error) {
	var th [1]byte
	if _, err := io.ReadFull(r, th[:]); err != nil {
		return 0, nil, err
	}
	var lh [4]byte
	if _, err := io.ReadFull(r, lh[:]); err != nil {
		return 0, nil, err
	}
	limit := maxSize
	if limit <= 0 || limit > 10<<20 {
		limit = 10 << 20
	}
	n32 := binary.BigEndian.Uint32(lh[:])
	if n32 > uint32(limit) {
		return 0, nil, errors.New("tcp: frame too large")
	}
	buf := make([]byte, int(n32))
	if n32 > 0 {
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, nil, err
		}
	}
	return th[0], buf, nil
}

const maxQUICFrameSize = 10 << 20 // 10MB

// writeQUICFrame 在 QUIC stream 上写带长度前缀的客户端帧。
func writeQUICFrame(w io.Writer, frame []byte) error {
	if len(frame) > maxQUICFrameSize {
		return errors.New("quic: frame too large")
	}
	var hdr [4]byte
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

// readQUICFrame 从 QUIC stream 读一条带长度前缀的客户端帧。
func readQUICFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
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
