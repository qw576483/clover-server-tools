package modules

import (
	"fmt"

	"gmt/internal/auth"
	"gmt/internal/gameclient"
	"gmt/internal/store"
)

// Group 是左侧菜单的一级分组。
type Group struct {
	Key     string
	Name    string
	Icon    string
	OrderNo int
}

// Groups 是菜单分组，顺序即左侧菜单的展示顺序。
var Groups = []Group{
	{Key: "ops", Name: "运维管理", Icon: "server", OrderNo: 10},
	{Key: "player", Name: "玩家管理", Icon: "users", OrderNo: 20},
	{Key: "operate", Name: "运营管理", Icon: "gift", OrderNo: 30},
	{Key: "system", Name: "系统设置", Icon: "settings", OrderNo: 40},
}

// 常用下拉选项与状态色。集中放在这里，免得同一个「状态」在十个模块里写十遍。
var (
	yesNo          = []Option{{Value: "1", Label: "是"}, {Value: "0", Label: "否"}}
	onOff          = []Option{{Value: "1", Label: "正常"}, {Value: "0", Label: "停用"}}
	serverStatus   = []Option{{Value: "1", Label: "正常"}, {Value: "2", Label: "维护"}, {Value: "3", Label: "预告"}, {Value: "0", Label: "隐藏"}}
	onOffBadge     = map[string]string{"1": "success", "0": "danger"}
	onOffLabels    = map[string]string{"1": "正常", "0": "停用"}
	boolBadge      = map[string]string{"true": "success", "false": "secondary"}
	boolLabels     = map[string]string{"true": "是", "false": "否"}
	serverBadge    = map[string]string{"1": "success", "2": "warning", "3": "info", "0": "secondary"}
	serverLabels   = map[string]string{"1": "正常", "2": "维护", "3": "预告", "0": "隐藏"}
	banKind        = []Option{{Value: "account", Label: "封账号"}, {Value: "role", Label: "封角色"}, {Value: "chat", Label: "禁言"}}
	banBadge       = map[string]string{"account": "danger", "role": "warning", "chat": "info"}
	banLabels      = map[string]string{"account": "封账号", "role": "封角色", "chat": "禁言"}
	giftKind       = []Option{{Value: "normal", Label: "通用码"}, {Value: "once", Label: "一次性"}}
	giftBadge      = map[string]string{"normal": "primary", "once": "warning"}
	giftLabels     = map[string]string{"normal": "通用码", "once": "一次性"}
	bulletinType   = []Option{{Value: "login", Label: "登录公告"}, {Value: "roll", Label: "滚动公告"}}
	bulletinBadge  = map[string]string{"login": "primary", "roll": "info"}
	bulletinLabels = map[string]string{"login": "登录公告", "roll": "滚动公告"}
	orderStatus    = []Option{{Value: "-1", Label: "全部"}, {Value: "0", Label: "未支付"}, {Value: "1", Label: "已支付"}, {Value: "2", Label: "已发货"}, {Value: "3", Label: "已退款"}}
	orderBadge     = map[string]string{"0": "secondary", "1": "success", "2": "info", "3": "danger"}
	orderLabels    = map[string]string{"0": "未支付", "1": "已支付", "2": "已发货", "3": "已退款"}
	activeBadge    = map[string]string{"true": "danger", "false": "success"}
	activeLabels   = map[string]string{"true": "生效中", "false": "已解除"}
	enableBadge    = map[string]string{"true": "success", "false": "secondary"}
	enableLabels   = map[string]string{"true": "启用", "false": "停用"}
)

func init() { RegisterBuiltin() }

// RegisterBuiltin 注册全部内置模块与自定义页面。
//
// 这里就是整个后台的「功能清单」：想加功能，照着下面任意一段再写一段声明即可，
// 不用碰路由、不用写页面、不用写 JS。
func RegisterBuiltin() {
	RegisterPage(Page{Key: "dashboard", Name: "概览", Icon: "home", Group: "", Path: "/"})
	RegisterPage(Page{Key: "player", Name: "玩家查询", Icon: "user-check", Group: "player", Path: "/player"})
	RegisterPage(Page{Key: "mail", Name: "邮件管理", Icon: "mail", Group: "operate", Path: "/mail"})

	registerMachine()
	registerServer()
	registerBan()
	registerBulletin()
	registerGift()
	registerOrder()
	registerAccount()
	registerRole()
	registerMenu()
	registerAudit()
}

// ---- 机器管理 ----

