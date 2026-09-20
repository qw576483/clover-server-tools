// Package config 读取 manager 的配置文件（config.yaml）。
//
// 约定与仓库其它工具一致：文件不存在时自动生成一份默认配置，首次运行即可用；
// 时长字段写成 Go duration 字符串（"3s" / "5m"），解析失败回落到默认值。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// 默认值。
const (
	defaultEtcdDialTimeout = 3 * time.Second
	defaultAdminTimeout    = 5 * time.Second
	defaultAdminPort       = 8041
)

// Config manager 的顶层配置。
type Config struct {
	Etcd  EtcdConfig  `yaml:"etcd"`
	Admin AdminConfig `yaml:"admin"`

	path string // 配置文件绝对路径（Load 后有效）。
}

// EtcdConfig etcd 连接配置：节点目录与服务发现的唯一数据源。
type EtcdConfig struct {
	Endpoints   []string `yaml:"endpoints"`
	Username    string   `yaml:"username"`
	Password    string   `yaml:"password"`
	DialTimeout string   `yaml:"dial_timeout"`
}

// AdminConfig admin 控制面 HTTP 访问配置。
type AdminConfig struct {
	Timeout string `yaml:"timeout"`
	// DefaultPort 节点目录未上报 admin 地址时的兜底端口。
	// 节点会自报 admin.listen_addr，只有老版本（未上报）节点才需要它。
	DefaultPort int `yaml:"default_port"`
	// Token admin 控制面鉴权令牌，取值必须与**被管节点的** `admin.token` 一致。
	//
	// 空 = 不发令牌头（默认，对应「节点未启用鉴权」的部署；此时节点按
	// AdminConfig.Normalize 只可能绑回环，manager 也只能管本机）。
	// 非空 = 每个 admin 请求都带 `X-Admin-Token: <token>`；被管节点一旦配了
	// admin.token，不带这个头就会被 401 拒绝（drain / shutdown / upstream / drain-status）。
	//
	// 为什么不做成 CLI 参数：令牌是**长期凭据**，写在命令行上会进 shell 历史与进程列表；
	// 配置文件是唯一入口，`-config` 可指到权限收紧的路径。
	Token string `yaml:"token"`
}

// Default 返回内置默认配置。
func Default() *Config {
	return &Config{
		Etcd: EtcdConfig{
			Endpoints:   []string{"127.0.0.1:2379"},
			DialTimeout: defaultEtcdDialTimeout.String(),
		},
		Admin: AdminConfig{
			Timeout:     defaultAdminTimeout.String(),
			DefaultPort: defaultAdminPort,
		},
	}
}

// Load 读取配置。
// 文件不存在时写入一份默认配置再返回（首次运行友好）；已存在时以默认值为底做合并，
// 因此配置里只写关心的字段即可。
func Load(path string) (*Config, error) {
	if path == "" {
		path = "config.yaml"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: resolve %s: %w", path, err)
	}

	raw, err := os.ReadFile(abs)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("config: read %s: %w", abs, err)
		}
		cfg := Default()
		cfg.path = abs
		if werr := cfg.save(); werr != nil {
			// 写不出默认文件不阻断本次运行，只是下次还得重来。
			return cfg, fmt.Errorf("config: write default %s: %w", abs, werr)
		}
		return cfg, nil
	}

	cfg := Default()
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", abs, err)
	}
	cfg.path = abs
	cfg.normalize()
	return cfg, nil
}

// Path 返回配置文件绝对路径。
func (c *Config) Path() string { return c.path }

// EtcdDialTimeout 返回 etcd 连接超时。
func (c *Config) EtcdDialTimeout() time.Duration {
	return parseDurationOr(c.Etcd.DialTimeout, defaultEtcdDialTimeout)
}

// AdminTimeout 返回单次 admin 请求超时。
func (c *Config) AdminTimeout() time.Duration {
	return parseDurationOr(c.Admin.Timeout, defaultAdminTimeout)
}

// AdminToken 返回 admin 控制面鉴权令牌；空串表示不发令牌头。
//
// 原样返回（不做 TrimSpace）：引擎侧是逐字节比较（`subtle.ConstantTimeCompare`），
// 这里替调用方「顺手修一下空白」会让两边悄悄不一致，反而变成难查的 401。
func (c *Config) AdminToken() string {
	return c.Admin.Token
}

// normalize 补齐空字段（文件里缺项时回落默认值）。
func (c *Config) normalize() {
	if len(c.Etcd.Endpoints) == 0 {
		c.Etcd.Endpoints = Default().Etcd.Endpoints
	}
	if c.Etcd.DialTimeout == "" {
		c.Etcd.DialTimeout = defaultEtcdDialTimeout.String()
	}
	if c.Admin.Timeout == "" {
		c.Admin.Timeout = defaultAdminTimeout.String()
	}
	if c.Admin.DefaultPort <= 0 {
		c.Admin.DefaultPort = defaultAdminPort
	}
}

// save 把当前配置写回磁盘（首次运行时生成模板）。
func (c *Config) save() error {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, raw, 0o644)
}

// parseDurationOr 解析 duration 字符串；空串 / 非法 / 非正值一律回落默认。
func parseDurationOr(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
