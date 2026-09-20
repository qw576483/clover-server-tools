// Command viewprobe 是「多人在线视野同步」的无人值守端到端探针。
//
// 为什么需要它：`clover-mmo-1/docs/验证报告.md` 的 B-2 只用「**单客户端 + 服务端假人**」
// 验过视野同步（enter/leave 的实体是刷出来的 bot）。真实的多客户端场景没测过 ——
// 两个**真玩家**互相进出视野时的 AOI 事件、以及事件是否真的下发到对端，两者是不同的链路：
//
//	假人路径：业务直接调 PushToPlayer 推移动/属性（业务自己算接收者）
//	真人路径：引擎 WireEntitySync 订阅 AOI 事件 → 按「谁在看谁」的反向索引下发
//	          （enter/leave 由引擎产生，业务不介入）
//
// 本探针用两个真实连接（msg-client 自己的线协议客户端）把真人路径跑通并断言：
//
//	A 上线进图 → B 上线进图 → 断言 **A 收到 enter(B)** 且 **B 收到 enter(A)**
//	→ B 离图 → 断言 **A 收到 leave(B)**
//
// 判定依据是客户端**实际收到的 EPushDataSync(4003)** 帧体（`{"enter":{"entity_id":N}}` /
// `{"leave":{"entity_id":N}}`），而不是服务端日志 —— 「推送算出来了」不等于「推送送到了」。
//
// 用法：
//
//	go run ./cmd/viewprobe -a 900001 -b 900002
//
// 退出码：0=全部断言通过；1=有断言失败；2=用法/环境问题。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/qw576483/clover-server-tools/msg-client/internal/authclient"
	"github.com/qw576483/clover-server-tools/msg-client/internal/client"
)

// 线协议消息号（与引擎 pkg/shared/proto 及 clover-mmo-1/game/def 对齐）。
const (
	// epushDataSync 引擎的数据同步推送（业务用它下发视野事件 / 移动 / 属性）。
	// client 包只内置了少数几个引擎 opcode，这里按 proto 定义自带一个常量。
	epushDataSync = 4003

	msgEnterMap     = 1000101 // C2S 进图
	msgLeaveMap     = 1000103 // C2S 离图
	msgCreatePlayer = 1000107 // C2S 建角（进游戏）
)

// peer 一个已登录的客户端，并把收到的帧累积下来供断言。
type peer struct {
	name    string
	account string
	cl      *client.Client

	mu     sync.Mutex
	frames []client.Incoming
}

// collect 后台收集服务端下发的每一帧（直到连接关闭）。
func (p *peer) collect() {
	for inc := range p.cl.Recv() {
		if inc.Err != nil {
			return
		}
		p.mu.Lock()
		p.frames = append(p.frames, inc)
		p.mu.Unlock()
	}
}

// snapshot 返回当前已收帧的副本（避免断言期间持锁）。
func (p *peer) snapshot() []client.Incoming {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]client.Incoming, len(p.frames))
	copy(out, p.frames)
	return out
}

