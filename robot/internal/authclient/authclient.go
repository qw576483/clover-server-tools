// Package authclient 账号服（auth 服）HTTP 客户端。
//
// 登录体系外置后，客户端流程是两步：
//
//  1. POST /auth/login  {account, password} → JWT
//  2. 连游戏网关 → EMsgLogin{token: <JWT>}  → owner
//
// 本包只负责第 1 步；第 2 步由 internal/client 走长连接。
//
// 与 msg-client/internal/authclient 的差别只有一处，但很重要：
// **HTTP 连接池调大了**。Go 默认的 http.Transport 是 MaxIdleConnsPerHost=2，
// 几百个机器人同时换 token 时会退化成一两条连接串行往返 —— 压测结果会变成
// 「测自己的连接池」，而不是测账号服。这个坑很隐蔽，特意在此说明。
package authclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultTimeout 单次 HTTP 请求超时。
	defaultTimeout = 10 * time.Second
	// maxRespBytes 响应体读取上限（防异常服务端返回超大 body 打爆内存）。
	maxRespBytes = 64 << 10

	// 账号服接口路径（与引擎 internal/domain/auth/state/wire.go 对齐）。
	pathSignup = "/auth/signup"
	pathLogin  = "/auth/login"
	pathHealth = "/auth/health"
)

// Client 账号服客户端。**并发安全**：所有机器人共用一个实例。
type Client struct {
	baseURL string
	http    *http.Client
}

// Result 注册 / 登录结果（字段与账号服响应一致）。
type Result struct {
	Success bool   `json:"success"`
	Owner   string `json:"owner"`
	Token   string `json:"token"`
	Err     string `json:"err"`
	// Exp token 过期时间（Unix 秒）；0 表示账号服未返回。
	Exp int64 `json:"exp"`
}

// Health /auth/health 的响应体。
type Health struct {
	OK      bool   `json:"ok"`
	Service string `json:"service"`
	Time    int64  `json:"time"`
}

// New 构造客户端。baseURL 形如 http://127.0.0.1:8051（结尾斜杠会被裁掉）。
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http: &http.Client{
			Timeout: defaultTimeout,
			Transport: &http.Transport{
				// 压测关键：默认值（每 host 2 条空闲连接）会把并发请求串行化。
				MaxIdleConns:        4096,
				MaxIdleConnsPerHost: 4096,
				MaxConnsPerHost:     0, // 不限并发连接数
				IdleConnTimeout:     30 * time.Second,
				// 不禁用 keep-alive：换 token 是短请求，复用连接能显著降低
				// 压测自身的端口消耗。
				DisableKeepAlives: false,
			},
		},
	}
}

// BaseURL 返回账号服地址。
func (c *Client) BaseURL() string { return c.baseURL }

// Signup 注册。账号服注册成功即签发 token。
func (c *Client) Signup(account, password string) (*Result, error) {
	return c.post(pathSignup, account, password)
}

// Login 登录。
func (c *Client) Login(account, password string) (*Result, error) {
	return c.post(pathLogin, account, password)
}

// Health 查询账号服存活探针。
func (c *Client) Health() (*Health, error) {
	resp, err := c.http.Get(c.baseURL + pathHealth)
	if err != nil {
		return nil, fmt.Errorf("连接账号服失败（%s）: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, fmt.Errorf("读取账号服响应失败: %w", err)
	}
	var h Health
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("账号服响应无法解析（HTTP %d）: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return &h, nil
}

// post 发送凭证并解析结果。
//
// 返回 (result, nil) 表示 HTTP 链路正常 —— **业务失败**（账号不存在 / 密码错）
// 体现在 result.Success=false + result.Err，由调用方决定怎么处理；
// 返回 (nil, err) 只表示链路层问题（连不上 / 响应无法解析）。
// 区分这两者很重要：压测报告要能分清「账号服没起来」和「账号密码不对」。
func (c *Client) post(path, account, password string) (*Result, error) {
	payload, err := json.Marshal(map[string]string{"account": account, "password": password})
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	resp, err := c.http.Post(c.baseURL+path, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("连接账号服失败（%s）: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, fmt.Errorf("读取账号服响应失败: %w", err)
	}
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("账号服响应无法解析（HTTP %d）: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	// 账号服已用 HTTP 状态码表达语义；补一层兜底，避免空 Err 让报告里出现
	// "失败但没原因"。
	if !r.Success && r.Err == "" {
		r.Err = fmt.Sprintf("账号服返回 HTTP %d", resp.StatusCode)
	}
	return &r, nil
}
