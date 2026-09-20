// Package adminclient 调引擎内置 admin 控制面，是 manager「操作」的一侧。
//
// 端点定义见 clover-server-engine/internal/app/drain_admin.go 与 admin.go：
//
//	GET  /ping                     自检（进程活着 + 已注册路由）
//	GET  /admin/drain/status       灰度下线状态
//	POST /admin/drain              发起灰度下线
//	POST /admin/drain/cancel       取消灰度下线
//	GET  /admin/gateway/upstream   查询网关默认上游
//	POST /admin/gateway/upstream?addr=host:port  切换网关默认上游
//	POST /admin/shutdown           程序化优雅退出
package adminclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxRespBytes 单次响应体读取上限，防止异常端点撑爆内存。
const maxRespBytes = 1 << 20

// TokenHeader 令牌请求头名，与引擎侧 adminTokenHeader 保持一致
// （[`clover-server-engine/internal/app/admin.go`](https://github.com/qw576483/clover-server-engine/blob/main/internal/app/admin.go)，引擎同时接受 `Authorization: Bearer <token>`）。
const TokenHeader = "X-Admin-Token"

// Client 单个节点 admin 控制面的 HTTP 客户端。
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New 构造客户端。addr 为 host:port（可带 http:// 前缀）；token 为空表示不发令牌头。
//
// 令牌在**每次请求**都带上（含 /ping、/routes、/metrics 这些不设门禁的只读端点）：
// 按路径决定发不发，一旦引擎侧 guardedAdminPaths 增加前缀就会漏带而 401；
// 目标主机与端口由 base 固定，不存在把令牌发给第三方的路径。
func New(addr string, timeout time.Duration, token string) *Client {
	base := strings.TrimSpace(addr)
	if base == "" {
		base = "127.0.0.1:8041"
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{
		base:  strings.TrimSuffix(base, "/"),
		token: token,
		hc:    &http.Client{Timeout: timeout},
	}
}

// Addr 返回实际的 base 地址（含 scheme）。
func (c *Client) Addr() string { return c.base }

// Ping admin 自检回包。
type Ping struct {
	Status string   `json:"status"`
	Routes []string `json:"routes"`
	Time   string   `json:"time"`
}

// DrainStatus 一次灰度下线的状态（字段与引擎 DrainStatus 对齐）。
type DrainStatus struct {
	Draining  bool      `json:"draining"`
	Mode      string    `json:"mode"`
	Target    string    `json:"target"`
	Phase     string    `json:"phase"`
	Remaining int       `json:"remaining"`
	Migrated  int       `json:"migrated"`
	Kicked    int       `json:"kicked"`
	StartedAt time.Time `json:"started_at"`
	Deadline  time.Time `json:"deadline"`
}

// DrainRequest 发起灰度下线的参数（字段与引擎 drainRequest 对齐）。
// 零值字段不下发，由引擎侧 DrainOptions.normalize 兜默认值。
type DrainRequest struct {
	Mode         string `json:"mode,omitempty"`
	Target       string `json:"target,omitempty"`
	Grace        string `json:"grace,omitempty"`
	HardTimeout  string `json:"hard_timeout,omitempty"`
	MigrateBatch int    `json:"migrate_batch,omitempty"`
	KickBatch    int    `json:"kick_batch,omitempty"`
	StopAfter    bool   `json:"stop_after,omitempty"`
}

// Ping 调 GET /ping。
func (c *Client) Ping(ctx context.Context) (*Ping, error) {
	var out Ping
	if err := c.do(ctx, http.MethodGet, "/ping", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DrainStatus 调 GET /admin/drain/status。
func (c *Client) DrainStatus(ctx context.Context) (*DrainStatus, error) {
	var out DrainStatus
	if err := c.do(ctx, http.MethodGet, "/admin/drain/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Drain 调 POST /admin/drain 发起灰度下线。
func (c *Client) Drain(ctx context.Context, req DrainRequest) (*DrainStatus, error) {
	var out DrainStatus
	if err := c.do(ctx, http.MethodPost, "/admin/drain", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelDrain 调 POST /admin/drain/cancel 取消灰度下线（回滚）。
func (c *Client) CancelDrain(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/admin/drain/cancel", nil, nil)
}

// Upstream 调 GET /admin/gateway/upstream。
func (c *Client) Upstream(ctx context.Context) (string, error) {
	var out struct {
		Upstream string `json:"upstream"`
	}
	if err := c.do(ctx, http.MethodGet, "/admin/gateway/upstream", nil, &out); err != nil {
		return "", err
	}
	return out.Upstream, nil
}

// SetUpstream 调 POST /admin/gateway/upstream?addr=... 切换网关默认上游。
func (c *Client) SetUpstream(ctx context.Context, addr string) (string, error) {
	if addr == "" {
		return "", fmt.Errorf("adminclient: upstream addr required")
	}
	path := "/admin/gateway/upstream?addr=" + url.QueryEscape(addr)
	var out struct {
		Upstream string `json:"upstream"`
	}
	if err := c.do(ctx, http.MethodPost, path, nil, &out); err != nil {
		return "", err
	}
	return out.Upstream, nil
}

// Shutdown 调 POST /admin/shutdown 请求进程优雅退出。
func (c *Client) Shutdown(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/admin/shutdown", nil, nil)
}

// do 发起一次 admin 请求并解析回包。非 2xx 时把引擎 {"error":...} 体原样带出。
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("adminclient: encode request body: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("adminclient: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// 令牌头：只在配置了令牌时设置（空 = 该节点未启用鉴权，多发一个空头没意义）。
	// 注意别把它写进日志或错误文案 —— 错误里只回显状态码与响应体。
	if c.token != "" {
		req.Header.Set(TokenHeader, c.token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("adminclient: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 401 单独给可操作的提示：这是最容易被误判成「节点挂了」的分支
		// （节点活着、只是我们的令牌不对 / 没带）。提示里不含令牌本身。
		if resp.StatusCode == http.StatusUnauthorized {
			hint := "（节点启用了 admin.token：请把同一令牌填进 manager 配置的 admin.token）"
			if c.token != "" {
				hint = "（manager 配置的 admin.token 与节点不一致）"
			}
			return fmt.Errorf("adminclient: %s %s: HTTP %d: %s %s",
				method, path, resp.StatusCode, extractError(raw), hint)
		}
		return fmt.Errorf("adminclient: %s %s: HTTP %d: %s", method, path, resp.StatusCode, extractError(raw))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("adminclient: %s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

// extractError 取出引擎统一错误体 {"error": "..."} 的文案；解析不出就回退原文。
func extractError(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err == nil && e.Error != "" {
		return e.Error
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "(empty response)"
	}
	return s
}
