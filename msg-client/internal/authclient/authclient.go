// Package authclient 账号服（auth 服）HTTP 客户端。
//
// 登录体系外置后，客户端流程从一步变两步：
//
//	1) POST /auth/login   {account, password}  → JWT
//	2) 连游戏网关 → EMsgLogin{token: <JWT>}    → owner
//
// 本包只负责第 1 步（HTTP 请求-响应）；第 2 步仍由 internal/client 走长连接。
// 登录是低频操作、且账号服天然是 HTTP 服务，两者不该挤在同一条长连接上。
//
// 刻意不依赖引擎包：请求路径与 JSON 结构在本文件内自描述，
// 使本工具保持可独立编译（与 internal/proto 的做法一致）。
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

// defaultTimeout 单次 HTTP 请求超时。登录是交互式操作，超过这个时间用户已经在等了。
const defaultTimeout = 10 * time.Second

// maxRespBytes 响应体读取上限（防异常服务端返回超大 body 打爆内存）。
const maxRespBytes = 64 << 10

// Client 账号服客户端。
type Client struct {
	baseURL string
	http    *http.Client
}

// Result 注册 / 登录结果（字段与账号服 authTokenResp 一致）。
type Result struct {
	Success bool   `json:"success"`
	Owner   string `json:"owner"`
	Token   string `json:"token"`
	Err     string `json:"err"`
	// Exp token 过期时间（Unix 秒）；0 表示账号服未返回。
	Exp int64 `json:"exp"`
}

// New 构造客户端。baseURL 形如 http://127.0.0.1:8051（结尾斜杠会被裁掉）。
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    &http.Client{Timeout: defaultTimeout},
	}
}

// Signup 注册。账号服注册成功即签发 token，无需再调 Login。
func (c *Client) Signup(account, password string) (*Result, error) {
	return c.post("/auth/signup", account, password)
}

// Login 登录。
func (c *Client) Login(account, password string) (*Result, error) {
	return c.post("/auth/login", account, password)
}

// post 发送凭证并解析结果。
//
// 返回 (result, nil) 表示 HTTP 链路正常——**业务失败**（账号密码错等）体现在
// result.Success=false + result.Err，由调用方决定如何提示；
// 返回 (nil, err) 仅表示链路层问题（连不上 / 响应无法解析）。
// 这样调用方能区分「账号不对」和「账号服没起来」。
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
	// 账号服已用 HTTP 状态码表达语义；这里补一层兜底，避免空 Err 让用户看到"失败但没原因"。
	if !r.Success && r.Err == "" {
		r.Err = fmt.Sprintf("账号服返回 HTTP %d", resp.StatusCode)
	}
	return &r, nil
}
