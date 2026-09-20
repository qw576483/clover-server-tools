// Package cli 负责子命令解析与分发, 以及 <svc> 客户端入口。
// 入口 main.go 只调用 cli.Run, 保持薄。
package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/qw576483/clover-server-tools/windows-env/core/internal/svc"
	"github.com/qw576483/clover-server-tools/windows-env/core/internal/util"
)

// 组件下载地址, 与 core/README.md 保持一致 (取自 README)。
var downloadURLs = map[string]string{
	"etcd":  "https://github.com/etcd-io/etcd/releases （etcd-vX.X.X-windows-amd64.zip）",
	"nats":  "https://github.com/nats-io/nats-server/releases （nats-server-vX.X.X-windows-amd64.zip）",
	"redis": "https://github.com/redis-windows/redis-windows/releases （Redis-X.X.X-Windows-X64-msys2.zip）",
	"mysql": "https://dev.mysql.com/downloads/mysql/ （Windows x86 64-bit 的 ZIP Archive）",
}

// 解析子命令并分发。args 不含程序名。
// 双击 env.exe (无参数) 时: 先检查组件目录, 缺失则直接报告并退出 (不进菜单);
// 齐全才进入交互菜单, 窗口常驻, 输入 q 退出。
func Run(args []string) {
	if len(args) == 0 {
		// 双击场景: 缺组件时报告后必须暂停, 否则窗口一闪而过看不到内容。
		if missing := checkMissing(); len(missing) > 0 {
			reportMissing(missing)
			pauseExit()
			return
		}
		interactive()
		return
	}
	dispatch(args)
}

