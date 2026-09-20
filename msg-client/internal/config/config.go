// Package config 读取 msg-client 的配置文件（config.yaml）。
//
// 配置决定：
//   - 默认网关 TCP 接入地址；
//   - 业务 proto 文件夹（CLI 启动时递归扫描，自动解析消息号与结构体）。
//
// 引擎内核消息（EMsgLogin/EMsgResumeSession 等）已内置硬编码，无需配置引擎源码路径。
// 所有目录在 Load 时解析为相对配置文件的绝对路径，调用方无需再处理相对路径
// （与 table/core 的 config 包一致）。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// msg-client 配置。
type Config struct {
	// 默认网关 QUIC 接入地址（listen_udp）。
	Addr string `yaml:"addr"`

	// TCP 回退地址（listen_tcp），当 QUIC 连接失败时使用。
	TCPAddr string `yaml:"tcp_addr"`

	// 账号服 HTTP 地址（如 http://127.0.0.1:8051）。**必填**。
	//
	// 引擎只有一种登录模式：账号体系在账号服，游戏服不碰账号表、不收账号密码。
	// login / signup 固定两步：先 HTTP 换 token，再 EMsgLogin{token}。
	AuthAddr string `yaml:"auth_addr"`

	// proto 源文件夹配置。
	Proto ProtoCfg `yaml:"proto"`

	// 配置文件所在目录，用于把相对路径解析为绝对路径。
	dir string `yaml:"-"`
}

// proto 源文件夹（引擎消息已内置，只需配置业务 def 路径）。
type ProtoCfg struct {
	// 业务 proto 文件夹列表（业务侧自定义的 body 类型；可多个，按需配置）。
	Business []string `yaml:"business"`
}

// 返回内置默认配置（用于缺失时生成 config.yaml 与兜底）。
func Default() *Config {
	return &Config{
		Addr:    "", // 必须配置，否则无法连接网关
		TCPAddr: "", // 必须配置，否则无法连接网关
		// AuthAddr 必须配置：留空会在启动时直接报错退出（见 cmd/client 的校验）。
		AuthAddr: "",
		// 业务 proto 目录由使用者按需配置（引擎消息已内置，无需引擎源码路径）。
		Proto: ProtoCfg{Business: []string{}},
	}
}

// 读取并校验配置文件。path 可为相对或绝对路径；找不到则用默认配置并在
// CLI 目录落盘一份 config.yaml，方便用户直接编辑。
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: 解析路径失败: %w", err)
	}
	// #nosec G304 -- 配置文件路径由用户传入并已转绝对路径。
	raw, err := os.ReadFile(abs)
	if err != nil {
		// 找不到：用默认配置，落盘到 CLI 目录，再按它解析。
		d := Default()
		dir := cliDir()
		out, _ := yaml.Marshal(d)
		gen := filepath.Join(dir, "config.yaml")
		// #nosec G306 -- CLI 默认配置文件，写入用户主目录，0600 会在下一条设置。
		if werr := os.WriteFile(gen, out, 0o600); werr == nil {
			fmt.Printf("已生成默认配置: %s\n", gen)
		}
		// #nosec G304 -- 刚由本函数生成的默认配置文件。
		raw, err = os.ReadFile(gen)
		if err != nil {
			return nil, fmt.Errorf("config: 读取默认配置失败: %w", err)
		}
		d.dir = dir
		// 用落盘内容解析（保证字段一致）
		cfg := Default()
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("config: 解析默认配置失败: %w", err)
		}
		cfg.dir = dir
		cfg.resolvePaths()
		return cfg, nil
	}

	cfg := Default()
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("config: 解析 %s 失败: %w", abs, err)
	}
	cfg.dir = filepath.Dir(abs)

	// 兜底：空字段用默认。
	d := Default()
	if cfg.Addr == "" {
		cfg.Addr = d.Addr
	}
	if cfg.TCPAddr == "" {
		cfg.TCPAddr = d.TCPAddr
	}
	if len(cfg.Proto.Business) == 0 {
		cfg.Proto.Business = d.Proto.Business
	}
	cfg.resolvePaths()
	return cfg, nil
}

func (c *Config) resolvePaths() {
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(c.dir, p)
	}
	for i := range c.Proto.Business {
		c.Proto.Business[i] = abs(c.Proto.Business[i])
	}
}

// 配置文件所在目录。
func (c *Config) RootDir() string { return c.dir }

// 返回 CLI 自身目录（用于生成默认配置）。
func cliDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	// runtime.Caller 给的是本源文件路径：.../internal/config/config.go，向上两级到模块根。
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}
