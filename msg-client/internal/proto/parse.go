// Package proto 解析 clover 引擎与业务的 proto 源（Go 文件），自动提取消息号与
// Request / Reply / Notify 结构体字段，供 msg-client 的 ls / send 自动绑定使用。
//
// 解析规则（约定，无需依赖 clover-server-engine）：
//   - 消息号：`const MsgXxx uint32 = N`（含 0x 十六进制）。
//   - 结构体：`type Xxx struct { ... }` 中带 `json:"tag"` 的字段。
//   - 结构体分类（按名字后缀）：
//   - Request / Req  -> 请求体
//   - Reply  / Resp  -> 回包体
//   - Notify / Push  -> 推送体（服务端 -> 客户端）
//   - 其余            -> 普通/嵌套结构体（仍入库，types 可查）
//   - 消息号关联：base = 去掉 "Msg" 前缀；req=base+Req/Request、reply=base+Reply/Resp、
//     notify=base+Notify/Push；都没有但存在同名结构体时当 reply（如全量同步）。
//
// 给定文件夹后递归扫描 *.go（跳过 _test.go），所以引擎/demo 往里加新消息或结构体，
// CLI 启动即自动识别，无需改 CLI。
package proto

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 描述消息体/结构体的一个字段（来自 json tag）。
type Field struct {
	Name string // Go 字段名
	JSON string // json tag（缺省用字段名小写）
	Type string // Go 类型关键字（string/uint32/int/bool...）
}

// 一个解析到的结构体。
type Struct struct {
	Name   string // 结构体名
	Kind   string // request / reply / notify / other
	Fields []Field
	Source string // engine / demo
	File   string // 来源文件（相对/绝对路径，便于排查）
}

// 一条协议消息的元数据。
type Message struct {
	ID     uint32  // 消息号
	Name   string  // MsgXxx
	Source string  // engine / demo
	Req    []Field // 请求体字段
	Reply  []Field // 回包体字段
	Notify []Field // 推送体字段
	Dir    string  // c2s / s2c / both / ?
}

// proto 解析结果索引。
type Index struct {
	msgs     map[uint32]*Message
	byName   map[string]*Message // 小写消息名 -> 消息
	structs  map[string]*Struct  // 结构体名 -> 结构体
	ordered  []*Message          // 按消息号排序
	sordered []*Struct           // 按名字排序
}

// 是否数值类型（send 时把值解析成 JSON number）。
func (f Field) IsNumericType() bool {
	switch f.Type {
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64":
		return true
	}
	return false
}

// 是否布尔类型。
func (f Field) IsBoolType() bool { return f.Type == "bool" }

// 写入 JSON 时用的 key（优先 json tag，否则字段名小写）。
func (f Field) JSONKey() string {
	if f.JSON != "" {
		return f.JSON
	}
	return strings.ToLower(f.Name)
}

var (
	reConst  = regexp.MustCompile(`const\s+(\w+)\s+uint32\s*=\s*(0x[0-9A-Fa-f]+|\d+)`)
	reStruct = regexp.MustCompile(`type\s+(\w+)\s+struct\s*\{`)
	// 匹配 `Name Type `json:"tag"`` 形式的字段行。
	reField = regexp.MustCompile(`(\w+)\s+([\*]?\w+(?:\[\])?(?:\.[\w]+)?)\s+` + "`" + `json:"([^"]*)"` + "`")
)

// 按名字后缀判断结构体类别。
func structKind(name string) string {
	switch {
	case strings.HasSuffix(name, "Request"), strings.HasSuffix(name, "Req"):
		return "request"
	case strings.HasSuffix(name, "Reply"), strings.HasSuffix(name, "Resp"):
		return "reply"
	case strings.HasSuffix(name, "Notify"), strings.HasSuffix(name, "Push"):
		return "notify"
	}
	return "other"
}

// 递归扫描 businessDirs 下的 *.go（跳过 _test.go），
// 解析消息号与结构体，建立索引。引擎消息通过内置硬编码注入，不依赖引擎源码。
func LoadIndex(businessDirs []string) (*Index, error) {
	idx := &Index{
		msgs:    map[uint32]*Message{},
		byName:  map[string]*Message{},
		structs: map[string]*Struct{},
	}
	// 注入内置引擎消息（不依赖引擎源码目录）
	idx.builtinEngineMessages()
	for _, d := range businessDirs {
		if err := idx.scanDir(d, "demo"); err != nil {
			return nil, err
		}
	}
	idx.associate()
	idx.sortAll()
	return idx, nil
}