func registerMachine() {
	Register(crud[*store.Machine]("machine", "机器管理", "server", "ops", store.Machines,
		[]Column{
			{Key: "name", Title: "名称"},
			{Key: "ip", Title: "外网 IP"},
			{Key: "inner_ip", Title: "内网 IP"},
			{Key: "ssh_port", Title: "SSH 端口", Width: "90px"},
			{Key: "ssh_user", Title: "SSH 账号"},
			{Key: "is_game", Title: "游戏机", Kind: "badge", Badge: boolBadge, Labels: boolLabels},
			{Key: "status", Title: "状态", Kind: "badge", Badge: onOffBadge, Labels: onOffLabels},
			{Key: "note", Title: "备注"},
		},
		[]Field{
			{Key: "name", Label: "名称", Type: "text", Required: true},
			{Key: "ip", Label: "外网 IP", Type: "text", Required: true},
			{Key: "inner_ip", Label: "内网 IP", Type: "text"},
			{Key: "ssh_port", Label: "SSH 端口", Type: "number", Default: "22"},
			{Key: "ssh_user", Label: "SSH 账号", Type: "text", Default: "root"},
			{Key: "ssh_pass", Label: "SSH 密码", Type: "password", Placeholder: "留空表示不修改"},
			{Key: "is_game", Label: "是否是游戏机", Type: "select", Options: yesNo},
			{Key: "status", Label: "状态", Type: "select", Options: onOff},
			{Key: "note", Label: "备注", Type: "textarea"},
		},
		[]Field{
			{Key: "name", Label: "名称", Type: "text"},
			{Key: "ip", Label: "IP", Type: "text"},
		},
		crudConf[*store.Machine]{
			toRow: func(c *Ctx, m *store.Machine) Row {
				return Row{"id": m.ID, "name": m.Name, "ip": m.IP, "inner_ip": m.InnerIP,
					"ssh_port": m.SSHPort, "ssh_user": m.SSHUser, "is_game": m.IsGame,
					"status": m.Status, "note": m.Note}
			},
			fromForm: func(c *Ctx, v Values, cur *store.Machine) (*store.Machine, error) {
				cur.Name = v.Str("name")
				cur.IP = v.Str("ip")
				cur.InnerIP = v.Str("inner_ip")
				cur.SSHPort = v.Int("ssh_port")
				cur.SSHUser = v.Str("ssh_user")
				cur.IsGame = v.Bool("is_game")
				cur.Status = v.Int("status")
				cur.Note = v.Str("note")
				if cur.SSHPort == 0 {
					cur.SSHPort = 22
				}
				// 密码留空 = 不改动：否则每次编辑别的字段都会把口令清掉。
				if pw := v.Str("ssh_pass"); pw != "" {
					cur.SSHPass = pw
				}
				if cur.Name == "" || cur.IP == "" {
					return cur, fmt.Errorf("名称与 IP 不能为空")
				}
				return cur, nil
			},
			match: func(m *store.Machine, q Query) bool {
				return like(m.Name, q.S("name")) && like(m.IP, q.S("ip"))
			},
			less: func(a, b *store.Machine) bool { return a.ID < b.ID },
		}))
}

// ---- 区服管理 ----

