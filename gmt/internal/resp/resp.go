// Package resp 定义前后端之间的统一响应体。
//
// 全部接口（含错误）一律回 200 + {code,message,data}：
//   - 前端只有一个解析分支，不用为「HTTP 错误」和「业务错误」写两套逻辑；
//   - 网关/浏览器不会把业务错误当成网络故障重试。
//
// code 的取值只有四个，够用且好记：0 成功 / 1 业务失败 / 401 未登录 / 403 没权限。
package resp

import "fmt"

const (
	CodeOK      = 0
	CodeFail    = 1
	CodeNoLogin = 401
	CodeNoPerm  = 403
)

// Result 是所有接口的返回体。
type Result struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// OK 成功返回。
func OK(data any) Result { return Result{Code: CodeOK, Data: data} }

// Fail 业务失败。
func Fail(msg string) Result { return Result{Code: CodeFail, Message: msg} }

// Failf 带格式化的业务失败。
func Failf(format string, a ...any) Result {
	return Result{Code: CodeFail, Message: fmt.Sprintf(format, a...)}
}

// Err 把 error 转成失败响应。
func Err(err error) Result { return Result{Code: CodeFail, Message: err.Error()} }

// NoLogin 未登录（前端收到后跳登录页）。
func NoLogin() Result { return Result{Code: CodeNoLogin, Message: "未登录或会话已过期"} }

// NoPerm 无权限。
func NoPerm() Result { return Result{Code: CodeNoPerm, Message: "没有该功能的访问权限"} }

// List 分页列表的返回体。
type List struct {
	Rows  any `json:"rows"`
	Total int `json:"total"`
	Page  int `json:"page"`
	Size  int `json:"size"`
}
