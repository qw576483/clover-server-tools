package gameclient

import (
	"context"
	"fmt"
	"time"

	"github.com/qw576483/clover-server-tools/gmt/internal/conf"
)

// mockClient 不发真实请求，返回可预期的样例数据。
//
// 它的价值不在「假数据好看」，而在让后台**脱离游戏服也能跑通全链路**：
// 装环境、改页面、演示、压前端都用它。所以每个方法都必须返回结构完整的数据，
// 而不是空数组——空数组会让页面看起来像「功能坏了」。
type mockClient struct {
	cfg *conf.Config
}

func (m *mockClient) Driver() string { return "mock" }

func (m *mockClient) Health(ctx context.Context) []NodeHealth {
	out := make([]NodeHealth, 0, len(m.cfg.Remote.Endpoints))
	for node, addr := range m.cfg.Remote.Endpoints {
		out = append(out, NodeHealth{
			Node:    node,
			Addr:    addr,
			Online:  true,
			Latency: 1,
			Message: "mock 模式，未真实探测",
		})
	}
	return out
}

func (m *mockClient) SearchPlayer(ctx context.Context, serverID int64, kind, keyword string) (*Player, error) {
	if keyword == "" {
		return nil, fmt.Errorf("请输入查询关键字")
	}
	now := time.Now().Unix()
	return &Player{
		PlayerID:   100001,
		Name:       "示例角色",
		Account:    keyword,
		ServerID:   serverID,
		Level:      60,
		VIP:        5,
		Power:      128000,
		Guild:      "示例军团",
		Online:     true,
		CreateTime: now - 30*24*3600,
		LastLogin:  now - 3600,
		Items: []Item{
			{ConfigID: 1001, Num: 99},
			{ConfigID: 2002, Num: 3},
		},
	}, nil
}

func (m *mockClient) SendMail(ctx context.Context, req MailRequest) error {
	if req.Title == "" {
		return fmt.Errorf("邮件标题不能为空")
	}
	if !req.All && len(req.Targets) == 0 {
		return fmt.Errorf("个人邮件必须填至少一个玩家 ID")
	}
	return nil
}

var mockBulletins = []Bulletin{
	{ID: 1, Type: "login", Title: "开服公告", Content: "欢迎来到示例服", StartAt: time.Now().Unix() - 3600, EndAt: time.Now().Unix() + 7*24*3600, Enabled: true},
	{ID: 2, Type: "roll", Title: "双倍活动", Content: "今晚 20:00 双倍经验", StartAt: time.Now().Unix(), EndAt: time.Now().Unix() + 3*24*3600, Interval: 300, Enabled: true},
}

func (m *mockClient) ListBulletins(ctx context.Context) ([]Bulletin, error) {
	out := make([]Bulletin, len(mockBulletins))
	copy(out, mockBulletins)
	return out, nil
}

func (m *mockClient) SaveBulletin(ctx context.Context, b Bulletin) error {
	for i := range mockBulletins {
		if mockBulletins[i].ID == b.ID {
			mockBulletins[i] = b
			return nil
		}
	}
	b.ID = int64(len(mockBulletins) + 1)
	mockBulletins = append(mockBulletins, b)
	return nil
}

func (m *mockClient) DeleteBulletin(ctx context.Context, id int64) error {
	for i := range mockBulletins {
		if mockBulletins[i].ID == id {
			mockBulletins = append(mockBulletins[:i], mockBulletins[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("公告 %d 不存在", id)
}

func (m *mockClient) ApplyBan(ctx context.Context, req BanRequest) error {
	if req.Target == "" {
		return fmt.Errorf("封禁目标不能为空")
	}
	return nil
}

func (m *mockClient) LiftBan(ctx context.Context, req BanRequest) error { return nil }

func (m *mockClient) GenGift(ctx context.Context, req GiftRequest) ([]string, error) {
	if req.Count <= 0 || req.Count > 200 {
		return nil, fmt.Errorf("单次生成数量需在 1~200 之间")
	}
	out := make([]string, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		out = append(out, fmt.Sprintf("%s%04d%03d", req.Prefix, i+1, req.ServerID%1000))
	}
	return out, nil
}

func (m *mockClient) QueryOrders(ctx context.Context, q OrderQuery) ([]Order, error) {
	now := time.Now().Unix()
	out := make([]Order, 0, 3)
	for i := 0; i < 3; i++ {
		out = append(out, Order{
			OrderID:  fmt.Sprintf("MOCK%06d", i+1),
			Account:  "test_account",
			PlayerID: 100001,
			ServerID: q.ServerID,
			Product:  "月卡",
			Amount:   int64(30 + i*10),
			Status:   1,
			Channel:  "mock",
			Time:     now - int64(i)*3600,
		})
	}
	return out, nil
}
