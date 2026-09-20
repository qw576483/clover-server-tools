// Package client 实现 clover 网关 TCP 客户端与线协议。
//
// 传输层帧格式（与 clover-server-engine/pkg/transport/net/tcp 对齐）：
//
//	[1B 帧类型][4B 大端长度][payload]
//
// 帧类型：0=数据，1=ping，2=pong。
// 数据帧 payload = [4B requestID][4B msgID][body]。
package client

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	frameTypeData byte = 0
	frameTypePing byte = 1
	frameTypePong byte = 2

	requestIDLen    = 4
	msgIDLen        = 4
	headerLen       = requestIDLen + msgIDLen // 8
	lengthFieldSize = 4
	maxMsgSize      = 1 << 20
	dialTimeout     = 5 * time.Second
)

// 服务端下发的一个数据帧（回包或推送）。
type Frame struct {
	RequestID uint32 // 请求关联 ID（回包非 0，推送为 0）
	MsgID     uint32
	Body      []byte
}

type recvMsg struct {
	frame Frame
	err   error
}

// 网关 TCP 客户端。
// 后台读协程持续消费帧（自动回 pong），Send 排空通道返回所有已接收的数据帧。
type Client struct {
	mu        sync.Mutex
	conn      net.Conn
	timeout   time.Duration
	recvCh    chan recvMsg
	done      chan struct{}
	requestID atomic.Uint32 // 请求关联 ID 自增计数器
}

// 创建客户端。
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{timeout: timeout}
}

// 拨号连接网关，启动后台读协程。
func (c *Client) Connect(addr string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
	}

	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return err
	}
	c.conn = conn
	c.recvCh = make(chan recvMsg, 256)
	c.done = make(chan struct{})
	go c.readLoop()
	return nil
}

// 后台持续读帧：数据帧→recvCh，ping→自动回 pong。
func (c *Client) readLoop() {
	defer func() {
		close(c.recvCh)
	}()
	for {
		select {
		case <-c.done:
			return
		default:
		}
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()
		if conn == nil {
			return
		}

		typ, payload, err := readFrame(conn)
		if err != nil {
			select {
			case c.recvCh <- recvMsg{err: err}:
			default:
			}
			return
		}
		switch typ {
		case frameTypeData:
			if len(payload) < headerLen {
				continue
			}
			requestID := binary.BigEndian.Uint32(payload[:requestIDLen])
			msgID := binary.BigEndian.Uint32(payload[requestIDLen : requestIDLen+msgIDLen])
			body := make([]byte, len(payload)-headerLen)
			copy(body, payload[headerLen:])
			select {
			case c.recvCh <- recvMsg{frame: Frame{RequestID: requestID, MsgID: msgID, Body: body}}:
			default:
				// 通道满则丢弃（避免阻塞读协程）
			}
		case frameTypePing:
			c.mu.Lock()
			if c.conn != nil {
				_ = writeFrame(c.conn, frameTypePong, nil)
			}
			c.mu.Unlock()
		case frameTypePong:
			// 忽略
		}
	}
}

// 发送消息，阻塞等首帧（回包），非阻塞排空已缓冲帧，立即返回。
// 走 NATS 的异步推送帧（如 EPushPlayerFullSync）到达较晚，前端通过 /api/recv 轮询拿。
func (c *Client) Send(msgID uint32, body []byte) ([]Frame, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return nil, errors.New("未连接")
	}

	// 构建客户端帧: [4B requestID][4B msgID][body]
	requestID := c.requestID.Add(1)
	clientFrame := make([]byte, headerLen+len(body))
	binary.BigEndian.PutUint32(clientFrame[:requestIDLen], requestID)
	binary.BigEndian.PutUint32(clientFrame[requestIDLen:requestIDLen+msgIDLen], msgID)
	copy(clientFrame[headerLen:], body)

	c.mu.Lock()
	if err := writeFrame(c.conn, frameTypeData, clientFrame); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()

	// 阻塞等第一个帧（回包）
	first, ok := <-c.recvCh
	if !ok {
		return nil, errors.New("连接已断开")
	}
	if first.err != nil {
		return nil, first.err
	}
	frames := []Frame{first.frame}

	// 非阻塞排空已缓冲的帧（立即返回，不等待）
	for {
		select {
		case recv, ok := <-c.recvCh:
			if !ok {
				return frames, nil
			}
			if recv.err != nil {
				return frames, recv.err
			}
			frames = append(frames, recv.frame)
		default:
			return frames, nil
		}
	}
}

// 非阻塞读取 recvCh 中的所有待处理帧（供轮询端点调用）。
func (c *Client) Recv() []Frame {
	var frames []Frame
	for {
		select {
		case recv, ok := <-c.recvCh:
			if !ok || recv.err != nil {
				return frames
			}
			frames = append(frames, recv.frame)
		default:
			return frames
		}
	}
}

// 关闭连接，停止后台读协程。
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done != nil {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	}
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// 是否已连接。
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var th [1]byte
	if _, err := io.ReadFull(r, th[:]); err != nil {
		return 0, nil, err
	}
	var lh [lengthFieldSize]byte
	if _, err := io.ReadFull(r, lh[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(lh[:]))
	if n < 0 || n > maxMsgSize {
		return 0, nil, errors.New("tcp: frame too large")
	}
	buf := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, nil, err
		}
	}
	return th[0], buf, nil
}

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	var hdr [1 + lengthFieldSize]byte
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