func registerServer() {
	Register(crud[*store.Server]("server", "区服管理", "layers", "ops", store.Servers,
		[]Column{
			{Key: "server_id", Title: "区服 ID", Width: "100px"},
			{Key: "zone", Title: "大区"},
			{Key: "name", Title: "服名"},
			{Key: "alias", Title: "简称"},
			{Key: "status", Title: "状态", Kind: "badge", Badge: serverBadge, Labels: serverLabels},
			{Key: "parent_id", Title: "父服", Width: "90px"},
			{Key: "order_no", Title: "排序", Width: "80px"},
			{Key: "open_time", Title: "开服时间", Kind: "time"},
			{Key: "will_open_time", Title: "预开服时间", Kind: "time"},
		},
		[]Field{
			{Key: "server_id", Label: "区服 ID", Type: "number", Required: true},
			{Key: "zone", Label: "大区", Type: "text"},
			{Key: "name", Label: "服名", Type: "text", Required: true},
			{Key: "alias", Label: "简称", Type: "text"},
			{Key: "status", Label: "状态", Type: "select", Options: serverStatus},
			{Key: "parent_id", Label: "父服 ID", Type: "number", Placeholder: "0 表示本身就是父服"},
			{Key: "order_no", Label: "排序", Type: "number"},
			{Key: "open_time", Label: "开服时间", Type: "datetime"},
			{Key: "will_open_time", Label: "预开服时间", Type: "datetime"},
		},
		[]Field{
			{Key: "server_id", Label: "区服 ID", Type: "number"},
			{Key: "name", Label: "服名", Type: "text"},
			{Key: "zone", Label: "大区", Type: "text"},
			{Key: "status", Label: "状态", Type: "select", Options: append([]Option{{Value: "-1", Label: "全部"}}, serverStatus...)},
		},
		crudConf[*store.Server]{
			toRow: func(c *Ctx, s *store.Server) Row {
				return Row{"id": s.ID, "server_id": s.ServerID, "zone": s.Zone, "name": s.Name,
					"alias": s.Alias, "status": s.Status, "parent_id": s.ParentID,
					"order_no": s.OrderNo, "open_time": s.OpenTime, "will_open_time": s.WillOpenTime}
			},
			fromForm: func(c *Ctx, v Values, cur *store.Server) (*store.Server, error) {
				cur.ServerID = v.Int64("server_id")
				cur.Zone = v.Str("zone")
				cur.Name = v.Str("name")
				cur.Alias = v.Str("alias")
				cur.Status = v.Int("status")
				cur.ParentID = v.Int64("parent_id")
				cur.OrderNo = v.Int("order_no")
				cur.OpenTime = v.Time("open_time")
				cur.WillOpenTime = v.Time("will_open_time")
				if cur.ServerID == 0 || cur.Name == "" {
					return cur, fmt.Errorf("区服 ID 与服名不能为空")
				}
				return cur, nil
			},
			match: func(s *store.Server, q Query) bool {
				status := q.S("status")
				return matchAny(
					like(fmt.Sprint(s.ServerID), q.S("server_id")),
					like(s.Name, q.S("name")),
					like(s.Zone, q.S("zone")),
					status == "" || status == "-1" || fmt.Sprint(s.Status) == status,
				)
			},
			// 父服排前面、同级按排序号：列表天然是一棵树，比另做树形控件省事。
			less: func(a, b *store.Server) bool {
				if a.ParentID != b.ParentID {
					return a.ParentID < b.ParentID
				}
				if a.OrderNo != b.OrderNo {
					return a.OrderNo < b.OrderNo
				}
				return a.ServerID < b.ServerID
			},
		}))
}

// ---- 封禁管理 ----

func registerBan() {
	m := crud[*store.Ban]("ban", "封禁管理", "lock", "player", store.Bans,
		[]Column{
			{Key: "kind", Title: "类型", Kind: "badge", Badge: banBadge, Labels: banLabels},
			{Key: "target", Title: "封禁目标"},
			{Key: "player_id", Title: "角色 ID"},
			{Key: "server_id", Title: "区服", Width: "90px"},
			{Key: "until", Title: "解封时间", Kind: "time"},
			{Key: "reason", Title: "原因"},
			{Key: "active", Title: "状态", Kind: "badge", Badge: activeBadge, Labels: activeLabels},
			{Key: "operator", Title: "操作人", Width: "100px"},
			{Key: "created_at", Title: "操作时间", Kind: "time"},
		},
		[]Field{
			{Key: "kind", Label: "封禁类型", Type: "select", Options: banKind, Required: true},
			{Key: "target", Label: "封禁目标", Type: "text", Required: true, Placeholder: "账号名 / 角色名 / 角色 ID"},
			{Key: "player_id", Label: "角色 ID", Type: "number"},
			{Key: "server_id", Label: "区服 ID", Type: "number"},
			{Key: "until", Label: "解封时间", Type: "datetime", Required: true},
			{Key: "reason", Label: "原因", Type: "text", Required: true},
		},
		[]Field{
			{Key: "target", Label: "封禁目标", Type: "text"},
			{Key: "kind", Label: "类型", Type: "select", Options: append([]Option{{Value: "", Label: "全部"}}, banKind...)},
		},
		crudConf[*store.Ban]{
			toRow: func(c *Ctx, b *store.Ban) Row {
				return Row{"id": b.ID, "kind": b.Kind, "target": b.Target, "player_id": b.PlayerID,
					"server_id": b.ServerID, "until": b.Until, "reason": b.Reason,
					"active": b.Active, "operator": b.Operator, "created_at": b.CreatedAt}
			},
			fromForm: func(c *Ctx, v Values, cur *store.Ban) (*store.Ban, error) {
				cur.Kind = v.Str("kind")
				cur.Target = v.Str("target")
				cur.PlayerID = v.Int64("player_id")
				cur.ServerID = v.Int64("server_id")
				cur.Until = v.Time("until")
				cur.Reason = v.Str("reason")
				cur.CreatedAt = store.Now()
				cur.Active = true
				if c.User != nil {
					cur.Operator = c.User.Name
				}
				if cur.Kind == "" || cur.Target == "" {
					return cur, fmt.Errorf("封禁类型与目标不能为空")
				}
				if cur.Until != 0 && cur.Until < store.Now() {
					return cur, fmt.Errorf("解封时间不能早于当前时间")
				}
				return cur, nil
			},
			match: func(b *store.Ban, q Query) bool {
				return like(b.Target, q.S("target")) && (q.S("kind") == "" || b.Kind == q.S("kind"))
			},
			less: func(a, b *store.Ban) bool { return a.CreatedAt > b.CreatedAt },
			// 先落库再下发：宁可「后台有记录、游戏没生效」（可重试、可发现），
			// 也不要「游戏封了、后台查不到」（无从追溯）。
			afterSave: func(c *Ctx, saved *store.Ban) error {
				return c.Game.ApplyBan(c.Gin.Request.Context(), gameclient.BanRequest{
					Kind: saved.Kind, Target: saved.Target, PlayerID: saved.PlayerID,
					ServerID: saved.ServerID, Until: saved.Until, Reason: saved.Reason,
				})
			},
			beforeDel: func(c *Ctx, cur *store.Ban) error {
				if !cur.Active {
					return nil
				}
				return c.Game.LiftBan(c.Gin.Request.Context(), gameclient.BanRequest{
					Kind: cur.Kind, Target: cur.Target, PlayerID: cur.PlayerID, ServerID: cur.ServerID,
				})
			},
		})
	m.Actions = []Action{{Key: "lift", Name: "解封", Style: "warning", Confirm: "确认解除该封禁？解除后会立即通知游戏侧"}}
	m.Do = func(c *Ctx, action string, id int64, v Values) (any, error) {
		if action != "lift" {
			return nil, fmt.Errorf("未知操作 %s", action)
		}
		b, ok, err := store.Get[*store.Ban](c.Store, store.Bans, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("记录 %d 不存在", id)
		}
		if !b.Active {
			return nil, fmt.Errorf("该封禁已解除")
		}
		if err := c.Game.LiftBan(c.Gin.Request.Context(), gameclient.BanRequest{
			Kind: b.Kind, Target: b.Target, PlayerID: b.PlayerID, ServerID: b.ServerID,
		}); err != nil {
			return nil, err
		}
		b.Active = false
		if _, err := store.Put[*store.Ban](c.Store, store.Bans, b); err != nil {
			return nil, err
		}
		c.Audit("lift", fmt.Sprintf("id=%d target=%s", id, b.Target))
		return nil, nil
	}
	Register(m)
}

