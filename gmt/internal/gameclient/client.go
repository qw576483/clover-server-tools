// Package gameclient 是 gmt 访问**游戏侧**的唯一出口。
//
// 为什么要这一层：
//   - 节点地址、超时、错误码解析只在这里出现一次。业务模块不拼 URL、不管超时，
//     换一个服的地址只改配置。
//   - 有了接口就能换实现：`mock` 不发真实请求，返回可预期的假数据，
//     于是后台在没有游戏服的机器上也能完整点通、做前端联调、给人演示。
//
// 接口方法按「后台要做什么」组织，不按「游戏服有哪些接口」组织——
// 后台不该关心游戏内部的模块划分。
package gameclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"gmt/internal/conf"
)

// NodeHealth 是一个游戏侧节点的连通性。
type NodeHealth struct {
	Node    string `json:"node"`
	Addr    string `json:"addr"`
	Online  bool   `json:"online"`
	Latency int64  `json:"latency_ms"`
	Message string `json:"message"`
}

// Item 是邮件/奖励里的道具。
type Item struct {
	ConfigID int64 `json:"config_id"`
	Num      int64 `json:"num"`
}

// Player 是角色详情。
type Player struct {
	PlayerID   int64  `json:"player_id"`
	Name       string `json:"name"`
	Account    string `json:"account"`
	ServerID   int64  `json:"server_id"`
	Level      int    `json:"level"`
	VIP        int    `json:"vip"`
	Power      int64  `json:"power"`
	Guild      string `json:"guild"`
	Online     bool   `json:"online"`
	CreateTime int64  `json:"create_time"`
	LastLogin  int64  `json:"last_login"`
	Items      []Item `json:"items"`
}

// MailRequest 是发邮件请求。
type MailRequest struct {
	ServerID int64   `json:"server_id"`
	All      bool    `json:"all"`
	MinLevel int     `json:"min_level"`
	Targets  []int64 `json:"targets"`
	Title    string  `json:"title"`
	Content  string  `json:"content"`
	Items    []Item  `json:"items"`
}

// Bulletin 是公告。
type Bulletin struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"` // login 登录公告 / roll 滚动公告
	Title    string `json:"title"`
	Content  string `json:"content"`
	StartAt  int64  `json:"start_at"`
	EndAt    int64  `json:"end_at"`
	Interval int    `json:"interval"`
	Enabled  bool   `json:"enabled"`
}

// BanRequest 是封禁/解封请求。
type BanRequest struct {
	Kind     string `json:"kind"` // account 封账号 / role 封角色 / chat 禁言
	Target   string `json:"target"`
	PlayerID int64  `json:"player_id"`
	ServerID int64  `json:"server_id"`
	Until    int64  `json:"until"`
	Reason   string `json:"reason"`
}

// GiftRequest 是生成礼包码的请求。
type GiftRequest struct {
	Batch    string `json:"batch"`
	Prefix   string `json:"prefix"`
	Count    int    `json:"count"`
	Kind     string `json:"kind"`
	ServerID int64  `json:"server_id"`
	ExpireAt int64  `json:"expire_at"`
	Items    []Item `json:"items"`
}

// OrderQuery 是订单查询条件。
type OrderQuery struct {
	ServerID int64  `json:"server_id"`
	Account  string `json:"account"`
	PlayerID int64  `json:"player_id"`
	OrderID  string `json:"order_id"`
	Status   int    `json:"status"`
	From     int64  `json:"from"`
	To       int64  `json:"to"`
	Page     int    `json:"page"`
	Size     int    `json:"size"`
}

// Order 是充值订单。
type Order struct {
	OrderID  string `json:"order_id"`
	Account  string `json:"account"`
	PlayerID int64  `json:"player_id"`
	ServerID int64  `json:"server_id"`
	Product  string `json:"product"`
	Amount   int64  `json:"amount"`
	Status   int    `json:"status"`
	Channel  string `json:"channel"`
	Time     int64  `json:"time"`
}

// Client 是后台对游戏侧的全部依赖。
type Client interface {
	// Driver 返回当前实现名，页面上会显示（让人一眼知道数据是不是真的）。
	Driver() string
	// Health 探测各节点连通性，用于首页。
	Health(ctx context.Context) []NodeHealth
	// Servers 返回区服列表（游戏侧为准）；mock 下返回样例。
	SearchPlayer(ctx context.Context, serverID int64, kind, keyword string) (*Player, error)
	SendMail(ctx context.Context, req MailRequest) error
	ListBulletins(ctx context.Context) ([]Bulletin, error)
	SaveBulletin(ctx context.Context, b Bulletin) error
	DeleteBulletin(ctx context.Context, id int64) error
	ApplyBan(ctx context.Context, req BanRequest) error
	LiftBan(ctx context.Context, req BanRequest) error
	GenGift(ctx context.Context, req GiftRequest) ([]string, error)
	QueryOrders(ctx context.Context, q OrderQuery) ([]Order, error)
}

