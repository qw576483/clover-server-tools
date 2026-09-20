// Package modules 是 gmt 的**模块声明层**。
//
// 老后台（ThinkPHP 版）是「一个功能一套 controller + 一套 view + 一套 JS」：
// 二十多个功能各写一份列表页、分页与查询表单，view 目录里因此堆了一百多个模板，
// 改一个交互要挨个改。这里换成声明式模型，让「一样的东西」只写一遍。
//
// 这里换成一个声明式模型：模块只描述**自己长什么样**（有哪些列、哪些查询条件、
// 表单有哪些字段、行上有哪些按钮）和**数据从哪来**（几个回调）。
// 列表页、查询、分页、新增/编辑弹窗、删除确认全部由一套通用实现承担：
//
//	GET  /m/{key}              通用列表页
//	GET  /api/m/{key}/schema   页面要的元信息（列/表单/按钮）
//	POST /api/m/{key}/list     分页数据
//	POST /api/m/{key}/save     新增/保存
//	POST /api/m/{key}/delete   删除
//	POST /api/m/{key}/action   自定义行操作（解封、推送…）
//
// 新增一个模块 = 写一段声明，不需要再写页面和 JS。
package modules

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qw576483/clover-server-tools/gmt/internal/gameclient"
	"github.com/qw576483/clover-server-tools/gmt/internal/store"
)

// Ctx 是模块回调能拿到的全部上下文。
//
// 传这个结构而不是直接传 *gin.Context，是为了让模块代码不依赖 HTTP 细节：
// 以后要在定时任务或命令行里复用同一段业务逻辑，不用改签名。
type Ctx struct {
	Gin    *gin.Context
	Store  *store.Client
	Game   gameclient.Client
	User   *store.Account
	Secret string // 配置的 secret，口令散列要用
	// Type 是操作日志里的「模块」字段，由模块自身声明带过来。
	Type string
}

// Audit 记一条操作日志（谁在哪个模块做了什么）。
//
// 所有写操作都必须调它——后台的每一次改动要能追溯，这是运营后台的底线。
func (c *Ctx) Audit(subType, info string) {
	name := ""
	if c.User != nil {
		name = c.User.Name
	}
	_, _ = store.Put[*store.Audit](c.Store, store.Audits, &store.Audit{
		Time:    store.Now(),
		Account: name,
		IP:      c.Gin.ClientIP(),
		Type:    c.Type,
		SubType: subType,
		Info:    info,
	})
}

// ---- 页面元信息 ----

// Column 是列表里的一列。
type Column struct {
	Key   string            `json:"key"`
	Title string            `json:"title"`
	Kind  string            `json:"kind"` // text | badge | time | money
	Badge map[string]string `json:"badge,omitempty"`
	// Labels 把存的值翻译成人话（1→正常）。没有它，页面上会出现一列看不懂的数字。
	Labels map[string]string `json:"labels,omitempty"`
	Width  string            `json:"width,omitempty"`
}

// Option 是下拉框的一个选项。
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Field 是查询条件 / 表单的一个字段。
type Field struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"` // text | number | password | select | textarea | datetime
	Options     []Option `json:"options,omitempty"`
	Required    bool     `json:"required"`
	Placeholder string   `json:"placeholder,omitempty"`
	// Default 是新增时的默认值（编辑时不用）。
	Default string `json:"default,omitempty"`
	// Multiple 用于 select：true 时前端渲染成多选（值以数组提交）。
	Multiple bool `json:"multiple,omitempty"`
	// OptionsFrom 给「选项来自数据」的字段用（比如角色下拉）。
	// 返回 schema 时由它现算，避免模块注册时就把数据快照死。
	OptionsFrom func(c *Ctx) []Option `json:"-"`
}

// Action 是行上的自定义按钮。
type Action struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Style   string `json:"style"`   // primary | danger | warning
	Confirm string `json:"confirm"` // 非空则点击后先弹确认
}

// Query 是列表查询参数。
type Query struct {
	Page   int               `json:"page"`
	Size   int               `json:"size"`
	Search map[string]string `json:"search"`
}

// S 取查询条件（字符串）。不存在的条件返回空串，等价于「不过滤」。
func (q Query) S(key string) string { return q.Search[key] }

// I 取查询条件（整数）。
func (q Query) I(key string) int {
	n, _ := strconv.Atoi(q.Search[key])
	return n
}

// I64 取查询条件（64 位整数）。
func (q Query) I64(key string) int64 {
	n, _ := strconv.ParseInt(q.Search[key], 10, 64)
	return n
}