// call 发一条消息并等它的回包（回包 msgID 恒为 0，按 requestID 配对），返回包体。
func (p *peer) call(msgID uint32, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	reqID, err := p.cl.Send(msgID, raw)
	if err != nil {
		return nil, fmt.Errorf("发送 %d: %w", msgID, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, inc := range p.snapshot() {
			if inc.RequestID == reqID && inc.MsgID == 0 {
				return inc.Body, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("等回包超时 msgID=%d", msgID)
}

// waitViewEvent 在 p 的收帧里等一条「视野事件」：EPushDataSync 且事件名与实体匹配。
// 返回命中的原始帧体（作为证据）。
func (p *peer) waitViewEvent(event string, entityID uint64, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	needle := fmt.Sprintf("\"%s\"", event)
	entNeedle := fmt.Sprintf("\"entity_id\":%d", entityID)
	for time.Now().Before(deadline) {
		for _, inc := range p.snapshot() {
			if inc.MsgID != epushDataSync {
				continue
			}
			body := string(inc.Body)
			if strings.Contains(body, needle) && strings.Contains(body, entNeedle) {
				return body, true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", false
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8003", "网关 QUIC/UDP 地址")
	tcpAddr := flag.String("tcp", "127.0.0.1:8002", "网关 TCP 地址（QUIC 不可用时回退）")
	authAddr := flag.String("auth", "http://127.0.0.1:8051", "账号服 HTTP 地址")
	accA := flag.String("a", "900001", "玩家 A 的账号（必须纯数字：场景 objID 取自账号）")
	accB := flag.String("b", "900002", "玩家 B 的账号（必须纯数字）")
	password := flag.String("password", "123123", "账号密码")
	sceneID := flag.Uint64("scene", 1, "进图场景 id")
	settle := flag.Duration("settle", 3*time.Second, "进图后等待 AOI 事件的时间")
	flag.Parse()

	ac := authclient.New(*authAddr)
	a, err := login(*addr, *tcpAddr, ac, "A", *accA, *password)
	if err != nil {
		fmt.Printf("玩家 A 登录失败: %v\n", err)
		os.Exit(2)
	}
	defer func() { _ = a.cl.Close() }()
	fmt.Printf("玩家 A(%s) 已登录\n", a.account)

	if _, err := a.call(msgCreatePlayer, map[string]any{"name": "viewprobe-a"}); err != nil {
		fmt.Printf("玩家 A 建角失败: %v\n", err)
		os.Exit(2)
	}
	if reply, err := a.call(msgEnterMap, map[string]any{"scene_id": *sceneID}); err != nil {
		fmt.Printf("玩家 A 进图失败: %v\n", err)
		os.Exit(2)
	} else {
		fmt.Printf("玩家 A 进图回包: %s\n", strings.TrimSpace(string(reply)))
	}

	b, err := login(*addr, *tcpAddr, ac, "B", *accB, *password)
	if err != nil {
		fmt.Printf("玩家 B 登录失败: %v\n", err)
		os.Exit(2)
	}
	defer func() { _ = b.cl.Close() }()
	fmt.Printf("玩家 B(%s) 已登录\n", b.account)

	if _, err := b.call(msgCreatePlayer, map[string]any{"name": "viewprobe-b"}); err != nil {
		fmt.Printf("玩家 B 建角失败: %v\n", err)
		os.Exit(2)
	}
	if reply, err := b.call(msgEnterMap, map[string]any{"scene_id": *sceneID}); err != nil {
		fmt.Printf("玩家 B 进图失败: %v\n", err)
		os.Exit(2)
	} else {
		fmt.Printf("玩家 B 进图回包: %s\n", strings.TrimSpace(string(reply)))
	}

	// 场景 objID = 账号的数值形态（业务 sceneObjID(account)），断言里用它匹配 entity_id。
	idA := mustUint64(a.account)
	idB := mustUint64(b.account)

	fail := 0
	// 断言 1：A 收到 B 进入视野（A 先在图里，B 后进 → 只有 A 能"看到"这次进入）。
	if body, ok := a.waitViewEvent("enter", idB, *settle); ok {
		fmt.Printf("PASS  A 收到 enter(B=%d)：%s\n", idB, body)
	} else {
		fmt.Printf("FAIL  A 未收到 enter(B=%d)\n", idB)
		fail++
	}
	// 断言 2：B 进图时也应立刻看到已在图里的 A（enter 是双向的）。
	if body, ok := b.waitViewEvent("enter", idA, *settle); ok {
		fmt.Printf("PASS  B 收到 enter(A=%d)：%s\n", idA, body)
	} else {
		fmt.Printf("FAIL  B 未收到 enter(A=%d)\n", idA)
		fail++
	}

	// 断言 3：B 离图 → A 收到 leave(B)。
	if _, err := b.call(msgLeaveMap, map[string]any{"scene_id": *sceneID}); err != nil {
		fmt.Printf("玩家 B 离图失败: %v\n", err)
		fail++
	}
	if body, ok := a.waitViewEvent("leave", idB, *settle); ok {
		fmt.Printf("PASS  A 收到 leave(B=%d)：%s\n", idB, body)
	} else {
		fmt.Printf("FAIL  A 未收到 leave(B=%d)\n", idB)
		fail++
	}

	if fail > 0 {
		fmt.Printf("\n结论：%d 项断言未通过\n", fail)
		os.Exit(1)
	}
	fmt.Printf("\n结论：多人在线视野同步（真人 ↔ 真人）三项断言全部通过\n")
}

// login 建立连接 → 账号服换 token → 发 EMsgLogin → 等 success=true 的回包。
//
// 整体带重试：网关有连接级**准入**（max_conns_per_sec；未启用 queue_cap 时超限直接关连接），
// 于是「QUIC 握手成功但登录帧发不出去/收不到回包」是**预期现象**，不是登录链路故障
// （见 robot README「会被压测结果骗的几处」第 1 条）。重试即绕过。
func login(addr, tcpAddr string, ac *authclient.Client, name, account, password string) (*peer, error) {
	res, err := ac.Login(account, password)
	if err != nil {
		return nil, fmt.Errorf("账号服换 token: %w", err)
	}
	if !res.Success {
		return nil, fmt.Errorf("账号服拒绝: %s", res.Err)
	}
	body, _ := json.Marshal(map[string]string{"token": res.Token})

	const attempts = 6
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		p, err := dialAndLogin(addr, tcpAddr, name, account, body)
		if err == nil {
			return p, nil
		}
		lastErr = err
		fmt.Printf("玩家 %s 第 %d/%d 次登录未成功（%v），1.5s 后重试\n", name, attempt, attempts, err)
		time.Sleep(1500 * time.Millisecond)
	}
	return nil, fmt.Errorf("重试 %d 次仍失败: %w", attempts, lastErr)
}

// dialAndLogin 是登录的单次尝试：建连 → 发 EMsgLogin → 等回包。
func dialAndLogin(addr, tcpAddr, name, account string, body []byte) (*peer, error) {
	cl, err := client.Dial(addr, tcpAddr)
	if err != nil {
		return nil, fmt.Errorf("连接网关: %w", err)
	}
	p := &peer{name: name, account: account, cl: cl}
	go p.collect()

	reqID, err := cl.Send(client.EMsgLogin, body)
	if err != nil {
		_ = cl.Close()
		return nil, fmt.Errorf("发送登录: %w", err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		// 连接被网关准入直接关掉时 Recv 会投递 Err —— 提前失败，不必等满超时。
		for _, inc := range p.snapshot() {
			if inc.Err != nil {
				_ = cl.Close()
				return nil, fmt.Errorf("登录阶段连接被服务端关闭（多半是网关连接级准入）: %w", inc.Err)
			}
			if inc.RequestID != reqID || inc.MsgID != 0 {
				continue
			}
			var reply struct {
				Success bool   `json:"success"`
				Owner   string `json:"owner"`
				Err     string `json:"err"`
			}
			if json.Unmarshal(inc.Body, &reply) != nil {
				continue
			}
			if !reply.Success {
				_ = cl.Close()
				return nil, fmt.Errorf("登录被拒: %s", reply.Err)
			}
			return p, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cl.Close()
	return nil, fmt.Errorf("等登录回包超时")
}

// mustUint64 把纯数字账号转成场景 objID；非数字账号直接退出（业务侧 sceneObjID 也是这个前提）。
func mustUint64(s string) uint64 {
	var v uint64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil || v == 0 {
		fmt.Printf("账号 %q 不是纯数字，无法与场景 objID 对应\n", s)
		os.Exit(2)
	}
	return v
}