// New 按配置创建客户端。
func New(cfg *conf.Config) Client {
	if cfg.Remote.Driver == "http" {
		return &httpClient{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout()}}
	}
	return &mockClient{cfg: cfg}
}

// ---- HTTP 实现 ----
//
// 与游戏侧约定的调用方式：POST JSON 到 {节点地址}{path}，返回体也是
// {code,message,data}。任何非 0 code 都当成失败，把 message 原样抛给页面——
// 后台不做「猜错误码」的事，避免把游戏侧的真实原因吞掉。

type httpClient struct {
	cfg  *conf.Config
	http *http.Client
}

func (c *httpClient) Driver() string { return "http" }

// call 发起一次调用并把 data 解到 out（out 可为 nil）。
func (c *httpClient) call(ctx context.Context, node, path string, in, out any) error {
	base, ok := c.cfg.Remote.Endpoints[node]
	if !ok || base == "" {
		return fmt.Errorf("未配置节点 %s 的地址", node)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("调用 %s%s 失败: %w", node, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var r struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("解析 %s%s 返回失败: %w", node, path, err)
	}
	if r.Code != 0 {
		if r.Message == "" {
			r.Message = fmt.Sprintf("%s 返回 code=%d", path, r.Code)
		}
		return fmt.Errorf("%s", r.Message)
	}
	if out != nil && len(r.Data) > 0 {
		if err := json.Unmarshal(r.Data, out); err != nil {
			return fmt.Errorf("解析 %s%s 的 data 失败: %w", node, path, err)
		}
	}
	return nil
}

func (c *httpClient) Health(ctx context.Context) []NodeHealth {
	out := make([]NodeHealth, 0, len(c.cfg.Remote.Endpoints))
	for node, addr := range c.cfg.Remote.Endpoints {
		h := NodeHealth{Node: node, Addr: addr}
		start := time.Now()
		if err := c.call(ctx, node, "/ping", nil, nil); err != nil {
			h.Message = err.Error()
		} else {
			h.Online = true
			h.Latency = time.Since(start).Milliseconds()
		}
		out = append(out, h)
	}
	return out
}

func (c *httpClient) SearchPlayer(ctx context.Context, serverID int64, kind, keyword string) (*Player, error) {
	var p Player
	in := map[string]any{"server_id": serverID, "kind": kind, "keyword": keyword}
	if err := c.call(ctx, "game", "/gm/player/search", in, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *httpClient) SendMail(ctx context.Context, req MailRequest) error {
	return c.call(ctx, "game", "/gm/mail/send", req, nil)
}

func (c *httpClient) ListBulletins(ctx context.Context) ([]Bulletin, error) {
	var out []Bulletin
	if err := c.call(ctx, "coop", "/gm/bulletin/list", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *httpClient) SaveBulletin(ctx context.Context, b Bulletin) error {
	return c.call(ctx, "coop", "/gm/bulletin/save", b, nil)
}

func (c *httpClient) DeleteBulletin(ctx context.Context, id int64) error {
	return c.call(ctx, "coop", "/gm/bulletin/delete", map[string]any{"id": id}, nil)
}

// ApplyBan 按类型分发：账号封禁在 auth 服，角色/禁言在 game 服。
func (c *httpClient) ApplyBan(ctx context.Context, req BanRequest) error {
	node := "game"
	if req.Kind == "account" {
		node = "auth"
	}
	return c.call(ctx, node, "/gm/ban/apply", req, nil)
}

func (c *httpClient) LiftBan(ctx context.Context, req BanRequest) error {
	node := "game"
	if req.Kind == "account" {
		node = "auth"
	}
	return c.call(ctx, node, "/gm/ban/lift", req, nil)
}

func (c *httpClient) GenGift(ctx context.Context, req GiftRequest) ([]string, error) {
	var codes []string
	if err := c.call(ctx, "auth", "/gm/gift/gen", req, &codes); err != nil {
		return nil, err
	}
	return codes, nil
}

func (c *httpClient) QueryOrders(ctx context.Context, q OrderQuery) ([]Order, error) {
	var out []Order
	if err := c.call(ctx, "auth", "/gm/order/query", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}
