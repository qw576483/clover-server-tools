package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// 表结构与 clover-server-engine 的写法保持一致：显式 DDL（不靠反射生成 SQL），
// 列名 snake_case，`ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`，建表语句幂等。
//
// 表名不带前缀：gmt 用独立库（默认 clover-gmt），库内没有别的系统的东西要区分。
// 时间列用 DATETIME（引擎统一用 DATETIME，不用 int 时间戳），Go 侧仍是 int64 秒，
// 转换在 store 边上做（见 store.go 的 col:"time"）。
//
// 注意 desc 是 MySQL 保留字，角色表的说明列取名 remark（引擎也是遇到保留字就换名，如 order→orders）。

// Schema 是一张表的建表描述。
type Schema struct {
	Name   string
	Create string
	// Columns 是 Go 侧期望的列，用来在启动时发现「代码加了字段、DDL 忘了加」的漂移。
	Columns []string
}

// Schemas 返回全部建表描述。
func Schemas() []Schema {
	return []Schema{
		{Name: Accounts.Name, Create: createAccount, Columns: Accounts.Columns},
		{Name: Roles.Name, Create: createRole, Columns: Roles.Columns},
		{Name: Menus.Name, Create: createMenu, Columns: Menus.Columns},
		{Name: Machines.Name, Create: createMachine, Columns: Machines.Columns},
		{Name: Servers.Name, Create: createServer, Columns: Servers.Columns},
		{Name: Bans.Name, Create: createBan, Columns: Bans.Columns},
		{Name: Gifts.Name, Create: createGift, Columns: Gifts.Columns},
		{Name: Audits.Name, Create: createAudit, Columns: Audits.Columns},
	}
}

const (
	createAccount = "CREATE TABLE IF NOT EXISTS `account` (\n" +
		"    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,\n" +
		"    name          VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    pass_hash     VARCHAR(255) NOT NULL DEFAULT '',\n" +
		"    nick          VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    role_id       BIGINT       NOT NULL DEFAULT 0,\n" +
		"    status        TINYINT      NOT NULL DEFAULT 0,\n" +
		"    created_at    DATETIME     NULL,\n" +
		"    last_login_at DATETIME     NULL,\n" +
		"    last_login_ip VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    PRIMARY KEY (id),\n" +
		"    UNIQUE KEY uk_name (name)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"

	createRole = "CREATE TABLE IF NOT EXISTS `role` (\n" +
		"    id       BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,\n" +
		"    name     VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    remark   VARCHAR(255) NOT NULL DEFAULT '',\n" +
		"    menu_ids TEXT,\n" +
		"    PRIMARY KEY (id),\n" +
		"    UNIQUE KEY uk_name (name)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"

	createMenu = "CREATE TABLE IF NOT EXISTS `menu` (\n" +
		"    id        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,\n" +
		"    parent_id BIGINT       NOT NULL DEFAULT 0,\n" +
		"    name      VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    url       VARCHAR(255) NOT NULL DEFAULT '',\n" +
		"    icon      VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    order_no  INT          NOT NULL DEFAULT 0,\n" +
		"    PRIMARY KEY (id),\n" +
		"    KEY idx_parent (parent_id, order_no)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"

	createMachine = "CREATE TABLE IF NOT EXISTS `machine` (\n" +
		"    id       BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,\n" +
		"    name     VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    ip       VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    inner_ip VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    ssh_port INT          NOT NULL DEFAULT 0,\n" +
		"    ssh_user VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    ssh_pass VARCHAR(255) NOT NULL DEFAULT '',\n" +
		"    is_game  TINYINT      NOT NULL DEFAULT 0,\n" +
		"    note     TEXT,\n" +
		"    status   TINYINT      NOT NULL DEFAULT 0,\n" +
		"    PRIMARY KEY (id)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"

	createServer = "CREATE TABLE IF NOT EXISTS `server` (\n" +
		"    id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,\n" +
		"    server_id      BIGINT       NOT NULL DEFAULT 0,\n" +
		"    zone           VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    name           VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    alias          VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    status         TINYINT      NOT NULL DEFAULT 0,\n" +
		"    parent_id      BIGINT       NOT NULL DEFAULT 0,\n" +
		"    order_no       INT          NOT NULL DEFAULT 0,\n" +
		"    open_time      DATETIME     NULL,\n" +
		"    will_open_time DATETIME     NULL,\n" +
		"    PRIMARY KEY (id),\n" +
		"    UNIQUE KEY uk_server_id (server_id),\n" +
		"    KEY idx_parent (parent_id, order_no)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"

	createBan = "CREATE TABLE IF NOT EXISTS `ban` (\n" +
		"    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,\n" +
		"    kind       VARCHAR(32)  NOT NULL DEFAULT '',\n" +
		"    target     VARCHAR(255) NOT NULL DEFAULT '',\n" +
		"    player_id  BIGINT       NOT NULL DEFAULT 0,\n" +
		"    server_id  BIGINT       NOT NULL DEFAULT 0,\n" +
		"    until      DATETIME     NULL,\n" +
		"    reason     TEXT,\n" +
		"    operator   VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    created_at DATETIME     NULL,\n" +
		"    active     TINYINT      NOT NULL DEFAULT 0,\n" +
		"    PRIMARY KEY (id),\n" +
		"    KEY idx_target (kind, target),\n" +
		"    KEY idx_active (active, created_at)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"

	createGift = "CREATE TABLE IF NOT EXISTS `gift` (\n" +
		"    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,\n" +
		"    name       VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    prefix     VARCHAR(32)  NOT NULL DEFAULT '',\n" +
		"    count      INT          NOT NULL DEFAULT 0,\n" +
		"    kind       VARCHAR(32)  NOT NULL DEFAULT '',\n" +
		"    server_id  BIGINT       NOT NULL DEFAULT 0,\n" +
		"    expire_at  DATETIME     NULL,\n" +
		"    note       TEXT,\n" +
		"    created_at DATETIME     NULL,\n" +
		"    PRIMARY KEY (id)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"

	createAudit = "CREATE TABLE IF NOT EXISTS `audit` (\n" +
		"    id       BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,\n" +
		"    time     DATETIME     NULL,\n" +
		"    account  VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    ip       VARCHAR(64)  NOT NULL DEFAULT '',\n" +
		"    type     VARCHAR(32)  NOT NULL DEFAULT '',\n" +
		"    sub_type VARCHAR(32)  NOT NULL DEFAULT '',\n" +
		"    info     TEXT,\n" +
		"    PRIMARY KEY (id),\n" +
		"    KEY idx_time (time),\n" +
		"    KEY idx_account (account, time)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
)