// 在双击(无参)退出前阻塞窗口, 等待用户按回车, 避免一闪而过。
func pauseExit() {
	fmt.Println()
	fmt.Print("按回车键退出...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// 返回缺失的组件名列表 (按 svc.Services 期望的目录/可执行文件探测)。
func checkMissing() []string {
	var missing []string
	for _, s := range svc.Services() {
		p := filepath.Join(svc.RootDir(), s.ExeRel)
		if _, err := os.Stat(p); err != nil {
			missing = append(missing, s.Name)
		}
	}
	return missing
}

// 打印缺失组件与下载地址 (地址取自 README)。
func reportMissing(missing []string) {
	sep := util.Amber("============================================================")
	fmt.Println(sep)
	fmt.Println(util.Red(" 环境检查未通过: 以下组件目录/可执行文件缺失"))
	fmt.Println(sep)
	for _, name := range missing {
		fmt.Printf("   %s %-6s 期望: %s\\%s\\ (含可执行文件)\n", util.Red("[缺失]"), name, svc.RootDir(), name)
	}
	fmt.Println()
	fmt.Println(util.Yellow(" 请先放置对应组件, 下载地址见 core/README.md:"))
	for _, name := range missing {
		fmt.Printf("   %-6s %s\n", util.Cyan(name), downloadURLs[name])
	}
	fmt.Println()
	fmt.Println(util.Dim(" 放置正确后重新运行 env.exe。"))
}

// 执行具体子命令 (供命令行与交互菜单共用)。
func dispatch(args []string) {
	cmd := strings.ToLower(args[0])
	target := ""
	if len(args) > 1 {
		target = strings.ToLower(args[1])
	}
	switch cmd {
	case "start", "open":
		// open 不带参=全部, 带参(如 open redis)只操作指定服务
		if cmd == "open" && target == "" {
			target = "all"
		}
		if missing := checkMissing(); len(missing) > 0 {
			reportMissing(missing)
			return
		}
		svc.CheckAndStart(target)
	case "stop", "close":
		// close 不带参=全部, 带参(如 close redis)只操作指定服务
		if cmd == "close" && target == "" {
			target = "all"
		}
		svc.Stop(target)
	case "restart":
		if target == "" {
			target = "all"
		}
		if missing := checkMissing(); len(missing) > 0 {
			reportMissing(missing)
			return
		}
		svc.Stop(target)
		time.Sleep(2 * time.Second)
		svc.CheckAndStart(target)
	case "info", "status", "ps":
		svc.Info()
	// 直接输入服务名 = 启动该服务 (与 cxxx 客户端区分)
	case "etcd", "nats", "redis", "mysql":
		svc.CheckAndStart(cmd)
	case "mysql-cmd", "redis-cmd":
		openClient(strings.TrimSuffix(cmd, "-cmd"), args[1:])
	case "credis", "cmysql":
		openClient(strings.TrimPrefix(cmd, "c"), args[1:])
	case "help", "-h", "--help", "/?":
		usage()
	default:
		fmt.Printf("%s %s\n\n", util.Red("[未知命令]"), cmd)
		usage()
	}
}

// 无参数时的交互菜单模式。
func interactive() {
	reader := bufio.NewReader(os.Stdin)
	// 数字/字母快捷键 -> 子命令 (不再单拎 redis, 四个服务一视同仁)
	shortcuts := map[string][]string{
		"1": {"open"},
		"2": {"close"},
		"3": {"status"},
		"?": {"help"},
		"h": {"help"},
	}
	fmt.Println(util.Amber("Clover Windows 开发环境 - 交互模式 (输入 q 退出, 输入 ? 看帮助)"))
	printMenu()
	for {
		fmt.Print(util.Amber("> "))
		line, err := reader.ReadString('\n')
		if err != nil {
			// Ctrl+C / 意外 EOF: 换行继续, 不退出交互模式 (输入 q 退出)
			fmt.Println()
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		if low == "q" || low == "quit" || low == "exit" {
			fmt.Println("已退出")
			return
		}
		if s, ok := shortcuts[low]; ok {
			dispatch(s)
		} else {
			dispatch(strings.Fields(low))
		}
		fmt.Println()
	}
}

// 打印交互菜单 (统一命令词, 不偏袒任一服务)。
func printMenu() {
	sep := util.Amber("--------------------------------------------------")
	fmt.Println(sep)
	fmt.Println(" " + util.Cyan("1)") + " open    " + util.Dim("启动全部"))
	fmt.Println(" " + util.Cyan("2)") + " close   " + util.Dim("停止全部"))
	fmt.Println(" " + util.Cyan("3)") + " status  " + util.Dim("查看状态"))
	fmt.Println(" " + util.Cyan("?)") + " help    " + util.Dim("帮助"))
	fmt.Println(" " + util.Bold("客户端:") + " " + util.Cyan("cmysql") + " / " + util.Cyan("credis"))
	fmt.Println(" " + util.Dim("直接输入服务名可单独启动:") + " " + util.Cyan("etcd") + " / " + util.Cyan("nats") + " / " + util.Cyan("redis") + " / " + util.Cyan("mysql"))
	fmt.Println("    " + util.Dim("也可直接输入命令, 如:") + " open / close / status / cmysql / mysql")
	fmt.Println(sep)
}

// 打开某个服务的命令行客户端。
func openClient(name string, extra []string) {
	root := svc.RootDir()
	switch name {
	case "mysql":
		runClient(filepath.Join(root, `mysql\bin\mysql.exe`),
			svc.MySQLAuthArgs(),
			extra, filepath.Join(root, `mysql\bin`))
	case "redis":
		runClient(filepath.Join(root, `redis\redis-cli.exe`),
			[]string{"-p", "6379"},
			extra, filepath.Join(root, `redis`))
	default:
		fmt.Printf("未知客户端: %s (可选: mysql/redis)\n", name)
	}
}

// runClient: 无额外参数则新开控制台窗口进入交互模式;
// 带额外参数则直接执行, 结果打到当前终端。
func runClient(exe string, base, extra []string, work string) {
	if _, err := os.Stat(exe); err != nil {
		fmt.Printf("未找到客户端: %s\n", exe)
		return
	}
	if len(extra) == 0 {
		full := exe + " " + strings.Join(base, " ")
		c := exec.Command("cmd", "/c", "start", "", "cmd", "/k", full)
		c.Dir = work
		if err := c.Run(); err != nil {
			fmt.Println("打开客户端失败:", err)
		}
		return
	}
	c := exec.Command(exe, append(base, extra...)...)
	c.Dir = work
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	_ = c.Run()
}

func usage() {
	sep := util.Amber("============================================================")
	fmt.Println(sep)
	fmt.Println(util.Amber("  Clover Windows 开发环境管理工具 (env.exe)"))
	fmt.Println(sep)
	fmt.Println()
	fmt.Println(util.Cyan("用法:"))
	fmt.Println("  " + util.Cyan("env.exe open  [name]") + "  " + util.Dim("启动服务 (name 可选, 默认全部)  [别名: start]"))
	fmt.Println("  " + util.Cyan("env.exe close [name]") + "  " + util.Dim("停止服务 (默认全部)            [别名: stop]"))
	fmt.Println("  " + util.Cyan("env.exe restart [name]") + "  " + util.Dim("重启服务 (默认全部)"))
	fmt.Println("  " + util.Cyan("env.exe status") + "        " + util.Dim("查看各服务运行状态           [别名: info]"))
	fmt.Println("  " + util.Cyan("env.exe cmysql|credis [args]") + "  " + util.Dim("打开服务命令行客户端"))
	fmt.Println("  " + util.Cyan("env.exe help") + "          " + util.Dim("显示本帮助"))
	fmt.Println()
	fmt.Println(util.Cyan("快捷键:") + " " + util.Yellow("credis") + "=" + util.Yellow("redis-cmd") + ", " + util.Yellow("cmysql") + "=" + util.Yellow("mysql-cmd"))
	fmt.Println("  name: " + util.Cyan("etcd") + " | " + util.Cyan("nats") + " | " + util.Cyan("redis") + " | " + util.Cyan("mysql") + " | " + util.Cyan("all") + " (默认)")
	fmt.Println()
	fmt.Println(util.Cyan("示例:"))
	fmt.Println("  " + util.Green("env.exe open") + "        " + util.Dim("启动全部"))
	fmt.Println("  " + util.Green("env.exe open redis") + "  " + util.Dim("只启动 redis"))
	fmt.Println("  " + util.Green("env.exe close") + "       " + util.Dim("停止全部"))
	fmt.Println("  " + util.Green("env.exe cmysql") + "      " + util.Dim("打开 mysql 客户端"))
	fmt.Println("  " + util.Green("env.exe credis ping") + " " + util.Dim("执行 redis-cli ping"))
	fmt.Println("  " + util.Green("env.exe status") + "      " + util.Dim("查看状态"))
	fmt.Println("  " + util.Green("env.exe help") + "        " + util.Dim("显示帮助"))
	fmt.Println(sep)
}