// ---- 公告管理（数据在游戏侧） ----
//
// 这个模块的数据不在 gmt 本地：公告属于游戏内容，由 coop 服持有。
// 模块声明的形状和本地模块完全一致，只是 List/Save/Delete 三个回调换成远程调用——
// 页面与前端完全感知不到差别。

func registerBulletin() {
	m := &Module{
		Key:   "bulletin",
		Name:  "公告管理",
		Icon:  "message-square",
		Group: "operate",
		Columns: []Column{
			{Key: "id", Title: "ID", Width: "70px"},
			{Key: "type", Title: "类型", Kind: "badge", Badge: bulletinBadge, Labels: bulletinLabels},
			{Key: "title", Title: "标题"},
			{Key: "content", Title: "内容"},
			{Key: "start_at", Title: "开始", Kind: "time"},
			{Key: "end_at", Title: "结束", Kind: "time"},
			{Key: "interval", Title: "间隔(秒)", Width: "100px"},
			{Key: "enabled", Title: "状态", Kind: "badge", Badge: enableBadge, Labels: enableLabels},
		},
		Form: []Field{
			{Key: "type", Label: "类型", Type: "select", Options: bulletinType, Required: true},
			{Key: "title", Label: "标题", Type: "text", Required: true},
			{Key: "content", Label: "内容", Type: "textarea", Required: true},
			{Key: "start_at", Label: "开始时间", Type: "datetime", Required: true},
			{Key: "end_at", Label: "结束时间", Type: "datetime", Required: true},
			{Key: "interval", Label: "滚动间隔(秒)", Type: "number", Placeholder: "滚动公告专用，建议 >5 秒"},
			{Key: "enabled", Label: "是否启用", Type: "select", Options: yesNo},
		},
		Search: []Field{
			{Key: "title", Label: "标题", Type: "text"},
			{Key: "type", Label: "类型", Type: "select", Options: append([]Option{{Value: "", Label: "全部"}}, bulletinType...)},
		},
	}
	m.List = func(c *Ctx, q Query) ([]Row, int, error) {
		list, err := c.Game.ListBulletins(c.Gin.Request.Context())
		if err != nil {
			return nil, 0, err
		}
		rows := make([]Row, 0, len(list))
		for _, b := range list {
			if !like(b.Title, q.S("title")) {
				continue
			}
			if t := q.S("type"); t != "" && b.Type != t {
				continue
			}
			rows = append(rows, Row{"id": b.ID, "type": b.Type, "title": b.Title, "content": b.Content,
				"start_at": b.StartAt, "end_at": b.EndAt, "interval": b.Interval, "enabled": b.Enabled})
		}
		return rows, len(rows), nil
	}
	m.Save = func(c *Ctx, v Values) error {
		b := gameclient.Bulletin{
			ID: v.Int64("id"), Type: v.Str("type"), Title: v.Str("title"), Content: v.Str("content"),
			StartAt: v.Time("start_at"), EndAt: v.Time("end_at"), Interval: v.Int("interval"), Enabled: v.Bool("enabled"),
		}
		if b.Title == "" || b.Content == "" {
			return fmt.Errorf("标题与内容不能为空")
		}
		if b.StartAt != 0 && b.EndAt != 0 && b.EndAt <= b.StartAt {
			return fmt.Errorf("结束时间必须晚于开始时间")
		}
		if err := c.Game.SaveBulletin(c.Gin.Request.Context(), b); err != nil {
			return err
		}
		c.Audit("save", fmt.Sprintf("title=%s", b.Title))
		return nil
	}
	m.Delete = func(c *Ctx, id int64) error {
		if err := c.Game.DeleteBulletin(c.Gin.Request.Context(), id); err != nil {
			return err
		}
		c.Audit("delete", fmt.Sprintf("id=%d", id))
		return nil
	}
	Register(m)
}