// 硬编码引擎内核消息定义。
// 引擎消息稳定且数量少，内置后 CLI 不再需要读取引擎源码目录，
// 方便引擎独立发布后用户仍能正常使用测试工具。
func (idx *Index) builtinEngineMessages() {
	const source = "engine"

	// ── C2S 消息号 ──
	c2s := []struct {
		name string
		id   uint32
	}{
		{"EMsgLogin", 2},
		{"EMsgResumeSession", 3},
		{"EMsgRankQuery", 4},
		{"EMsgBindUDP", 5}, // 客户端 → 网关：以绑定令牌上报常驻裸 UDP 端点
	}
	for _, m := range c2s {
		msg := &Message{ID: m.id, Name: m.name, Source: source}
		idx.msgs[m.id] = msg
		idx.byName[strings.ToLower(m.name)] = msg
	}

	// ── 推送消息号 ──
	push := []struct {
		name string
		id   uint32
	}{
		{"EMsgUDPBindGrant", 6},  // 网关 → 客户端：一次性不可靠通道绑定令牌（S2C 引擎帧）
		{"EMsgQueuePosition", 7}, // 网关 → 客户端：排队位置通知（S2C 引擎帧，限流/满载入队时下发）
		{"EPushPlayerFullSync", 4001},
		{"EPushAlert", 4002},
		{"EPushDataSync", 4003},
		{"EPushRoomTakeover", 4004},
		{"EPushSceneInfo", 4005},
	}
	for _, m := range push {
		msg := &Message{ID: m.id, Name: m.name, Source: source}
		idx.msgs[m.id] = msg
		idx.byName[strings.ToLower(m.name)] = msg
	}

	// ── 结构体定义 ──
	builtin := []*Struct{
		// C2S 请求体
		{Name: "ELoginRequest", Kind: "request", Source: source, Fields: []Field{
			{Name: "Token", Type: "string", JSON: "token"},
		}},
		{Name: "EResumeSessionRequest", Kind: "request", Source: source, Fields: []Field{
			{Name: "PlayerID", Type: "string", JSON: "player_id"},
			{Name: "SessionToken", Type: "string", JSON: "session_token"},
		}},
		{Name: "ERankQueryRequest", Kind: "request", Source: source, Fields: []Field{
			{Name: "Board", Type: "string", JSON: "board"},
			{Name: "Member", Type: "string", JSON: "member"},
			{Name: "Start", Type: "int", JSON: "start"},
			{Name: "Stop", Type: "int", JSON: "stop"},
		}},
		// 回包体
		{Name: "ELoginReply", Kind: "reply", Source: source, Fields: []Field{
			{Name: "Owner", Type: "string", JSON: "owner"},
			{Name: "Token", Type: "string", JSON: "token"},
			{Name: "Success", Type: "bool", JSON: "success"},
			{Name: "Err", Type: "string", JSON: "err"},
			{Name: "SessionKey", Type: "string", JSON: "session_key"},
		}},
		{Name: "EResumeSessionReply", Kind: "reply", Source: source, Fields: []Field{
			{Name: "PlayerID", Type: "string", JSON: "player_id"},
			{Name: "Success", Type: "bool", JSON: "success"},
			{Name: "Err", Type: "string", JSON: "err"},
		}},
		{Name: "ERankQueryReply", Kind: "reply", Source: source, Fields: []Field{
			{Name: "Board", Type: "string", JSON: "board"},
			{Name: "Member", Type: "ERankEntry", JSON: "member"},
			{Name: "MemberFound", Type: "bool", JSON: "member_found"},
			{Name: "Range", Type: "[]ERankEntry", JSON: "range"},
			{Name: "Total", Type: "int", JSON: "total"},
			{Name: "Err", Type: "string", JSON: "err"},
		}},
		{Name: "EErrorReply", Kind: "reply", Source: source, Fields: []Field{
			{Name: "Err", Type: "string", JSON: "err"},
			{Name: "Code", Type: "int", JSON: "code"}, // 机器可读错误码（400/401/403/404/429/500）
		}},
		// 推送体
		{Name: "EPlayerFullSyncNotify", Kind: "notify", Source: source, Fields: []Field{
			{Name: "PlayerID", Type: "string", JSON: "player_id"},
			{Name: "Account", Type: "string", JSON: "account"},
			{Name: "Player", Type: "EPlayerSyncView", JSON: "player"},
			{Name: "AccountInfo", Type: "*EAccountSyncView", JSON: "account_info"},
			{Name: "Data", Type: "map[string]map[string]json.RawMessage", JSON: "data"},
			{Name: "AccountData", Type: "map[string]json.RawMessage", JSON: "account_data"},
			{Name: "SessionToken", Type: "string", JSON: "session_token"},
		}},
		{Name: "EAlertNotify", Kind: "notify", Source: source, Fields: []Field{
			{Name: "Title", Type: "string", JSON: "title"},
			{Name: "Content", Type: "string", JSON: "content"},
			{Name: "Level", Type: "string", JSON: "level"},
			{Name: "Style", Type: "string", JSON: "style"},
			{Name: "TTL", Type: "int", JSON: "ttl"},
		}},
		{Name: "EQueuePositionNotify", Kind: "notify", Source: source, Fields: []Field{
			{Name: "Ahead", Type: "int", JSON: "ahead"},
			{Name: "Total", Type: "int", JSON: "total"},
			{Name: "Ticket", Type: "int64", JSON: "ticket"},
		}},
		{Name: "DataSyncNotify", Kind: "notify", Source: source, Fields: []Field{
			{Name: "Type", Type: "string", JSON: "type"},
			{Name: "Body", Type: "json.RawMessage", JSON: "body"},
		}},
	}
	for _, s := range builtin {
		idx.structs[s.Name] = s
	}
}

