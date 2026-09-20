// Package conf 负责 gmt 自身的配置加载。
//
// 配置只有一个 yaml 文件，节点命名与 clover-server-engine 对齐：
// 数据库在 data.mysql 下（字段名与引擎的 mysql.MySQLConfig 一一对应），
// 缺项走默认值，保证「拷一份就能跑」。
package conf

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/qw576483/clover-server-tools/gmt/internal/store"
)

// Config 是 gmt 的全部配置。
type Config struct {
	Server ServerConf `yaml:"server"`
	Site   SiteConf   `yaml:"site"`
	Data   DataConf   `yaml:"data"`
	Remote RemoteConf `yaml:"remote"`
}

// ServerConf 是 gmt 自己的 HTTP 服务配置。
type ServerConf struct {
	Listen string `yaml:"listen"`
	// Secret 用于会话签名、口令散列加盐与验证码签名。**换了它，所有已登录会话失效**，
	// 这正好是「强制所有人重新登录」的手段。
	Secret string `yaml:"secret"`
}

// SiteConf 是页面展示用的站点信息。
type SiteConf struct {
	Name     string `yaml:"name"`
	Timezone string `yaml:"timezone"`
}

// DataConf 是 gmt 自身数据的存放方式（与引擎的 data 节点同构）。
type DataConf struct {
	// AutoCreateTable 为 true 时启动执行 CREATE TABLE IF NOT EXISTS。
	AutoCreateTable bool `yaml:"auto_create_table"`
	// MySQL 是连接配置，字段与 clover-server-engine 的 mysql.MySQLConfig 相同。
	MySQL store.MySQLConfig `yaml:"mysql"`
}

// RemoteConf 描述游戏侧（auth/game/coop/log）怎么连。
//
// driver=mock 时不发任何真实请求，返回可预期的假数据——
// 目的是让后台在没有游戏服的环境下也能完整点通、演示、做前端联调。
type RemoteConf struct {
	Driver    string            `yaml:"driver"`
	TimeoutMS int               `yaml:"timeout_ms"`
	Endpoints map[string]string `yaml:"endpoints"`
}

// Timeout 返回调用游戏侧的超时时间。
func (c *Config) Timeout() time.Duration {
	if c.Remote.TimeoutMS <= 0 {
		return 5 * time.Second
	}
	return time.Duration(c.Remote.TimeoutMS) * time.Millisecond
}

// Load 读取并补全配置。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	c := new(Config)
	if err := yaml.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	applyDefaults(c)
	return c, nil
}

func applyDefaults(c *Config) {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:9000"
	}
	if c.Server.Secret == "" {
		// 没配就用一个固定串：单进程本地后台够用，但生产必须改。
		c.Server.Secret = "clover-gmt-default-secret"
	}
	if c.Site.Name == "" {
		c.Site.Name = "Clover GMT"
	}
	if c.Site.Timezone == "" {
		c.Site.Timezone = "Asia/Shanghai"
	}

	// 数据库缺项用引擎那套开发默认值补（127.0.0.1:3306 / root / 空口令 / clover-gmt）。
	def := store.DefaultConfig()
	m := &c.Data.MySQL
	if m.Host == "" {
		m.Host = def.Host
	}
	if m.Port == 0 {
		m.Port = def.Port
	}
	if m.User == "" {
		m.User = def.User
	}
	if m.DBName == "" {
		m.DBName = def.DBName
	}
	if m.Charset == "" {
		m.Charset = def.Charset
	}
	if m.Loc == "" {
		m.Loc = def.Loc
	}
	// gmt 的时间列是 DATETIME，需要驱动把 DATETIME 解析成 time.Time。
	m.ParseTime = true
	// 其余（连接池大小、各类超时）在 store.NewClient 里按同一张默认值表补齐。

	if c.Remote.Driver == "" {
		c.Remote.Driver = "mock"
	}
	if len(c.Remote.Endpoints) == 0 {
		c.Remote.Endpoints = map[string]string{
			"auth": "http://127.0.0.1:8081",
			"game": "http://127.0.0.1:8082",
			"coop": "http://127.0.0.1:8083",
			"log":  "http://127.0.0.1:8084",
		}
	}
}