// ---- 礼包码 ----

func registerGift() {
	m := crud[*store.GiftBatch]("gift", "礼包码", "gift", "operate", store.Gifts,
		[]Column{
			{Key: "name", Title: "批次名"},
			{Key: "prefix", Title: "码前缀"},
			{Key: "count", Title: "数量", Width: "90px"},
			{Key: "kind", Title: "类型", Kind: "badge", Badge: giftBadge, Labels: giftLabels},
			{Key: "server_id", Title: "区服", Width: "90px"},
			{Key: "expire_at", Title: "有效期至", Kind: "time"},
			{Key: "note", Title: "备注"},
			{Key: "created_at", Title: "创建时间", Kind: "time"},
		},
		[]Field{
			{Key: "name", Label: "批次名", Type: "text", Required: true},
			{Key: "prefix", Label: "码前缀", Type: "text", Placeholder: "留空则随机"},
			{Key: "count", Label: "生成数量", Type: "number", Required: true},
			{Key: "kind", Label: "类型", Type: "select", Options: giftKind},
			{Key: "server_id", Label: "区服 ID", Type: "number", Placeholder: "0 表示全服"},
			{Key: "expire_at", Label: "有效期至", Type: "datetime"},
			{Key: "note", Label: "备注", Type: "textarea"},
		},
		[]Field{{Key: "name", Label: "批次名", Type: "text"}},
		crudConf[*store.GiftBatch]{
			toRow: func(c *Ctx, g *store.GiftBatch) Row {
				return Row{"id": g.ID, "name": g.Name, "prefix": g.Prefix, "count": g.Count,
					"kind": g.Kind, "server_id": g.ServerID, "expire_at": g.ExpireAt,
					"note": g.Note, "created_at": g.CreatedAt}
			},
			fromForm: func(c *Ctx, v Values, cur *store.GiftBatch) (*store.GiftBatch, error) {
				cur.Name = v.Str("name")
				cur.Prefix = v.Str("prefix")
				cur.Count = v.Int("count")
				cur.Kind = v.Str("kind")
				cur.ServerID = v.Int64("server_id")
				cur.ExpireAt = v.Time("expire_at")
				cur.Note = v.Str("note")
				cur.CreatedAt = store.Now()
				if cur.Name == "" {
					return cur, fmt.Errorf("批次名不能为空")
				}
				if cur.Count <= 0 || cur.Count > 10000 {
					return cur, fmt.Errorf("生成数量需在 1~10000 之间")
				}
				if cur.Kind == "" {
					cur.Kind = "normal"
				}
				return cur, nil
			},
			match: func(g *store.GiftBatch, q Query) bool { return like(g.Name, q.S("name")) },
			less:  func(a, b *store.GiftBatch) bool { return a.CreatedAt > b.CreatedAt },
		})
	m.Actions = []Action{{Key: "gen", Name: "生成兑换码", Style: "primary", Confirm: "确认按该批次生成兑换码？结果会弹窗展示，请自行保存"}}
	m.Do = func(c *Ctx, action string, id int64, v Values) (any, error) {
		if action != "gen" {
			return nil, fmt.Errorf("未知操作 %s", action)
		}
		g, ok, err := store.Get[*store.GiftBatch](c.Store, store.Gifts, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("批次 %d 不存在", id)
		}
		codes, err := c.Game.GenGift(c.Gin.Request.Context(), gameclient.GiftRequest{
			Batch: g.Name, Prefix: g.Prefix, Count: g.Count, Kind: g.Kind,
			ServerID: g.ServerID, ExpireAt: g.ExpireAt,
		})
		if err != nil {
			return nil, err
		}
		c.Audit("gen", fmt.Sprintf("batch=%s count=%d", g.Name, len(codes)))
		return map[string]any{"codes": codes}, nil
	}
	Register(m)
}

