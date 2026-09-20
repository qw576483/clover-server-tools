// Package store 是 gmt **自身**数据的存储层。
//
// 边界：gmt 只存「后台自己的东西」——账号、角色、菜单、机器、区服、封禁留档、
// 礼包批次、操作日志。玩家、邮件、公告、订单这些业务数据属于游戏侧，
// 由 internal/gameclient 去取，本地不留副本。
//
// 连接与配置对齐 clover-server-engine（见 mysql.go / schema.go）：
// 同一个驱动与 yaml 键名、显式 DDL、表名不带前缀、时间列用 DATETIME。
// 上层只用本文件的泛型函数 All/Get/Put/Del，不直接拼 SQL。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Row 是一行数据（列名 → 值）。
type Row = map[string]any

// Table 是一张表的句柄：表名 + 列（列从结构体 tag 推导，只用于建表漂移检查与标识符处理）。
type Table[T Entity] struct {
	Name    string
	Columns []string
	// timeCols 是 `col:"time"` 的列：库里 DATETIME，Go 侧 int64 秒。
	timeCols map[string]bool
}

var (
	Accounts = newTable[*Account]("account")
	Roles    = newTable[*Role]("role")
	Menus    = newTable[*Menu]("menu")
	Machines = newTable[*Machine]("machine")
	Servers  = newTable[*Server]("server")
	Bans     = newTable[*Ban]("ban")
	Gifts    = newTable[*GiftBatch]("gift")
	Audits   = newTable[*Audit]("audit")
)

// ---- 泛型增删查改 ----

// All 返回整张表（按主键升序，保证列表顺序稳定）。
func All[T Entity](c *Client, t *Table[T]) ([]T, error) {
	rows, err := c.Query(context.Background(), "SELECT * FROM "+quoteIdent(t.Name)+" ORDER BY `id`")
	if err != nil {
		return nil, fmt.Errorf("store: query %s: %w", t.Name, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, 16)
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("store: scan %s: %w", t.Name, err)
		}
		row := make(Row, len(cols))
		for i, col := range cols {
			row[col] = vals[i]
		}
		out = append(out, scan(t, row))
	}
	return out, rows.Err()
}

// Get 按主键取一条。
func Get[T Entity](c *Client, t *Table[T], id int64) (T, bool, error) {
	all, err := All(c, t)
	if err != nil {
		var zero T
		return zero, false, err
	}
	for _, it := range all {
		if it.GetID() == id {
			return it, true, nil
		}
	}
	var zero T
	return zero, false, nil
}

// Put 新增（id==0）或整行更新（id!=0）。
func Put[T Entity](c *Client, t *Table[T], v T) (T, error) {
	if v.GetID() == 0 {
		row := rowOf(t, v)
		delete(row, "id")
		id, err := insert(c, t, row)
		if err != nil {
			return v, err
		}
		v.SetID(id)
		return v, nil
	}
	if err := update(c, t, v.GetID(), rowOf(t, v)); err != nil {
		return v, err
	}
	return v, nil
}

