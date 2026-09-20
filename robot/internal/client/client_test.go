package client

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// TestEncodeClientFrame 守住帧布局：[4B requestID][4B msgID][body]，大端。
// 这个布局一旦错一个字节，服务端会静默把它当垃圾帧，排查起来非常费劲。
func TestEncodeClientFrame(t *testing.T) {
	body := []byte(`{"token":"t"}`)
	frame := EncodeClientFrame(7, EMsgLogin, body)

	if len(frame) != headerLen+len(body) {
		t.Fatalf("帧长 = %d, want %d", len(frame), headerLen+len(body))
	}
	if got := binary.BigEndian.Uint32(frame[0:4]); got != 7 {
		t.Fatalf("requestID = %d, want 7", got)
	}
	if got := binary.BigEndian.Uint32(frame[4:8]); got != EMsgLogin {
		t.Fatalf("msgID = %d, want %d", got, EMsgLogin)
	}
	if got := string(frame[8:]); got != string(body) {
		t.Fatalf("body = %q", got)
	}
}

// TestTCPRoundTrip 用裸 TCP mock 服务端跑一次真实往返：
// 既能验证 TCP 传输层帧 [1B type][4B len][payload] 的正确性，
// 也能验证 requestID 配对与 Recv 投递。
func TestTCPRoundTrip(t *testing.T) {
	// 本用例的 mock 服务端是**明文** TCP：显式声明模式，跳过首次 TLS 探测
	// （探测会把 ClientHello 当帧读，mock 读不了也不该为它让步，见 tcpTLSMode）。
	tcpTLSMode.Store(1)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer conn.Close()

		// 期望收到一个 data 帧，payload 是 [reqID][EMsgLogin][body]
		typ, payload, err := readTCPFrame(conn, maxMsgSize)
		if err != nil {
			srvErr <- err
			return
		}
		if typ != tcpFrameTypeData {
			srvErr <- errUnexpected("帧类型", int(typ))
			return
		}
		if len(payload) < headerLen {
			srvErr <- errUnexpected("payload 过短", len(payload))
			return
		}
		reqID := binary.BigEndian.Uint32(payload[0:4])
		msgID := binary.BigEndian.Uint32(payload[4:8])
		if msgID != EMsgLogin {
			srvErr <- errUnexpected("msgID", int(msgID))
			return
		}

		// 回一个「回包」：msgID 恒为 0，按 requestID 配对。
		reply := EncodeClientFrame(reqID, 0, []byte(`{"success":true,"owner":"tester"}`))
		srvErr <- writeTCPFrame(conn, tcpFrameTypeData, reply)
	}()

	c, err := Dial(Options{GatewayTCP: ln.Addr().String(), Transport: TransportTCP})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if c.TransportType() != TransportTCP {
		t.Fatalf("transport = %v, want TCP", c.TransportType())
	}

	reqID, err := c.Send(EMsgLogin, []byte(`{"token":"t"}`))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if reqID == 0 {
		t.Fatal("requestID 不可为 0（0 是推送语义）")
	}

	select {
	case inc := <-c.Recv():
		if inc.Err != nil {
			t.Fatalf("recv err: %v", inc.Err)
		}
		if inc.RequestID != reqID {
			t.Fatalf("回包 requestID = %d, want %d", inc.RequestID, reqID)
		}
		if inc.MsgID != 0 {
			t.Fatalf("回包 msgID = %d, want 0", inc.MsgID)
		}
		if got := string(inc.Body); got != `{"success":true,"owner":"tester"}` {
			t.Fatalf("回包 body = %q", got)
		}
	case err := <-srvErr:
		if err != nil {
			t.Fatalf("mock 服务端: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待回包超时")
	}
}

// TestSendUnreliableTCPRequiresUDPAddr 没有 UDP 地址时，TCP 模式的不可靠发送
// 必须**明确报错**，而不是静默成功 —— 静默会让压测报告看起来"发送正常"，
// 实际上一条都没出去。
func TestSendUnreliableTCPRequiresUDPAddr(t *testing.T) {
	// 同 TestTCPRoundTrip：mock 服务端为明文 TCP，跳过 TLS 探测。
	tcpTLSMode.Store(1)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
		}
	}()

	c, err := Dial(Options{GatewayTCP: ln.Addr().String(), Transport: TransportTCP})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := c.SendUnreliable(10001, []byte("{}")); err == nil {
		t.Fatal("未配置 UDP 地址时应当报错，而不是静默成功")
	}
}

func TestParseTransport(t *testing.T) {
	cases := map[string]Transport{
		"":      TransportAuto,
		"auto":  TransportAuto,
		"quic":  TransportQUIC,
		"udp":   TransportQUIC,
		"TCP":   TransportTCP,
		" tcp ": TransportTCP,
	}
	for in, want := range cases {
		got, err := ParseTransport(in)
		if err != nil {
			t.Fatalf("ParseTransport(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseTransport(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseTransport("websocket"); err == nil {
		t.Fatal("未知传输层应当报错")
	}
}

// errUnexpected 是 mock 服务端里断言失败时用的错误。
func errUnexpected(what string, got int) error {
	return fmt.Errorf("mock 服务端断言失败: %s = %d", what, got)
}