// ---- 订单查询（只读，数据在游戏侧） ----

func registerOrder() {
	m := &Module{
		Key:      "order",
		Name:     "订单查询",
		Icon:     "credit-card",
		Group:    "operate",
		ReadOnly: true,
		Columns: []Column{
			{Key: "order_id", Title: "订单号"},
			{Key: "account", Title: "账号"},
			{Key: "player_id", Title: "角色 ID"},
			{Key: "server_id", Title: "区服", Width: "90px"},
			{Key: "product", Title: "商品"},
			{Key: "amount", Title: "金额", Kind: "money"},
			{Key: "status", Title: "状态", Kind: "badge", Badge: orderBadge, Labels: orderLabels},
			{Key: "channel", Title: "渠道", Width: "100px"},
			{Key: "time", Title: "时间", Kind: "time"},
		},
		Search: []Field{
			{Key: "order_id", Label: "订单号", Type: "text"},
			{Key: "account", Label: "账号", Type: "text"},
			{Key: "player_id", Label: "角色 ID", Type: "number"},
			{Key: "server_id", Label: "区服 ID", Type: "number"},
			{Key: "status", Label: "状态", Type: "select", Options: orderStatus},
		},
	}
	m.List = func(c *Ctx, q Query) ([]Row, int, error) {
		list, err := c.Game.QueryOrders(c.Gin.Request.Context(), gameclient.OrderQuery{
			ServerID: q.I64("server_id"),
			Account:  q.S("account"),
			PlayerID: q.I64("player_id"),
			OrderID:  q.S("order_id"),
			Status:   q.I("status"),
			Page:     q.Page,
			Size:     q.Size,
		})
		if err != nil {
			return nil, 0, err
		}
		rows := make([]Row, 0, len(list))
		for _, o := range list {
			rows = append(rows, Row{"id": o.OrderID, "order_id": o.OrderID, "account": o.Account,
				"player_id": o.PlayerID, "server_id": o.ServerID, "product": o.Product,
				"amount": o.Amount, "status": o.Status, "channel": o.Channel, "time": o.Time})
		}
		return rows, len(rows), nil
	}
	Register(m)
}

// ---- 后台账号 ----

func registerAccount() {
	Register(crud[*store.Account]("account", "后台账号", "user", "system", store.Accounts,
		[]Column{
			{Key: "id", Title: "ID", Width: "70px"},
			{Key: "name", Title: "登录账号"},
			{Key: "nick", Title: "昵称"},
			{Key: "role_name", Title: "角色"},
			{Key: "status", Title: "状态", Kind: "badge", Badge: onOffBadge, Labels: onOffLabels},
			{Key: "last_login_at", Title: "最后登录", Kind: "time"},
			{Key: "last_login_ip", Title: "登录 IP"},
		},
		[]Field{
			{Key: "name", Label: "登录账号", Type: "text", Required: true},
			{Key: "password", Label: "密码", Type: "password", Placeholder: "留空表示不修改"},
			{Key: "nick", Label: "昵称", Type: "text"},
			{Key: "role_id", Label: "角色", Type: "select", Required: true, OptionsFrom: roleOptions},
			{Key: "status", Label: "状态", Type: "select", Options: onOff},
		},
		[]Field{{Key: "name", Label: "登录账号", Type: "text"}},
		crudConf[*store.Account]{
			toRow: func(c *Ctx, a *store.Account) Row {
				roleName := "-"
				if r, ok, err := store.Get[*store.Role](c.Store, store.Roles, a.RoleID); err == nil && ok {
					roleName = r.Name
				}
				return Row{"id": a.ID, "name": a.Name, "nick": a.Nick, "role_name": roleName,
					"status": a.Status, "last_login_at": a.LastLoginAt, "last_login_ip": a.LastLoginIP}
			},
			fromForm: func(c *Ctx, v Values, cur *store.Account) (*store.Account, error) {
				name := v.Str("name")
				cur.Name = name
				cur.Nick = v.Str("nick")
				cur.RoleID = v.Int64("role_id")
				cur.Status = v.Int("status")
				if cur.CreatedAt == 0 {
					cur.CreatedAt = store.Now()
				}
				if pw := v.Str("password"); pw != "" {
					cur.PassHash = auth.Hash(c.Secret, name, pw)
				} else if cur.PassHash == "" {
					return cur, fmt.Errorf("新账号必须设置密码")
				}
				if cur.Name == "" {
					return cur, fmt.Errorf("登录账号不能为空")
				}
				if cur.RoleID == 0 {
					return cur, fmt.Errorf("必须选择角色")
				}
				return cur, nil
			},
			match: func(a *store.Account, q Query) bool { return like(a.Name, q.S("name")) },
			less:  func(a, b *store.Account) bool { return a.ID < b.ID },
		}))
}