// Row 是一行数据。约定必须带 "id"（行操作靠它定位）。
type Row map[string]any

// Values 是表单提交上来的键值对。
//
// JSON 解出来的数字统一是 float64，所以取值函数要同时能吃字符串和数字，
// 否则「前端传 1」和「前端传 "1"」会写出两套分支。
type Values map[string]any

func (v Values) Str(key string) string {
	switch x := v[key].(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprint(x)
	}
}

func (v Values) Int(key string) int { return int(v.Int64(key)) }

func (v Values) Int64(key string) int64 {
	switch x := v[key].(type) {
	case float64:
		return int64(x)
	case string:
		if x == "" {
			return 0
		}
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case bool:
		if x {
			return 1
		}
		return 0
	default:
		return 0
	}
}

func (v Values) Bool(key string) bool {
	switch x := v[key].(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x == "1" || strings.EqualFold(x, "true") || strings.EqualFold(x, "on")
	default:
		return false
	}
}

// Time 解析时间字段：优先当时间戳，其次按常见格式解析字符串。
//
// 前端日期控件配置的是「显示给人看、提交时间戳」，但手填、脚本调用、旧数据
// 都可能给字符串，所以两种都得吃下。
func (v Values) Time(key string) int64 {
	if n := v.Int64(key); n > 0 {
		return n
	}
	s := strings.TrimSpace(v.Str(key))
	if s == "" {
		return 0
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02", "2006/01/02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// Int64Slice 解析 "1,2,3" 或数组形式的道具/玩家 ID 列表。
func (v Values) Int64Slice(key string) []int64 {
	switch x := v[key].(type) {
	case []any:
		out := make([]int64, 0, len(x))
		for _, it := range x {
			out = append(out, Values{"v": it}.Int64("v"))
		}
		return out
	case string:
		out := []int64{}
		for _, p := range strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == '，' || r == ' ' || r == '\n' }) {
			if n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
				out = append(out, n)
			}
		}
		return out
	default:
		return nil
	}
}

// Module 是一个功能模块的完整声明。
type Module struct {
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	Icon     string   `json:"icon"`
	Group    string   `json:"group"`
	ReadOnly bool     `json:"read_only"`
	Search   []Field  `json:"search"`
	Columns  []Column `json:"columns"`
	Form     []Field  `json:"form"`
	Actions  []Action `json:"actions"`

	// Type 用于操作日志归类，默认取 Key。
	Type string `json:"-"`

	// 下面四个是行为，不是数据，不能被序列化成 schema 给前端（Go 的函数
	// 也没法 JSON 编码，忘记加 `json:"-"` 会让 schema 接口静默返回空 body）。
	List   func(c *Ctx, q Query) ([]Row, int, error) `json:"-"`
	Save   func(c *Ctx, v Values) error              `json:"-"`
	Delete func(c *Ctx, id int64) error              `json:"-"`
	// Do 处理自定义行操作；action 为 Action.Key。
	// 返回值会原样给前端（比如「生成礼包码」要返回码列表让人复制）。
	Do func(c *Ctx, action string, id int64, v Values) (any, error) `json:"-"`
}

// Page 是一个**自定义页面**（不是通用列表）。
//
// 只有通用列表模型表达不了的页面才需要它：仪表盘、玩家详情、发邮件。
// 它们的路由和菜单与其他模块一致，只是页面自己写。
type Page struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Icon  string `json:"icon"`
	Group string `json:"group"`
	Path  string `json:"path"`
}

var pages []Page

// RegisterPage 注册自定义页面。
func RegisterPage(p Page) { pages = append(pages, p) }

// Pages 返回全部自定义页面。
func Pages() []Page { return pages }

// ---- 注册表 ----

var (
	registry = map[string]*Module{}
	order    []string
)

// Register 注册模块。重复注册同一个 key 直接 panic——
// 这是启动期就能发现的配置错误，不该等到页面上少一个菜单才察觉。
func Register(m *Module) {
	if _, ok := registry[m.Key]; ok {
		panic("modules: 重复注册模块 " + m.Key)
	}
	if m.Type == "" {
		m.Type = m.Key
	}
	registry[m.Key] = m
	order = append(order, m.Key)
}

// All 返回按注册顺序排列的模块。
func All() []*Module {
	out := make([]*Module, 0, len(order))
	for _, k := range order {
		out = append(out, registry[k])
	}
	return out
}

// ByKey 按键取模块。
func ByKey(key string) (*Module, bool) {
	m, ok := registry[key]
	return m, ok
}
