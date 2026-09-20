// Command gmt 是 Clover 的游戏运营后台。
//
// 它是**独立进程**：不嵌入游戏服，只通过 HTTP 调游戏侧（auth/game/coop/log）。
// 后台自己只保存「后台的数据」——账号、角色、菜单、机器、区服、封禁留档、礼包批次、
// 操作日志，存在 MySQL；玩家、邮件、公告、订单等业务数据一律不落本地。
//
// 数据库配置对齐 clover-server-engine：data.mysql.{host,port,user,pass,db_name,...}，
// 表名不带前缀（用独立库），启动建表 DDL 显式写在 store/schema.go。
//
// 启动：
//
//	cd gmt
//	go run . -conf conf/app.yaml
//
// 默认账号 admin / admin123（库里没有账号时自动创建，登录后请立即修改）。
package main

import (
	"flag"
	"log"

	"gmt/internal/auth"
	"gmt/internal/conf"
	"gmt/internal/gameclient"
	"gmt/internal/modules"
	"gmt/internal/store"
	"gmt/internal/web"
)

func main() {
	confPath := flag.String("conf", "conf/app.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := conf.Load(*confPath)
	if err != nil {
		log.Fatalf("gmt: %v", err)
	}

	// 连不上库就直接退出：宁可起不来，也不要在「看起来正常」的后台里
	// 把数据写到别的地方——那种问题要到运营改数据时才会发现。
	db, err := store.NewClient(cfg.Data.MySQL)
	if err != nil {
		log.Fatalf("gmt: 存储不可用: %v\n     请检查 conf 里的 data.mysql（host/port/user/pass/db_name）", err)
	}
	defer db.Close()

	if cfg.Data.AutoCreateTable {
		if err := db.Ensure(store.Schemas()); err != nil {
			log.Fatalf("gmt: %v", err)
		}
	}
	// 不管建不建表，启动前都确认表在：缺表就报清楚缺哪张，不要等到点开页面才 500。
	if err := db.Check(store.Schemas()); err != nil {
		log.Fatalf("gmt: %v", err)
	}

	// 先保证「有一个能登录的人」，再根据注册的模块同步菜单——
	// 顺序反了的话，超管角色的权限会因为菜单还没建好而为空。
	if err := auth.EnsureBase(db, cfg.Server.Secret); err != nil {
		log.Fatalf("gmt: 初始化默认账号失败: %v", err)
	}
	if err := modules.EnsureMenus(db); err != nil {
		log.Fatalf("gmt: 同步菜单失败: %v", err)
	}

	srv, err := web.New(cfg, db, gameclient.New(cfg))
	if err != nil {
		log.Fatalf("gmt: %v", err)
	}

	c := db.Config()
	log.Printf("gmt 启动: http://%s | mysql: %s:%d/%s | 游戏侧驱动: %s | 模块: %d",
		cfg.Server.Listen, c.Host, c.Port, c.DBName, cfg.Remote.Driver, len(modules.All()))
	if cfg.Remote.Driver == "mock" {
		log.Print("gmt: 当前为 mock 驱动，所有游戏侧数据都是示例，不会真实下发")
	}
	if err := srv.Engine().Run(cfg.Server.Listen); err != nil {
		log.Fatalf("gmt: %v", err)
	}
}