// ---- 角色权限 ----

func registerRole() {
	Register(crud[*store.Role]("role", "角色权限", "key", "system", store.Roles,
		[]Column{
			{Key: "id", Title: "ID", Width: "70px"},
			{Key: "name", Title: "角色名"},
			{Key: "remark", Title: "说明"},
			{Key: "menu_names", Title: "可见菜单"},
		},
		[]Field{
			{Key: "name", Label: "角色名", Type: "text", Required: true},
			{Key: "remark", Label: "说明", Type: "text"},
			{Key: "menu_ids", Label: "可见菜单", Type: "select", Multiple: true, OptionsFrom: menuOptions},
		},
		[]Field{{Key: "name", Label: "角色名", Type: "text"}},
		crudConf[*store.Role]{
			toRow: func(c *Ctx, r *store.Role) Row {
				return Row{"id": r.ID, "name": r.Name, "remark": r.Remark, "menu_names": menuNamesOf(c.Store, r.MenuIDs)}
			},
			fromForm: func(c *Ctx, v Values, cur *store.Role) (*store.Role, error) {
				cur.Name = v.Str("name")
				cur.Remark = v.Str("remark")
				cur.MenuIDs = v.Int64Slice("menu_ids")
				if cur.Name == "" {
					return cur, fmt.Errorf("角色名不能为空")
				}
				// 超管角色不允许被清空权限：一旦清空，就再也没人能改回来了。
				if cur.ID == 1 && len(cur.MenuIDs) == 0 {
					return cur, fmt.Errorf("超级管理员必须至少保留一个菜单")
				}
				return cur, nil
			},
			beforeDel: func(c *Ctx, cur *store.Role) error {
				// 超管角色被删掉，就再也没人能改回来了（IsSuper 认的是这个 ID）。
				if cur.ID == auth.SuperRoleID {
					return fmt.Errorf("超级管理员角色不可删除")
				}
				users, err := store.All[*store.Account](c.Store, store.Accounts)
				if err != nil {
					return err
				}
				for _, u := range users {
					if u.RoleID == cur.ID {
						return fmt.Errorf("还有账号 %s 在使用该角色，请先改掉他们的角色", u.Name)
					}
				}
				return nil
			},
			match: func(r *store.Role, q Query) bool { return like(r.Name, q.S("name")) },
			less:  func(a, b *store.Role) bool { return a.ID < b.ID },
		}))
}

// ---- 菜单管理 ----

func registerMenu() {
	Register(crud[*store.Menu]("menu", "菜单管理", "menu", "system", store.Menus,
		[]Column{
			{Key: "id", Title: "ID", Width: "70px"},
			{Key: "parent_name", Title: "上级"},
			{Key: "name", Title: "菜单名"},
			{Key: "url", Title: "地址"},
			{Key: "icon", Title: "图标", Width: "100px"},
			{Key: "order_no", Title: "排序", Width: "80px"},
		},
		[]Field{
			{Key: "name", Label: "菜单名", Type: "text", Required: true},
			{Key: "url", Label: "地址", Type: "text", Placeholder: "如 /m/server；分组菜单填 #分组名"},
			{Key: "icon", Label: "图标", Type: "text", Placeholder: "feather 图标名，如 server"},
			{Key: "parent_id", Label: "上级菜单", Type: "select", OptionsFrom: parentMenuOptions},
			{Key: "order_no", Label: "排序", Type: "number"},
		},
		[]Field{{Key: "name", Label: "菜单名", Type: "text"}},
		crudConf[*store.Menu]{
			toRow: func(c *Ctx, m *store.Menu) Row {
				parent := "-"
				if m.ParentID != 0 {
					if p, ok, err := store.Get[*store.Menu](c.Store, store.Menus, m.ParentID); err == nil && ok {
						parent = p.Name
					}
				}
				return Row{"id": m.ID, "parent_name": parent, "name": m.Name, "url": m.URL,
					"icon": m.Icon, "order_no": m.OrderNo}
			},
			fromForm: func(c *Ctx, v Values, cur *store.Menu) (*store.Menu, error) {
				cur.Name = v.Str("name")
				cur.URL = v.Str("url")
				cur.Icon = v.Str("icon")
				cur.ParentID = v.Int64("parent_id")
				cur.OrderNo = v.Int("order_no")
				if cur.Name == "" {
					return cur, fmt.Errorf("菜单名不能为空")
				}
				if cur.ID != 0 && cur.ParentID == cur.ID {
					return cur, fmt.Errorf("上级菜单不能是自己")
				}
				return cur, nil
			},
			match: func(m *store.Menu, q Query) bool { return like(m.Name, q.S("name")) },
			less: func(a, b *store.Menu) bool {
				if a.ParentID != b.ParentID {
					return a.ParentID < b.ParentID
				}
				return a.OrderNo < b.OrderNo
			},
		}))
}