// Del 按主键删除。
func Del[T Entity](c *Client, t *Table[T], id int64) (bool, error) {
	res, err := c.Exec(context.Background(),
		"DELETE FROM "+quoteIdent(t.Name)+" WHERE `id` = ?", id)
	if err != nil {
		return false, fmt.Errorf("store: delete %s(id=%d): %w", t.Name, id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Now 返回当前秒级时间戳（统一入口，便于将来整体切换时间源）。
func Now() int64 { return time.Now().Unix() }

// ---- SQL 拼装 ----

// insert 插入一行并返回自增主键。
func insert[T Entity](c *Client, t *Table[T], row Row) (int64, error) {
	cols := sortedCols(row, false)
	holders := make([]string, len(cols))
	args := make([]any, len(cols))
	for i, name := range cols {
		holders[i] = "?"
		args[i] = row[name]
	}
	q := "INSERT INTO " + quoteIdent(t.Name) +
		" (`" + strings.Join(cols, "`, `") + "`) VALUES (" + strings.Join(holders, ", ") + ")"
	res, err := c.Exec(context.Background(), q, args...)
	if err != nil {
		return 0, fmt.Errorf("store: insert %s: %w", t.Name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: last insert id %s: %w", t.Name, err)
	}
	return id, nil
}

// update 按主键整行更新；主键不存在时补写一条（与旧单文件后端语义一致）。
//
// MySQL 在「值没变」时也返回 0 行受影响，所以不能只看 RowsAffected 就认定记录不存在。
func update[T Entity](c *Client, t *Table[T], id int64, row Row) error {
	cols := sortedCols(row, true)
	sets := make([]string, 0, len(cols))
	args := make([]any, 0, len(cols)+1)
	for _, name := range cols {
		sets = append(sets, "`"+name+"` = ?")
		args = append(args, row[name])
	}
	args = append(args, id)
	res, err := c.Exec(context.Background(),
		"UPDATE "+quoteIdent(t.Name)+" SET "+strings.Join(sets, ", ")+" WHERE `id` = ?", args...)
	if err != nil {
		return fmt.Errorf("store: update %s(id=%d): %w", t.Name, id, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var one int
	err = c.QueryWith(context.Background(),
		"SELECT 1 FROM "+quoteIdent(t.Name)+" WHERE `id` = ?", []any{id},
		func(rows *sql.Rows) error {
			if !rows.Next() {
				return sql.ErrNoRows
			}
			return rows.Scan(&one)
		})
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("store: check %s(id=%d): %w", t.Name, id, err)
	}
	row["id"] = id
	_, err = insert(c, t, row)
	return err
}

// sortedCols 是两处共用的列排序逻辑：列名排序后拼 SQL，
// 保证同样的输入产生同样的语句（便于日志排查与预编译缓存）。
func sortedCols(row Row, skipID bool) []string {
	cols := make([]string, 0, len(row))
	for name := range row {
		if skipID && name == "id" {
			continue
		}
		cols = append(cols, name)
	}
	sort.Strings(cols)
	return cols
}

// ---- 结构体 ↔ 行 ----

func newTable[T Entity](name string) *Table[T] {
	var zero T
	rt := reflect.TypeOf(zero).Elem()
	t := &Table[T]{Name: name, timeCols: map[string]bool{}}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		col := columnOf(f)
		if col == "" {
			continue
		}
		t.Columns = append(t.Columns, col)
		if f.Tag.Get("col") == "time" {
			t.timeCols[col] = true
		}
	}
	return t
}

// columnOf 取列名：优先 db tag（引擎约定），否则用 json tag（与接口字段一致）。
func columnOf(f reflect.StructField) string {
	if tag := f.Tag.Get("db"); tag != "" && tag != "-" {
		return strings.Split(tag, ",")[0]
	}
	name := strings.Split(f.Tag.Get("json"), ",")[0]
	if name == "" || name == "-" {
		return ""
	}
	return name
}

// rowOf 把实体转成一行。时间列转 DATETIME，切片转 JSON 文本。
func rowOf[T Entity](t *Table[T], v T) Row {
	rv := reflect.ValueOf(v).Elem()
	rt := rv.Type()
	row := make(Row, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		col := columnOf(rt.Field(i))
		if col == "" {
			continue
		}
		row[col] = cellOf(t, col, rv.Field(i))
	}
	return row
}

func cellOf[T Entity](t *Table[T], col string, fv reflect.Value) any {
	switch fv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n := fv.Int()
		if t.timeCols[col] {
			if n <= 0 {
				return nil // 0 表示「没有这个时间」，库里存 NULL
			}
			return time.Unix(n, 0)
		}
		return n
	case reflect.Bool:
		if fv.Bool() {
			return 1
		}
		return 0
	case reflect.String:
		return fv.String()
	case reflect.Slice:
		if fv.IsNil() {
			return "[]"
		}
		b, err := json.Marshal(fv.Interface())
		if err != nil {
			return "[]"
		}
		return string(b)
	default:
		return fmt.Sprint(fv.Interface())
	}
}

// scan 把一行填进实体。取值走容错转换：驱动返回的类型并不统一
// （[]byte / int64 / time.Time / float64），在这里一次抹平。
func scan[T Entity](t *Table[T], row Row) T {
	var zero T
	ptr := reflect.New(reflect.TypeOf(zero).Elem())
	st := ptr.Elem()
	rt := st.Type()
	for i := 0; i < rt.NumField(); i++ {
		col := columnOf(rt.Field(i))
		if col == "" {
			continue
		}
		raw, ok := row[col]
		if !ok || raw == nil {
			continue
		}
		setCell(t, col, st.Field(i), raw)
	}
	return ptr.Interface().(T)
}

func setCell[T Entity](t *Table[T], col string, fv reflect.Value, raw any) {
	switch fv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if t.timeCols[col] {
			fv.SetInt(asUnix(raw))
			return
		}
		fv.SetInt(asInt64(raw))
	case reflect.Bool:
		fv.SetBool(asBool(raw))
	case reflect.String:
		fv.SetString(asString(raw))
	case reflect.Slice:
		fv.Set(reflect.ValueOf(asInt64Slice(raw)))
	}
}

// ---- 容错取值 ----

func asInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case int32:
		return int64(x)
	// 主键列是 BIGINT UNSIGNED，驱动在二进制协议下给的是 uint64（不是 int64）。
	case uint64:
		return int64(x)
	case uint32:
		return int64(x)
	case uint:
		return int64(x)
	case float64:
		return int64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	case []byte:
		n, _ := strconv.ParseInt(strings.TrimSpace(string(x)), 10, 64)
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	default:
		return 0
	}
}

// asUnix 把 DATETIME 列取成 unix 秒。parse_time=true 时驱动给 time.Time，
// 关掉 parse_time 时给 []byte/string，两种都得认。
func asUnix(v any) int64 {
	switch x := v.(type) {
	case time.Time:
		if x.IsZero() {
			return 0
		}
		return x.Unix()
	case []byte:
		return parseTimeString(string(x))
	case string:
		return parseTimeString(x)
	default:
		return asInt64(v)
	}
}

func parseTimeString(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Unix()
		}
	}
	return 0
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func asBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []byte:
		s := strings.TrimSpace(string(x))
		return s == "1" || strings.EqualFold(s, "true")
	case string:
		s := strings.TrimSpace(x)
		return s == "1" || strings.EqualFold(s, "true")
	default:
		return false
	}
}

func asInt64Slice(v any) []int64 {
	switch x := v.(type) {
	case []int64:
		return x
	case []any:
		out := make([]int64, 0, len(x))
		for _, it := range x {
			out = append(out, asInt64(it))
		}
		return out
	}
	s := strings.TrimSpace(asString(v))
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "[") {
		var list []int64
		if err := json.Unmarshal([]byte(s), &list); err == nil {
			return list
		}
	}
	out := []int64{}
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '，' || r == ' ' }) {
		if n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}