// Ensure 建表（幂等），并检查 Go 侧字段与库里的列是否一致。
//
// 只建表、不自动改表：列漂移只告警并给出 ALTER 语句，让人确认后再执行——
// 与引擎的 data.CreateTable 一样，DDL 变更不走「程序自动改结构」这条路。
func (c *Client) Ensure(schemas []Schema) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, sc := range schemas {
		if _, err := c.Exec(ctx, sc.Create); err != nil {
			return fmt.Errorf("store: create table %s: %w", sc.Name, err)
		}
		if err := c.warnDrift(ctx, sc); err != nil {
			return err
		}
	}
	return nil
}

// Check 确认这些表都存在（不建表），缺表时返回错误并列出缺了哪几张。
//
// auto_create_table=false 时用得上：运维自己管 DDL，程序只负责「发现不对就早说」。
func (c *Client) Check(schemas []Schema) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	missing := make([]string, 0, len(schemas))
	for _, sc := range schemas {
		var n int
		err := c.QueryWith(ctx,
			"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
			[]any{sc.Name},
			func(rows *sql.Rows) error {
				if !rows.Next() {
					return sql.ErrNoRows
				}
				return rows.Scan(&n)
			})
		if err != nil {
			return fmt.Errorf("store: check table %s: %w", sc.Name, err)
		}
		if n == 0 {
			missing = append(missing, sc.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("store: 库 %s 缺少表 %v（可把 data.auto_create_table 设为 true 让程序建表）",
			c.conf.DBName, missing)
	}
	return nil
}

// warnDrift 比对库里的列与 Go 侧期望的列，缺列时打告警并给出 ALTER 语句。
func (c *Client) warnDrift(ctx context.Context, sc Schema) error {
	rows, err := c.Query(ctx,
		"SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
		sc.Name)
	if err != nil {
		return fmt.Errorf("store: read columns of %s: %w", sc.Name, err)
	}
	defer rows.Close()
	existing := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	missing := make([]string, 0, 4)
	for _, col := range sc.Columns {
		if !existing[col] {
			missing = append(missing, col)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		log.Printf("store: 表 %s 缺少列 %v —— Go 结构体有字段但 DDL 没加，"+
			"请人工执行 ALTER（程序不会自动改表结构）", sc.Name, missing)
	}
	return nil
}

// quoteIdent 给标识符加反引号（表名/列名一律来自代码常量或结构体 tag，不是用户输入）。
func quoteIdent(name string) string {
	return "`" + strings.Trim(name, "`") + "`"
}
