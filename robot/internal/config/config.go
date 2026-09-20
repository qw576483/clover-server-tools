// Package config 读取 robot 的配置文件（config.yaml）。
//
// 约定与仓库其它工具一致（见 msg-client / manager）：
//   - 文件不存在时自动生成一份默认配置，首次运行即可用；
//   - 已存在时以默认值为底做合并，因此配置里只写关心的字段即可；
//   - 时长字段写成 Go duration 字符串（"60s" / "5m"），解析失败回落默认值。
//
// 本包只负责「连接信息 + 压测默认值」；命令行参数在 cli 层覆盖它。
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
	DefaultGateway     = "127.0.0.1:8003"
	DefaultGatewayTCP  = "127.0.0.1:8002"
	DefaultAuthAddr    = "http://127.0.0.1:8051"
	defaultRobots      = 10
	defaultPrefix      = "robot_"
	defaultStart       = 1
	defaultPassword    = "123123"
	defaultTransport   = "auto"
	defaultMsgInterval = time.Second
	defaultLoginTo     = 10 * time.Second
	defaultSignupConc  = 8
)

// Config robot 的顶层配置。
type Config struct {
	// Gateway 网关 QUIC/UDP 地址（与 msg-client 的 addr 同义）。
	Gateway string `yaml:"gateway"`
	// GatewayTCP 网关 TCP 地址（QUIC 不可用或强制 tcp 时使用）。
	GatewayTCP string `yaml:"gateway_tcp"`
	// AuthAddr 账号服 HTTP 地址。**必填**：游戏服不收账号密码，
	// 登录固定两步（先 HTTP 换 token，再长连接 EMsgLogin{token}）。
	AuthAddr string `yaml:"auth_addr"`

	// Load 压测默认参数（命令行可覆盖）。
	Load LoadConfig `yaml:"load"`

	path string // 配置文件绝对路径（Load 后有效）。
}

// LoadConfig 压测默认参数。
type LoadConfig struct {
	Robots            int    `yaml:"robots"`
	AccountPrefix     string `yaml:"account_prefix"`
	AccountStart      int    `yaml:"account_start"`
	Password          string `yaml:"password"`
	Transport         string `yaml:"transport"`
	RampPerSec        int    `yaml:"ramp_per_sec"`
	Duration          string `yaml:"duration"`
	MsgInterval       string `yaml:"msg_interval"`
	LoginTimeout      string `yaml:"login_timeout"`
	SignupConcurrency int    `yaml:"signup_concurrency"`
}

// Default 返回内置默认配置。
func Default() *Config {
	return &Config{
		Gateway:    DefaultGateway,
		GatewayTCP: DefaultGatewayTCP,
		AuthAddr:   DefaultAuthAddr,
		Load: LoadConfig{
			Robots:            defaultRobots,
			AccountPrefix:     defaultPrefix,
			AccountStart:      defaultStart,
			Password:          defaultPassword,
			Transport:         defaultTransport,
			RampPerSec:        0,
			Duration:          "0s",
			MsgInterval:       defaultMsgInterval.String(),
			LoginTimeout:      defaultLoginTo.String(),
			SignupConcurrency: defaultSignupConc,
		},
	}
}

// Load 读取配置。
// 文件不存在时写入一份默认配置再返回（首次运行友好）。
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

// Duration 解析压测时长；空 / 非法 / 负值一律按 0（登录成功即退出）处理。
func (c *Config) Duration() time.Duration { return nonNegDuration(c.Load.Duration) }

// MsgInterval 解析周期发送间隔；非正时回落 1s（否则会退化成满速刷包）。
func (c *Config) MsgInterval() time.Duration {
	d, err := time.ParseDuration(c.Load.MsgInterval)
	if err != nil || d <= 0 {
		return defaultMsgInterval
	}
	return d
}

// ProbeTimeout QUIC 探测超时（auto 模式快速失败、doctor 自检用）。
//
// 固定 2s：它必须远短于拨号超时，否则 QUIC 没起时每个机器人都要白等一整个
// 拨号超时才能回退 TCP；N 个机器人起来就是 N × 那个超时。
func (c *Config) ProbeTimeout() time.Duration { return 2 * time.Second }

// LoginTimeout 解析单机器人登录总超时；非正时回落 10s。
func (c *Config) LoginTimeout() time.Duration {
	d, err := time.ParseDuration(c.Load.LoginTimeout)
	if err != nil || d <= 0 {
		return defaultLoginTo
	}
	return d
}

// normalize 补齐空字段（文件里缺项时回落默认值）。
func (c *Config) normalize() {
	d := Default()
	if c.Gateway == "" {
		// 注意：这里**不**回落到 "127.0.0.1:8003" 之外的东西——地址必须显式，
		// 静默用一个猜出来的地址去压测比报错更浪费时间。
		c.Gateway = d.Gateway
	}
	if c.GatewayTCP == "" {
		c.GatewayTCP = d.GatewayTCP
	}
	if c.AuthAddr == "" {
		c.AuthAddr = d.AuthAddr
	}
	if c.Load.Robots <= 0 {
		c.Load.Robots = d.Load.Robots
	}
	if c.Load.AccountPrefix == "" {
		c.Load.AccountPrefix = d.Load.AccountPrefix
	}
	if c.Load.AccountStart <= 0 {
		c.Load.AccountStart = d.Load.AccountStart
	}
	if c.Load.Password == "" {
		c.Load.Password = d.Load.Password
	}
	if c.Load.Transport == "" {
		c.Load.Transport = d.Load.Transport
	}
	if c.Load.RampPerSec < 0 {
		c.Load.RampPerSec = 0
	}
	if c.Load.Duration == "" {
		c.Load.Duration = d.Load.Duration
	}
	if c.Load.MsgInterval == "" {
		c.Load.MsgInterval = d.Load.MsgInterval
	}
	if c.Load.LoginTimeout == "" {
		c.Load.LoginTimeout = d.Load.LoginTimeout
	}
	if c.Load.SignupConcurrency <= 0 {
		c.Load.SignupConcurrency = d.Load.SignupConcurrency
	}
}

// save 把当前配置写回磁盘（首次运行时生成模板）。
// 注意：写入的是**未 normalize 的默认值**，所以模板里会保留空串字段，
// 由使用者自己填；这是刻意的——不要替使用者猜地址。
func (c *Config) save() error {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, raw, 0o644)
}

// nonNegDuration 解析 duration，负数 / 非法 / 空一律归零。
func nonNegDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0
	}
	return d
}
