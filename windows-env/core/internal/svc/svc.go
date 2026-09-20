// Package svc 定义开发环境依赖的四个服务 (etcd/nats/redis/mysql),
// 并提供路径解析、端口探测、进程/PID 管理、后台拉起等核心能力。
//
// 分层约定 (参考 msg-client):
//   cmd/env/main.go  -> 仅入口: 解析参数, 调 cli.Run
//   internal/svc      -> 服务模型与生命周期 (本包)
//   internal/cli      -> 子命令分发与 <svc>-cmd 客户端
//   internal/util     -> 小工具 (UTF-8 控制台等)
package svc

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qw576483/clover-server-tools/windows-env/core/internal/util"
)

// 是连接 MySQL 时使用的 root 密码。
// 当前数据目录以 --initialize-insecure 初始化, root 无密码, 故此处留空。
// (env 工具不修改 root 密码, 仅在连接客户端/优雅关闭时使用此值。)
const MySQLPass = ""

// 返回 mysql / mysqladmin 的鉴权参数。
// 密码为空时不带 -p, 否则 "-p" 空值会让 mysqladmin/mysql 进入交互式密码提示而卡住。
func MySQLAuthArgs() []string {
	if MySQLPass == "" {
		return []string{"-uroot"}
	}
	return []string{"-uroot", "-p" + MySQLPass}
}

// 描述一个可管理的后台服务。
type Service struct {
	Name    string   // 逻辑名: etcd/nats/redis/mysql
	Title   string   // 显示名
	ExeRel  string   // 相对根目录(windows-env)的可执行文件路径
	Args    []string // 启动参数
	WorkRel string   // 相对根目录的工作目录
	Port    int      // 主监听端口 (用于状态探测)
}

var (
	root   string // 组件根目录: env.exe 的上一级 (windows-env)
	exeDir string // env.exe 自身目录: core/, 运行时文件(logs/run)都放这里
)

// 必须在任何其它函数之前调用: 解析路径并开启 UTF-8 控制台。
func Init() {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	exeDir = filepath.Dir(exe) // core/
	root = resolveRoot(exeDir)  // 向上查找含组件的 windows-env 根目录
	util.EnableUTF8Console()
	util.EnableANSIColors()
}

// 从 exe 目录向上查找"包含 mysql/etcd/nats/redis 组件目录"的根目录。
// 这样无论从 core/、core/cmd 还是 go run 的临时目录启动, 都能定位到正确的 windows-env,
// 不再依赖启动位置 (避免误入 core\cmd 之类路径)。
func resolveRoot(start string) string {
	dir := start
	for i := 0; i < 6; i++ {
		ok := fileExists(filepath.Join(dir, "mysql", "bin", "mysqld.exe")) &&
			fileExists(filepath.Join(dir, "etcd", "etcd.exe")) &&
			fileExists(filepath.Join(dir, "redis", "redis-server.exe")) &&
			fileExists(filepath.Join(dir, "nats", "nats-server.exe"))
		if ok {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Dir(start) // 兜底: 退回 exe 的上一级
}

// 返回组件根目录 (含 etcd/nats/redis/mysql)。
func RootDir() string { return root }

// 返回 env.exe 所在目录 (core/)。
func ExeDir() string { return exeDir }

// 返回全部受管服务的定义。
func Services() []Service {
	return []Service{
		{Name: "etcd", Title: "etcd ", ExeRel: `etcd\etcd.exe`, WorkRel: "etcd", Port: 2379},
		{Name: "nats", Title: "nats ", ExeRel: `nats\nats-server.exe`, WorkRel: "nats", Port: 4222},
		{Name: "redis", Title: "redis", ExeRel: `redis\redis-server.exe`, Args: []string{"redis.conf"}, WorkRel: "redis", Port: 6379},
		{Name: "mysql", Title: "mysql", ExeRel: `mysql\bin\mysqld.exe`, WorkRel: `mysql\bin`, Port: 3306},
	}
}

// 按名称筛选服务; 空或 "all" 返回全部; 未知名称返回 nil。
func Pick(target string) []Service {
	if target == "" || target == "all" {
		return Services()
	}
	for _, s := range Services() {
		if s.Name == target {
			return []Service{s}
		}
	}
	return nil
}

// ---- 基础工具 ----

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func portOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 400*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func pidAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), strconv.Itoa(pid))
}

func killByPid(pid int) bool {
	return exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run() == nil
}

func killByImage(image string) bool {
	return exec.Command("taskkill", "/IM", image, "/T", "/F").Run() == nil
}

func pidPath(name string) string { return filepath.Join(exeDir, "run", name+".pid") }

func writePid(name string, pid int) {
	_ = os.WriteFile(pidPath(name), []byte(strconv.Itoa(pid)), 0o644)
}

func readPid(name string) int {
	b, err := os.ReadFile(pidPath(name))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func removePid(name string) { _ = os.Remove(pidPath(name)) }

// 按进程镜像名扫描, 返回第一个存活的 PID (用于状态兜底显示)。
func pidByImage(image string) int {
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+image, "/NH").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	for i := 0; i+1 < len(fields); i++ {
		if strings.EqualFold(fields[i], image) {
			if p, e := strconv.Atoi(fields[i+1]); e == nil {
				return p
			}
		}
	}
	return 0
}

// 以分离方式在后台启动进程, 日志写入 core/logs/<name>.log, 并记录 PID。
// 子进程脱离当前控制台, 不随本终端关闭而被杀。
func spawn(name, exe string, args []string, workDir string) (int, error) {
	if err := os.MkdirAll(filepath.Join(exeDir, "logs"), 0o755); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Join(exeDir, "run"), 0o755); err != nil {
		return 0, err
	}
	logPath := filepath.Join(exeDir, "logs", name+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, args...)
	cmd.Dir = workDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// DETACHED_PROCESS(0x8) | CREATE_NEW_PROCESS_GROUP(0x200)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200}

	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	writePid(name, pid)
	go func() { _ = cmd.Wait() }() // 释放 Wait 资源, 让子进程完全独立
	return pid, nil
}
