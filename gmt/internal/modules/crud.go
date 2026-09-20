package modules

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gmt/internal/store"
)

// crudConf 描述一个「本地实体 + 标准增删改查」模块的差异部分。
//
// 只有真的不一样的地方才需要写：行怎么渲染、表单怎么落库、怎么筛选、怎么排序、
// 保存/删除时要不要顺带通知游戏侧。其余（分页、鉴权、日志、接口路由）全通用。
type crudConf[T store.Entity] struct {
	// toRow 把实体转成一行。带 *Ctx 是为了让展示层能顺带查关联数据
	// （比如把 role_id 翻成角色名），不必在实体里冗余字段。
	toRow     func(*Ctx, T) Row
	fromForm  func(c *Ctx, v Values, cur T) (T, error)
	match     func(T, Query) bool
	less      func(a, b T) bool
	afterSave func(c *Ctx, saved T) error
	beforeDel func(c *Ctx, cur T) error
}

// crud 组装一个标准的本地 CRUD 模块。
func crud[T store.Entity](
	key, name, icon, group string,
	table *store.Table[T],
	columns []Column,
	form []Field,
	search []Field,
	cf crudConf[T],
) *Module {
	if cf.toRow == nil || cf.fromForm == nil {
		panic("modules: " + key + " 必须提供 toRow 与 fromForm")
	}
	m := &Module{
		Key:     key,
		Name:    name,
		Icon:    icon,
		Group:   group,
		Columns: columns,
		Form:    form,
		Search:  search,
	}

	m.List = func(c *Ctx, q Query) ([]Row, int, error) {
		all, err := store.All[T](c.Store, table)
		if err != nil {
			return nil, 0, err
		}
		if cf.match != nil {
			kept := make([]T, 0, len(all))
			for _, it := range all {
				if cf.match(it, q) {
					kept = append(kept, it)
				}
			}
			all = kept
		}
		if cf.less != nil {
			sort.SliceStable(all, func(i, j int) bool { return cf.less(all[i], all[j]) })
		}

		size := q.Size
		if size <= 0 {
			size = 20
		}
		if size > 500 {
			size = 500
		}
		total := len(all)
		pages := (total + size - 1) / size
		page := q.Page
		if page < 1 {
			page = 1
		}
		if pages > 0 && page > pages {
			page = pages
		}
		start := (page - 1) * size
		end := start + size
		if start > total {
			start = total
		}
		if end > total {
			end = total
		}
		rows := make([]Row, 0, end-start)
		for _, it := range all[start:end] {
			rows = append(rows, cf.toRow(c, it))
		}
		return rows, total, nil
	}

	m.Save = func(c *Ctx, v Values) error {
		id := v.Int64("id")
		var cur T
		if id != 0 {
			got, ok, err := store.Get[T](c.Store, table, id)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("记录 %d 不存在", id)
			}
			cur = got
		} else {
			// 新增：得给一个可写的零值。T 是指针类型，直接 var 出来是 nil，
			// 交给 fromForm 会立刻空指针——这里按 T 指向的结构体 new 一个。
			cur = reflect.New(reflect.TypeOf(cur).Elem()).Interface().(T)
		}
		next, err := cf.fromForm(c, v, cur)
		if err != nil {
			return err
		}
		next.SetID(id) // 0 表示新增，非 0 表示更新
		saved, err := store.Put[T](c.Store, table, next)
		if err != nil {
			return err
		}
		// 先落库再同步游戏侧：宁可「后台有记录、游戏没生效」（可重试），
		// 也不要「游戏生效了、后台没记录」（无从追溯）。
		if cf.afterSave != nil {
			if err := cf.afterSave(c, saved); err != nil {
				return fmt.Errorf("本地已保存，但同步游戏侧失败: %w", err)
			}
		}
		c.Audit("save", fmt.Sprintf("id=%d", saved.GetID()))
		return nil
	}

	m.Delete = func(c *Ctx, id int64) error {
		cur, ok, err := store.Get[T](c.Store, table, id)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("记录 %d 不存在", id)
		}
		if cf.beforeDel != nil {
			if err := cf.beforeDel(c, cur); err != nil {
				return err
			}
		}
		if ok, err := store.Del[T](c.Store, table, id); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("记录 %d 不存在", id)
		}
		c.Audit("delete", fmt.Sprintf("id=%d", id))
		return nil
	}

	return m
}

// matchAny 是所有匹配条件都为真才通过的辅助（Query 里的条件一般是「与」关系）。
func matchAny(conds ...bool) bool {
	for _, c := range conds {
		if !c {
			return false
		}
	}
	return true
}

// like 做不区分大小写的包含匹配；查询词为空视为「不过滤」。
func like(field, keyword string) bool {
	if keyword == "" {
		return true
	}
	return containsFold(field, keyword)
}

func containsFold(s, sub string) bool {
	if sub == "" {
		return true
	}
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
