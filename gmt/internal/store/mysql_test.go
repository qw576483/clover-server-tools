package store

import (
	"context"
	"os"
	"strings"
	"testing"
)

// MySQL 往返测试。本机没有可用 MySQL 时跳过（换环境时用 GMT_TEST_DSN 指定）。
//
// 用独立的测试库（clover-gmt-test），结束删表，避免碰到真实数据。
func testClient(t *testing.T) *Client {
	t.Helper()
	conf := DefaultConfig()
	conf.DBName = "clover-gmt-test"
	conf.AutoCreate = true
	if dsn := os.Getenv("GMT_TEST_DSN"); dsn != "" {
		// 允许用连接串覆盖（解析出 host/port/user/pass/db_name）。
		conf = parseTestDSN(t, dsn)
	}
	c, err := NewClient(conf)
	if err != nil {
		t.Skipf("没有可用的 MySQL，跳过：%v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, sc := range Schemas() {
			_, _ = c.Exec(ctx, "DROP TABLE IF EXISTS "+quoteIdent(sc.Name))
		}
		_ = c.Close()
	})
	return c
}

// parseTestDSN 把 user:pass@tcp(host:port)/db 形式的连接串拆成配置。
func parseTestDSN(t *testing.T, dsn string) MySQLConfig {
	t.Helper()
	conf := DefaultConfig()
	at := strings.LastIndex(dsn, "@tcp(")
	if at < 0 {
		t.Fatalf("测试 DSN 格式不对: %s", dsn)
	}
	cred := dsn[:at]
	rest := dsn[at+len("@tcp("):]
	close := strings.Index(rest, ")/")
	if close < 0 {
		t.Fatalf("测试 DSN 格式不对: %s", dsn)
	}
	hostPort := strings.Split(rest[:close], ":")
	db := rest[close+2:]
	if i := strings.Index(db, "?"); i >= 0 {
		db = db[:i]
	}
	user, pass := cred, ""
	if i := strings.Index(cred, ":"); i >= 0 {
		user, pass = cred[:i], cred[i+1:]
	}
	conf.Host = hostPort[0]
	conf.User = user
	conf.Pass = pass
	conf.DBName = db
	if len(hostPort) > 1 {
		conf.Port = atoi(t, hostPort[1])
	}
	return conf
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("端口不是数字: %s", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func TestMySQLRoundTrip(t *testing.T) {
	c := testClient(t)
	if err := c.Ensure(Schemas()); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	// 再跑一次应当幂等。
	if err := c.Ensure(Schemas()); err != nil {
		t.Fatalf("重复建表应当幂等: %v", err)
	}

	// —— 基础增删改查 ——
	menu, err := Put[*Menu](c, Menus, &Menu{Name: "区服管理", URL: "/m/server", Icon: "layers", OrderNo: 20})
	if err != nil {
		t.Fatalf("新增菜单失败: %v", err)
	}
	if menu.ID == 0 {
		t.Fatal("新增后应当拿到自增主键")
	}

	role, err := Put[*Role](c, Roles, &Role{Name: "运营", Remark: "拥有部分权限", MenuIDs: []int64{menu.ID, 7}})
	if err != nil {
		t.Fatalf("新增角色失败: %v", err)
	}
	got, ok, err := Get[*Role](c, Roles, role.ID)
	if err != nil || !ok {
		t.Fatalf("取回角色失败: ok=%v err=%v", ok, err)
	}
	if got.Name != "运营" || got.Remark != "拥有部分权限" {
		t.Fatalf("标题/说明列没有正确往返: %+v", got)
	}
	if len(got.MenuIDs) != 2 || got.MenuIDs[1] != 7 {
		t.Fatalf("切片列没有正确往返: %+v", got.MenuIDs)
	}

	// 值不变时再写一次：不能因为 MySQL「0 行受影响」而误判成主键不存在去补一条。
	if _, err := Put[*Role](c, Roles, got); err != nil {
		t.Fatalf("同值更新失败: %v", err)
	}
	if all, _ := All[*Role](c, Roles); len(all) != 1 {
		t.Fatalf("同值更新后应当仍是 1 行，实际 %d 行", len(all))
	}

	// —— bool / int / DATETIME 列往返 ——
	ban, err := Put[*Ban](c, Bans, &Ban{
		Kind: "account", Target: "badboy", PlayerID: 1001, ServerID: 2,
		Until: 1789000000, Reason: "刷屏", Operator: "admin", CreatedAt: Now(), Active: true,
	})
	if err != nil {
		t.Fatalf("新增封禁失败: %v", err)
	}
	gotBan, ok, err := Get[*Ban](c, Bans, ban.ID)
	if err != nil || !ok {
		t.Fatalf("取回封禁失败: ok=%v err=%v", ok, err)
	}
	if !gotBan.Active || gotBan.Until != 1789000000 || gotBan.CreatedAt <= 0 || gotBan.PlayerID != 1001 {
		t.Fatalf("封禁列没有正确往返: %+v", gotBan)
	}
	// 0 值时间存 NULL，读回来还是 0。
	noExpire, err := Put[*GiftBatch](c, Gifts, &GiftBatch{Name: "无期限批次", Count: 5, Kind: "normal", CreatedAt: Now()})
	if err != nil {
		t.Fatalf("新增礼包批次失败: %v", err)
	}
	if back, _, _ := Get[*GiftBatch](c, Gifts, noExpire.ID); back.ExpireAt != 0 {
		t.Fatalf("未设置的时间应当读回 0，实际 %d", back.ExpireAt)
	}

	// —— 主键不存在时补写 ——
	if _, err := Put[*Menu](c, Menus, &Menu{ID: 4242, Name: "补写", URL: "/handmade"}); err != nil {
		t.Fatalf("主键不存在时应补写: %v", err)
	}
	if m, ok, _ := Get[*Menu](c, Menus, 4242); !ok || m.Name != "补写" {
		t.Fatal("补写的行没读到")
	}
	if ok, err := Del[*Menu](c, Menus, 4242); err != nil || !ok {
		t.Fatalf("删除失败: ok=%v err=%v", ok, err)
	}
	if ok, _ := Del[*Menu](c, Menus, 4242); ok {
		t.Fatal("删两次时第二次应当返回 false")
	}

	// —— 唯一键冲突要能被识别（账号名唯一）——
	if _, err := Put[*Account](c, Accounts, &Account{Name: "admin", PassHash: "x", Status: 1}); err != nil {
		t.Fatalf("新增账号失败: %v", err)
	}
	_, err = Put[*Account](c, Accounts, &Account{Name: "admin", PassHash: "y", Status: 1})
	if err == nil || !IsDuplicateError(err) {
		t.Fatalf("重名账号应当报唯一键冲突，实际 err=%v", err)
	}
}

// 表名不允许撞 MySQL 保留字（引擎遇到 order 就改名 orders 的原因）。
func TestTableNamesAreNotReservedWords(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	for _, sc := range Schemas() {
		rows, err := c.Query(ctx,
			"SELECT COUNT(*) FROM information_schema.KEYWORDS WHERE WORD = UPPER(?) AND RESERVED = 1",
			sc.Name)
		if err != nil {
			t.Skipf("无法查询 information_schema.KEYWORDS（MySQL 版本较老？）：%v", err)
		}
		var reserved int
		for rows.Next() {
			if err := rows.Scan(&reserved); err != nil {
				rows.Close()
				t.Fatal(err)
			}
		}
		rows.Close()
		if reserved > 0 {
			t.Fatalf("表名 %s 是 MySQL 保留字，需要改名（引擎的做法：order → orders）", sc.Name)
		}
	}
}