// ---- 操作日志（只读） ----

func registerAudit() {
	m := crud[*store.Audit]("audit", "操作日志", "activity", "system", store.Audits,
		[]Column{
			{Key: "time", Title: "时间", Kind: "time", Width: "170px"},
			{Key: "account", Title: "操作人", Width: "120px"},
			{Key: "ip", Title: "IP", Width: "130px"},
			{Key: "type", Title: "模块", Width: "110px"},
			{Key: "sub_type", Title: "动作", Width: "90px"},
			{Key: "info", Title: "详情"},
		},
		nil,
		[]Field{
			{Key: "account", Label: "操作人", Type: "text"},
			{Key: "type", Label: "模块", Type: "text"},
		},
		crudConf[*store.Audit]{
			toRow: func(c *Ctx, a *store.Audit) Row {
				return Row{"id": a.ID, "time": a.Time, "account": a.Account, "ip": a.IP,
					"type": a.Type, "sub_type": a.SubType, "info": a.Info}
			},
			fromForm: func(c *Ctx, v Values, cur *store.Audit) (*store.Audit, error) {
				return cur, fmt.Errorf("操作日志只读")
			},
			match: func(a *store.Audit, q Query) bool {
				return like(a.Account, q.S("account")) && like(a.Type, q.S("type"))
			},
			less: func(a, b *store.Audit) bool { return a.Time > b.Time },
		})
	m.ReadOnly = true
	m.Save = nil
	m.Delete = nil
	Register(m)
}

// ---- 动态下拉与展示辅助 ----

// 下面几个下拉数据是「展示用」：读不出来就是空列表（页面上表现为下拉没选项），
// 不值得为它把整页打成错误页，所以这里只看一眼错误就返回。

func roleOptions(c *Ctx) []Option {
	roles, err := store.All[*store.Role](c.Store, store.Roles)
	if err != nil {
		return nil
	}
	out := make([]Option, 0, len(roles))
	for _, r := range roles {
		out = append(out, Option{Value: fmt.Sprint(r.ID), Label: r.Name})
	}
	return out
}

func menuOptions(c *Ctx) []Option {
	menus, err := store.All[*store.Menu](c.Store, store.Menus)
	if err != nil {
		return nil
	}
	out := make([]Option, 0, len(menus))
	for _, m := range menus {
		label := m.Name
		if m.URL != "" {
			label = m.Name + "（" + m.URL + "）"
		}
		out = append(out, Option{Value: fmt.Sprint(m.ID), Label: label})
	}
	return out
}

func parentMenuOptions(c *Ctx) []Option {
	out := []Option{{Value: "0", Label: "（顶级菜单）"}}
	menus, err := store.All[*store.Menu](c.Store, store.Menus)
	if err != nil {
		return out
	}
	for _, m := range menus {
		if m.ParentID == 0 {
			out = append(out, Option{Value: fmt.Sprint(m.ID), Label: m.Name})
		}
	}
	return out
}

// menuNamesOf 把菜单 ID 列表翻成一句话，让角色列表一眼能看清权限范围。
func menuNamesOf(s *store.Client, ids []int64) string {
	if len(ids) == 0 {
		return "-"
	}
	menus, err := store.All[*store.Menu](s, store.Menus)
	if err != nil {
		return "-"
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		name := fmt.Sprint(id)
		for _, m := range menus {
			if m.ID == id {
				name = m.Name
				break
			}
		}
		names = append(names, name)
	}
	if len(names) > 6 {
		return fmt.Sprintf("%s 等 %d 项", names[0], len(names))
	}
	out := ""
	for i, n := range names {
		if i > 0 {
			out += "、"
		}
		out += n
	}
	return out
}
