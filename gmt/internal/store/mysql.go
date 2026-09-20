package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	mysqlerr "github.com/go-sql-driver/mysql"
)

// MySQL 连接配置与客户端：字段、默认值、DSN 拼装、建库方式全部对齐
// clover-server-engine（internal/domain/data/store/mysql）：同一个驱动、同一套 yaml 键名，
// 一个库里的服务配置写法保持一致。
//
// yaml 里对应：
//
//	data:
//	  auto_create_table: true
//	  mysql:
//	    host: 127.0.0.1
//	    port: 3306
//	    user: root
//	    pass: ""
//	    db_name: clover-gmt
type MySQLConfig struct {
	Host            string        `yaml:"host"`
	Port            int           `yaml:"port"`
	User            string        `yaml:"user"`
	Pass            string        `yaml:"pass"`
	DBName          string        `yaml:"db_name"`
	Charset         string        `yaml:"charset"`
	ParseTime       bool          `yaml:"parse_time"` // 解析 DATETIME/DATE 为 time.Time
	Loc             string        `yaml:"loc"`        // 时区，默认 Local
	AutoCreate      bool          `yaml:"auto_create"`
	MaxOpenConns    int           `yaml:"max_open_conns"`
	MaxIdleConns    int           `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time"`
	DialTimeout     time.Duration `yaml:"dial_timeout"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
}

// 默认值表：与 clover-server-engine 的默认值一致（端口/字符集/池大小/各超时）。
const (
	defaultPort            = 3306
	defaultCharset         = "utf8mb4"
	defaultLoc             = "Local"
	defaultMaxOpenConns    = 20
	defaultMaxIdleConns    = 10
	defaultConnMaxLifetime = time.Hour
	defaultConnMaxIdleTime = 30 * time.Minute
	defaultDialTimeout     = 5 * time.Second
	defaultReadTimeout     = 3 * time.Second
	defaultWriteTimeout    = 3 * time.Second
)

// DefaultConfig 返回本地开发可用的默认配置（在 conf 层被用来补零值）。
func DefaultConfig() MySQLConfig {
	return MySQLConfig{
		Host:      "127.0.0.1",
		Port:      defaultPort,
		User:      "root",
		Pass:      "",
		DBName:    "clover-gmt",
		Charset:   defaultCharset,
		ParseTime: true,
		Loc:       defaultLoc,
	}.Normalize()
}

// Normalize 补全零值字段（Host/User/DBName 非空由调用方保证）。
func (conf MySQLConfig) Normalize() MySQLConfig {
	c := conf
	c.Port = defInt(c.Port, defaultPort)
	c.Charset = defString(c.Charset, defaultCharset)
	c.Loc = defString(c.Loc, defaultLoc)
	c.MaxOpenConns = defInt(c.MaxOpenConns, defaultMaxOpenConns)
	c.MaxIdleConns = defInt(c.MaxIdleConns, defaultMaxIdleConns)
	// 空闲连接不能多于总连接数，否则连接池行为异常。
	if c.MaxIdleConns > c.MaxOpenConns {
		c.MaxIdleConns = c.MaxOpenConns
	}
	c.ConnMaxLifetime = defDuration(c.ConnMaxLifetime, defaultConnMaxLifetime)
	c.ConnMaxIdleTime = defDuration(c.ConnMaxIdleTime, defaultConnMaxIdleTime)
	c.DialTimeout = defDuration(c.DialTimeout, defaultDialTimeout)
	c.ReadTimeout = defDuration(c.ReadTimeout, defaultReadTimeout)
	c.WriteTimeout = defDuration(c.WriteTimeout, defaultWriteTimeout)
	return c
}

// DSN 生成连接串（带库名）。Host/User/DBName 必须非空。
func (conf MySQLConfig) DSN() (string, error) {
	if strings.TrimSpace(conf.DBName) == "" {
		return "", fmt.Errorf("mysql DSN: db_name is required")
	}
	return conf.dsn(conf.DBName)
}

// rootDSN 生成不带库名的连接串（建库前连上去用）。
func (conf MySQLConfig) rootDSN() (string, error) { return conf.dsn("") }

func (conf MySQLConfig) dsn(dbName string) (string, error) {
	if strings.TrimSpace(conf.Host) == "" {
		return "", fmt.Errorf("mysql DSN: host is required")
	}
	if conf.Port <= 0 {
		return "", fmt.Errorf("mysql DSN: port must be positive")
	}
	if strings.TrimSpace(conf.User) == "" {
		return "", fmt.Errorf("mysql DSN: user is required")
	}
	// 用驱动的 FormatDSN 拼装：手写 "%s:%s@tcp(...)" 时，密码里的 @ : / ? 会截断 DSN
	// （做法与 clover-server-engine 的 internal/domain/data/store/mysql 保持一致）。
	cfg := mysqlerr.NewConfig()
	cfg.User = conf.User
	cfg.Passwd = conf.Pass
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%d", conf.Host, conf.Port)
	cfg.DBName = dbName
	cfg.Params = map[string]string{
		"charset":   conf.Charset,
		"parseTime": strconvBool(conf.ParseTime),
		"loc":       conf.Loc,
	}
	if conf.DialTimeout > 0 {
		cfg.Params["timeout"] = conf.DialTimeout.String()
	}
	if conf.ReadTimeout > 0 {
		cfg.Params["readTimeout"] = conf.ReadTimeout.String()
	}
	if conf.WriteTimeout > 0 {
		cfg.Params["writeTimeout"] = conf.WriteTimeout.String()
	}
	return cfg.FormatDSN()
}

// Client 是 MySQL 客户端（连接池 + 建库 + 建表入口）。
type Client struct {
	conf MySQLConfig
	db   *sql.DB
}

// NewClient 建连接池：必要时先建库，再 Ping 探活。
func NewClient(conf MySQLConfig) (*Client, error) {
	if strings.TrimSpace(conf.Host) == "" {
		return nil, fmt.Errorf("mysql: host is required")
	}
	if strings.TrimSpace(conf.User) == "" {
		return nil, fmt.Errorf("mysql: user is required")
	}
	if strings.TrimSpace(conf.DBName) == "" {
		return nil, fmt.Errorf("mysql: db_name is required")
	}
	if err := sanitizeDBName(conf.DBName); err != nil {
		return nil, fmt.Errorf("mysql: %w", err)
	}
	c := conf.Normalize()
	dsn, err := c.DSN()
	if err != nil {
		return nil, err
	}
	if c.AutoCreate {
		if err := ensureDatabase(c); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("mysql open %s:%d/%s: %w", c.Host, c.Port, c.DBName, err)
	}
	db.SetMaxOpenConns(c.MaxOpenConns)
	db.SetMaxIdleConns(c.MaxIdleConns)
	db.SetConnMaxLifetime(c.ConnMaxLifetime)
	db.SetConnMaxIdleTime(c.ConnMaxIdleTime)

	ctx, cancel := context.WithTimeout(context.Background(), c.DialTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysql ping %s:%d/%s: %w", c.Host, c.Port, c.DBName, err)
	}
	log.Printf("mysql connected: %s:%d db=%s max_open=%d max_idle=%d",
		c.Host, c.Port, c.DBName, c.MaxOpenConns, c.MaxIdleConns)
	return &Client{conf: c, db: db}, nil
}

// Close 关闭连接池。
func (c *Client) Close() error { return c.db.Close() }

// Raw 返回底层连接池。
func (c *Client) Raw() *sql.DB { return c.db }

// Config 返回配置副本。
func (c *Client) Config() MySQLConfig { return c.conf }

// Ping 健康检查。
func (c *Client) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

// Exec 执行写操作（含 DDL）。
func (c *Client) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return c.db.ExecContext(ctx, query, args...)
}

// Query 执行查询，调用方负责关闭 rows。
func (c *Client) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return c.db.QueryContext(ctx, query, args...)
}

// QueryWith 在回调里处理结果集并自动关闭 rows。
func (c *Client) QueryWith(ctx context.Context, query string, args []any, fn func(*sql.Rows) error) error {
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	return fn(rows)
}

// IsDuplicateError 判断是否唯一键冲突（错误号 1062）。
func IsDuplicateError(err error) bool {
	var e *mysqlerr.MySQLError
	if errors.As(err, &e) && e.Number == 1062 {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

// sanitizeDBName 库名白名单校验，防注入（与引擎一致：允许字母数字下划线与短横线）。
func sanitizeDBName(name string) error {
	for _, ch := range name {
		ok := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '_' || ch == '-'
		if !ok {
			return fmt.Errorf("invalid db_name character %q", ch)
		}
	}
	return nil
}

// ensureDatabase 建库（幂等）。
func ensureDatabase(c MySQLConfig) error {
	if err := sanitizeDBName(c.DBName); err != nil {
		return fmt.Errorf("mysql: %w", err)
	}
	root, err := c.rootDSN()
	if err != nil {
		return err
	}
	db, err := sql.Open("mysql", root)
	if err != nil {
		return fmt.Errorf("mysql open (system) %s:%d: %w", c.Host, c.Port, err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), c.DialTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("mysql ping (system) %s:%d: %w", c.Host, c.Port, err)
	}
	q := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET %s", c.DBName, c.Charset)
	if _, err := db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("mysql create database %s: %w", c.DBName, err)
	}
	log.Printf("mysql ensure database: %s (created or already exists)", c.DBName)
	return nil
}

func defInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func defString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func defDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func strconvBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
