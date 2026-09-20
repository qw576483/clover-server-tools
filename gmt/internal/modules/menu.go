package modules

import "github.com/qw576483/clover-server-tools/gmt/internal/store"

// EnsureMenus 把「注册了哪些模块 / 页面」同步成菜单数据。
//
// 菜单是模块声明的投影，不是手填的静态表：
//   - 新注册的模块，启动后自动出现在菜单里；
//   - 已存在的菜单按 URL 匹配，只更新名称/图标/顺序/上级，**保留它的 ID**——
//     否则每次启动都重建一遍菜单，角色里配置的权限就全丢了；
//   - 代码里已经不存在的 URL 会被删掉，菜单因此不会指向已经下线的功能。
//     手工在页面里加的菜单同样会被清掉：菜单由代码定义。（要加链接先加模块/页面。）
func EnsureMenus(s *store.Client) error {
	menus, err := store.All[*store.Menu](s, store.Menus)
	if err != nil {
		return err
	}
	byURL := map[string]*store.Menu{}
	for _, m := range menus {
		byURL[m.URL] = m
	}
	want := map[string]bool{}

	upsert := func(name, url, icon string, order int, parentID int64) error {
		want[url] = true
		if m, ok := byURL[url]; ok {
			if m.Name == name && m.Icon == icon && m.OrderNo == order && m.ParentID == parentID {
				return nil
			}
			m.Name, m.Icon, m.OrderNo, m.ParentID = name, icon, order, parentID
			_, err := store.Put[*store.Menu](s, store.Menus, m)
			return err
		}
		saved, err := store.Put[*store.Menu](s, store.Menus, &store.Menu{
			Name: name, URL: url, Icon: icon, OrderNo: order, ParentID: parentID,
		})
		if err != nil {
			return err
		}
		byURL[url] = saved
		return nil
	}

	// 一级：分组（URL 用 #分组名，不可点击，只做折叠容器）。
	parents := map[string]int64{}
	for _, g := range Groups {
		if err := upsert(g.Name, "#"+g.Key, g.Icon, g.OrderNo, 0); err != nil {
			return err
		}
		parents[g.Key] = byURL["#"+g.Key].ID
	}
	// 二级：自定义页面在前，通用列表模块在后。
	for _, p := range pages {
		if err := upsert(p.Name, p.Path, p.Icon, 10, parents[p.Group]); err != nil {
			return err
		}
	}
	for _, m := range All() {
		if err := upsert(m.Name, "/m/"+m.Key, m.Icon, 20, parents[m.Group]); err != nil {
			return err
		}
	}

	// 清掉代码里已经没有的菜单（模块下线后菜单不该继续挂着）。
	for _, m := range menus {
		if want[m.URL] {
			continue
		}
		if _, err := store.Del[*store.Menu](s, store.Menus, m.ID); err != nil {
			return err
		}
	}
	return nil
}