func (idx *Index) scanDir(dir, source string) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("proto: 扫描目录 %s 失败: %w", dir, err)
	}
	if !info.IsDir() {
		// 兼容：给的是单个文件也支持。
		return idx.scanFile(dir, source)
	}
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		return idx.scanFile(path, source)
	})
}

func (idx *Index) scanFile(path, source string) error {
	// #nosec G304 -- path 来自配置的业务 proto 目录，由调用方控制。
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	src := string(raw)

	// 消息号常量。
	for _, m := range reConst.FindAllStringSubmatch(src, -1) {
		n, perr := strconv.ParseUint(m[2], 0, 64)
		if perr != nil {
			continue
		}
		name := m[1]
		if !strings.HasPrefix(name, "EMsg") && !strings.HasPrefix(name, "EReply") &&
			!strings.HasPrefix(name, "EPush") && !strings.HasPrefix(name, "Msg") &&
			!strings.HasPrefix(name, "ReplyMsg") && !strings.HasPrefix(name, "Push") {
			continue
		}
		if n > math.MaxUint32 {
			continue
		}
		id := uint32(n)
		if _, exists := idx.msgs[id]; !exists {
			msg := &Message{ID: id, Name: name, Source: source}
			idx.msgs[id] = msg
			idx.byName[strings.ToLower(name)] = msg
		}
	}

	// 结构体。
	for _, m := range reStruct.FindAllStringSubmatchIndex(src, -1) {
		name := src[m[2]:m[3]]
		start := m[1] // 正则已吃掉开括号 '{'，start 在其之后
		depth := 1    // 开括号已被正则匹配，故从 1 起算，找到使 depth 归 0 的闭括号
		end := start
		for i := start; i < len(src); i++ {
			switch src[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = i + 1
					goto closed
				}
			}
		}
	closed:
		body := src[start:end]
		var fields []Field
		for _, fm := range reField.FindAllStringSubmatch(body, -1) {
			tag := fm[3]
			jsonKey := tag
			if comma := strings.Index(tag, ","); comma >= 0 {
				jsonKey = tag[:comma]
			}
			fields = append(fields, Field{Name: fm[1], Type: fm[2], JSON: jsonKey})
		}
		if _, exists := idx.structs[name]; !exists {
			idx.structs[name] = &Struct{
				Name:   name,
				Kind:   structKind(name),
				Fields: fields,
				Source: source,
				File:   path,
			}
		}
	}
	return nil
}

