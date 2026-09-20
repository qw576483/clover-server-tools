package store

// Entity 是所有可持久化实体的约定：必须有 int64 主键。
//
// 字段上的 json tag 同时是**数据库列名**（列名与接口字段一致，见 store.go 的推导）；
// 额外的 `col:"time"` 表示这一列在库里是 DATETIME，Go 侧用 int64 秒表示
// （引擎的约定是时间列用 DATETIME，不用 int 时间戳；转换在本包内完成）。
type Entity interface {
	GetID() int64
	SetID(int64)
}

// Account 是后台登录账号。
type Account struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	PassHash    string `json:"pass_hash"`
	Nick        string `json:"nick"`
	RoleID      int64  `json:"role_id"`
	Status      int    `json:"status"` // 1 正常 / 0 禁用
	CreatedAt   int64  `json:"created_at" col:"time"`
	LastLoginAt int64  `json:"last_login_at" col:"time"`
	LastLoginIP string `json:"last_login_ip"`
}

func (a *Account) GetID() int64   { return a.ID }
func (a *Account) SetID(id int64) { a.ID = id }

// Role 是后台角色，权限 = 它能看到的菜单集合。
//
// 说明列叫 Remark 而不是 Desc：desc 是 SQL 保留字（ORDER BY ... DESC）。
type Role struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	Remark  string  `json:"remark"`
	MenuIDs []int64 `json:"menu_ids"`
}

func (r *Role) GetID() int64   { return r.ID }
func (r *Role) SetID(id int64) { r.ID = id }

// Menu 是菜单（同时充当权限项：URL 即权限标识）。
type Menu struct {
	ID       int64  `json:"id"`
	ParentID int64  `json:"parent_id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Icon     string `json:"icon"`
	OrderNo  int    `json:"order_no"`
}

func (m *Menu) GetID() int64   { return m.ID }
func (m *Menu) SetID(id int64) { m.ID = id }

// Machine 是游戏服所在物理机。
type Machine struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	IP      string `json:"ip"`
	InnerIP string `json:"inner_ip"`
	SSHPort int    `json:"ssh_port"`
	SSHUser string `json:"ssh_user"`
	SSHPass string `json:"ssh_pass"`
	IsGame  bool   `json:"is_game"`
	Note    string `json:"note"`
	Status  int    `json:"status"` // 1 正常 / 0 停用
}

func (m *Machine) GetID() int64   { return m.ID }
func (m *Machine) SetID(id int64) { m.ID = id }

// Server 是区服。
type Server struct {
	ID           int64  `json:"id"`
	ServerID     int64  `json:"server_id"`
	Zone         string `json:"zone"`
	Name         string `json:"name"`
	Alias        string `json:"alias"`
	Status       int    `json:"status"` // 1 正常 / 2 维护 / 3 预告 / 0 隐藏
	ParentID     int64  `json:"parent_id"`
	OrderNo      int    `json:"order_no"`
	OpenTime     int64  `json:"open_time" col:"time"`
	WillOpenTime int64  `json:"will_open_time" col:"time"`
}

func (s *Server) GetID() int64   { return s.ID }
func (s *Server) SetID(id int64) { s.ID = id }

// Ban 是封禁留档（真正的封禁动作发给游戏侧，这里只记「封了谁、到什么时候」）。
type Ban struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"` // account 封账号 / role 封角色 / chat 禁言
	Target    string `json:"target"`
	PlayerID  int64  `json:"player_id"`
	ServerID  int64  `json:"server_id"`
	Until     int64  `json:"until" col:"time"`
	Reason    string `json:"reason"`
	Operator  string `json:"operator"`
	CreatedAt int64  `json:"created_at" col:"time"`
	Active    bool   `json:"active"`
}

func (b *Ban) GetID() int64   { return b.ID }
func (b *Ban) SetID(id int64) { b.ID = id }

// GiftBatch 是礼包码批次。
type GiftBatch struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	Count     int    `json:"count"`
	Kind      string `json:"kind"` // normal 通用 / once 一次性
	ServerID  int64  `json:"server_id"`
	ExpireAt  int64  `json:"expire_at" col:"time"`
	Note      string `json:"note"`
	CreatedAt int64  `json:"created_at" col:"time"`
}

func (g *GiftBatch) GetID() int64   { return g.ID }
func (g *GiftBatch) SetID(id int64) { g.ID = id }

// Audit 是后台操作日志：谁在什么时候对什么做了什么。
type Audit struct {
	ID      int64  `json:"id"`
	Time    int64  `json:"time" col:"time"`
	Account string `json:"account"`
	IP      string `json:"ip"`
	Type    string `json:"type"`
	SubType string `json:"sub_type"`
	Info    string `json:"info"`
}

func (a *Audit) GetID() int64   { return a.ID }
func (a *Audit) SetID(id int64) { a.ID = id }