// 把消息号与请求/回包/推送结构体关联起来。
func (idx *Index) associate() {
	for _, msg := range idx.msgs {
		base := msg.Name
		switch {
		case strings.HasPrefix(base, "EReply"):
			base = base[len("EReply"):]
		case strings.HasPrefix(base, "EPush"):
			base = base[len("EPush"):]
		case strings.HasPrefix(base, "EMsg"):
			base = base[len("EMsg"):]
		case strings.HasPrefix(base, "ReplyMsg"):
			base = base[len("ReplyMsg"):]
		case strings.HasPrefix(base, "Push"):
			base = base[len("Push"):]
		case strings.HasPrefix(base, "Msg"):
			base = base[len("Msg"):]
		}
		msg.Req = idx.fieldsOf("E" + base + "Request")
		if msg.Req == nil {
			msg.Req = idx.fieldsOf("E" + base + "Req")
		}
		if msg.Req == nil {
			msg.Req = idx.fieldsOf(base + "Request")
		}
		if msg.Req == nil {
			msg.Req = idx.fieldsOf(base + "Req")
		}
		msg.Reply = idx.fieldsOf("E" + base + "Reply")
		if msg.Reply == nil {
			msg.Reply = idx.fieldsOf(base + "Reply")
		}
		if msg.Reply == nil {
			msg.Reply = idx.fieldsOf(base + "Resp")
		}
		msg.Notify = idx.fieldsOf("E" + base + "Notify")
		if msg.Notify == nil {
			msg.Notify = idx.fieldsOf(base + "Notify")
		}
		if msg.Notify == nil {
			msg.Notify = idx.fieldsOf(base + "Push")
		}
		// 兜底：没有 Req/Reply/Notify 但存在同名结构体（E 前缀优先）
		if len(msg.Req) == 0 && len(msg.Reply) == 0 && len(msg.Notify) == 0 {
			msg.Reply = idx.fieldsOf("E" + base)
		}
		if len(msg.Req) == 0 && len(msg.Reply) == 0 && len(msg.Notify) == 0 {
			msg.Reply = idx.fieldsOf(base)
		}
		switch {
		case len(msg.Req) > 0 && (len(msg.Reply) > 0 || len(msg.Notify) > 0):
			msg.Dir = "both"
		case len(msg.Req) > 0:
			msg.Dir = "c2s"
		case len(msg.Reply) > 0 || len(msg.Notify) > 0:
			msg.Dir = "s2c"
		default:
			msg.Dir = "?"
		}
	}
}

func (idx *Index) fieldsOf(structName string) []Field {
	if s, ok := idx.structs[structName]; ok {
		return s.Fields
	}
	return nil
}

func (idx *Index) sortAll() {
	idx.ordered = make([]*Message, 0, len(idx.msgs))
	for _, m := range idx.msgs {
		idx.ordered = append(idx.ordered, m)
	}
	sort.Slice(idx.ordered, func(i, j int) bool { return idx.ordered[i].ID < idx.ordered[j].ID })

	idx.sordered = make([]*Struct, 0, len(idx.structs))
	for _, s := range idx.structs {
		idx.sordered = append(idx.sordered, s)
	}
	sort.Slice(idx.sordered, func(i, j int) bool { return idx.sordered[i].Name < idx.sordered[j].Name })
}

// 返回全部消息（按消息号排序）。
func (idx *Index) Messages() []*Message { return idx.ordered }

// 返回全部结构体（按名字排序）。
func (idx *Index) Structs() []*Struct { return idx.sordered }

// 按消息号查消息。
func (idx *Index) MessageByID(id uint32) *Message { return idx.msgs[id] }

// 按消息名（大小写不敏感）查消息。
func (idx *Index) MessageByName(name string) *Message { return idx.byName[strings.ToLower(name)] }

// 按结构体名查结构体。
func (idx *Index) StructByName(name string) *Struct { return idx.structs[name] }

// 按名字（大小写不敏感）或数字号查消息。
func (idx *Index) Lookup(target string) *Message {
	if id, err := strconv.ParseUint(target, 10, 32); err == nil {
		return idx.MessageByID(uint32(id))
	}
	return idx.MessageByName(target)
}

// 消息总数。
func (idx *Index) Len() int { return len(idx.msgs) }

// 把字段列表拼成 `k1,k2`（用 json key），空则 "-"。
func FieldList(fs []Field) string {
	if len(fs) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(fs))
	for _, f := range fs {
		parts = append(parts, f.JSONKey())
	}
	return strings.Join(parts, ",")
}

// 根据字段定义，把 `k=v` 参数自动绑定成 JSON 字节。
// 数值/布尔类型做类型转换；未知字段按字符串落盘。
func BuildBody(fields []Field, kv []string) []byte {
	obj := map[string]any{}
	for _, pair := range kv {
		eq := strings.Index(pair, "=")
		if eq < 0 {
			continue
		}
		k := pair[:eq]
		v := pair[eq+1:]
		var f *Field
		for i := range fields {
			if strings.EqualFold(fields[i].JSONKey(), k) || strings.EqualFold(fields[i].Name, k) {
				f = &fields[i]
				break
			}
		}
		if f == nil {
			obj[k] = v
			continue
		}
		switch {
		case f.IsNumericType():
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				obj[f.JSONKey()] = n
			} else {
				obj[f.JSONKey()] = v
			}
		case f.IsBoolType():
			if b, err := strconv.ParseBool(v); err == nil {
				obj[f.JSONKey()] = b
			} else {
				obj[f.JSONKey()] = v
			}
		default:
			obj[f.JSONKey()] = v
		}
	}
	b, _ := json.Marshal(obj)
	return b
}
