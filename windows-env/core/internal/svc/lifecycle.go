package svc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qw576483/clover-server-tools/windows-env/core/internal/util"
)

// 执行两阶段: 先全部环境检查, 再统一后台启动。
func CheckAndStart(target string) {
	list := Pick(target)
	if list == nil {
		return
	}

	sep := util.Amber("============================================================")
	fmt.Println(sep)
	fmt.Println(util.Amber("  Clover 开发环境 - 启动"))
	fmt.Println("  " + util.Dim("根目录: ") + root)
	fmt.Println(sep)
	fmt.Println()

	// 阶段 1/2: 环境检查 (全部通过后才启动)
	fmt.Println(util.Cyan("[阶段 1/2]") + " 环境检查 ...")
	missing := false
	for _, s := range list {
		p := filepath.Join(root, s.ExeRel)
		if fileExists(p) {
			fmt.Printf("  %s %s\n", util.Green("[OK]"), s.ExeRel)
		} else {
			fmt.Printf("  %s %s 不存在\n", util.Red("[缺失]"), s.ExeRel)
			missing = true
		}
	}
	if missing {
		fmt.Println()
		printDownloadTips()
		os.Exit(1)
	}
	fmt.Println("  " + util.Green("[通过]") + " 所需组件均已就绪。")
	fmt.Println()

	// 阶段 2/2: 启动
	fmt.Println(util.Cyan("[阶段 2/2]") + " 启动服务 ...")
	for _, s := range list {
		if s.Name == "mysql" {
			startMySQL(s)
			continue
		}
		startGeneric(s)
	}

	fmt.Println()
	fmt.Println(util.Amber("============================================================"))
	fmt.Println("  " + util.Green("启动完成!") + " 使用 " + util.Cyan("env.exe info") + " 查看状态。")
	fmt.Println("    etcd  -> http://127.0.0.1:2379")
	fmt.Println("    nats  -> nats://127.0.0.1:4222")
	fmt.Println("    redis -> 127.0.0.1:6379")
	if MySQLPass == "" {
		fmt.Println("    mysql -> 127.0.0.1:3306  (root, 无密码)")
	} else {
		fmt.Printf("    mysql -> 127.0.0.1:3306  (root / %s)\n", MySQLPass)
	}
	fmt.Println("  " + util.Dim("日志目录: core\\logs\\   停止: env.exe stop"))
	fmt.Println(util.Amber("============================================================"))
}

func startGeneric(s Service) {
	if portOpen(s.Port) {
		fmt.Printf("  %s %s 端口 %d 已在监听, 视为已运行。\n", util.Yellow("[跳过]"), s.Name, s.Port)
		return
	}
	pid, err := spawn(s.Name, filepath.Join(root, s.ExeRel), s.Args, filepath.Join(root, s.WorkRel))
	if err != nil {
		fmt.Printf("  %s 启动 %s 失败: %v\n", util.Red("[错误]"), s.Name, err)
		return
	}
	fmt.Printf("  %s %-5s (pid %d, 端口 %d) 日志: core\\logs\\%s.log\n", util.Green("[启动]"), s.Name, pid, s.Port, s.Name)
}

// 停止一个或多个服务 (逆序, mysql 先优雅关闭)。
func Stop(target string) {
	list := Pick(target)
	if list == nil {
		return
	}

	fmt.Println(util.Amber("============================================================"))
	fmt.Println(util.Amber("  Clover 开发环境 - 停止"))
	fmt.Println(util.Amber("============================================================"))
	fmt.Println()

	for i := len(list) - 1; i >= 0; i-- {
		s := list[i]
		if s.Name == "mysql" {
			stopMySQL()
			continue
		}
		stopGeneric(s)
	}

	fmt.Println()
	fmt.Println(util.Cyan("[检查]") + " 端口释放情况:")
	for _, s := range list {
		if portOpen(s.Port) {
			fmt.Printf("  %s 端口 %d (%s) 仍被占用。\n", util.Yellow("[警告]"), s.Port, s.Name)
		} else {
			fmt.Printf("  %s 端口 %d (%s) 已释放。\n", util.Green("[OK]"), s.Port, s.Name)
		}
	}
	fmt.Println()
	fmt.Println(util.Green("停止完成。"))
}

func stopGeneric(s Service) {
	pid := readPid(s.Name)
	killed := false
	if pid > 0 && pidAlive(pid) {
		if killByPid(pid) {
			fmt.Printf("  %s %-5s (pid %d) 已结束。\n", util.Green("[停止]"), s.Name, pid)
			killed = true
		}
	}
	image := filepath.Base(s.ExeRel)
	if killByImage(image) && !killed {
		fmt.Printf("  %s %-5s (%s) 已结束。\n", util.Green("[停止]"), s.Name, image)
		killed = true
	}
	if !killed {
		fmt.Printf("  %s %-5s 未在运行。\n", util.Dim("[跳过]"), s.Name)
	}
	removePid(s.Name)
}

// 打印各服务运行状态。
func Info() {
	sep := util.Amber("============================================================")
	fmt.Println(sep)
	fmt.Println(util.Amber("  Clover 开发环境 - 状态"))
	fmt.Println("  " + util.Dim("根目录: ") + root)
	fmt.Println(sep)
	hdr := func(s string, w int) string { return util.Cyan(fmt.Sprintf("%-*s", w, s)) }
	fmt.Printf("  %s %s %s %s %s\n", hdr("服务", 6), hdr("状态", 8), hdr("端口", 6), hdr("PID", 8), util.Cyan("组件"))
	fmt.Println("  " + util.Dim("----------------------------------------------------------"))
	for _, s := range Services() {
		stateRaw := "已停止"
		if portOpen(s.Port) {
			stateRaw = "运行中"
		}
		stateCell := util.Dim(fmt.Sprintf("%-8s", stateRaw))
		if portOpen(s.Port) {
			stateCell = util.Green(fmt.Sprintf("%-8s", stateRaw))
		}
		pidStr := "-"
		if pid := readPid(s.Name); pid > 0 && pidAlive(pid) {
			pidStr = strconv.Itoa(pid)
		} else if live := pidByImage(filepath.Base(s.ExeRel)); live > 0 {
			pidStr = strconv.Itoa(live)
		}
		existRaw := "缺失"
		if fileExists(filepath.Join(root, s.ExeRel)) {
			existRaw = "已就绪"
		}
		existCell := util.Red(existRaw)
		if fileExists(filepath.Join(root, s.ExeRel)) {
			existCell = util.Green(existRaw)
		}
		fmt.Printf("  %s %s %-6d %-8s %s\n",
			util.Cyan(fmt.Sprintf("%-6s", s.Name)),
			stateCell, s.Port,
			util.Cyan(fmt.Sprintf("%-8s", pidStr)),
			existCell)
	}
	fmt.Println(sep)
}

// ---- MySQL 专用 ----

func startMySQL(s Service) {
	if portOpen(s.Port) {
		fmt.Printf("  [跳过] mysql 端口 %d 已在监听, 视为已运行。\n", s.Port)
		return
	}

	mysqlRoot := filepath.Join(root, "mysql")
	binDir := filepath.Join(mysqlRoot, "bin")
	dataDir := filepath.Join(mysqlRoot, "data")
	iniPath := filepath.Join(mysqlRoot, "my.ini")
	mysqld := filepath.Join(binDir, "mysqld.exe")

	// 动态生成 mysql/my.ini (mysql/ 是用户下载的发行版,不随 git 走), 使用绝对路径,
	// 避免基于 CWD 的相对路径解析差异和权限问题。
	iniContent := "[mysqld]\r\n" +
		"basedir=" + mysqlRoot + "\r\n" +
		"datadir=" + dataDir + "\r\n" +
		"port=3306\r\n" +
		"max_connections=200\r\n" +
		"character-set-server=utf8mb4\r\n" +
		"default-storage-engine=INNODB\r\n" +
		"sql_mode=NO_ENGINE_SUBSTITUTION,STRICT_TRANS_TABLES\r\n" +
		"log-error=" + filepath.Join(exeDir, "logs", "mysql.log") + "\r\n" +
		"[client]\r\n" +
		"port=3306\r\n" +
		"default-character-set=utf8mb4\r\n"
	if err := os.WriteFile(iniPath, []byte(iniContent), 0o644); err != nil {
		fmt.Println("  " + util.Red("[错误]") + " 写 mysql/my.ini 失败:", err)
		return
	}

	// 数据目录是否完好: MySQL 8.0+ 用 mysql.ibd (数据字典) 记录 mysql 系统库,
	// 旧版才把每个表拆成 mysql/<table>.ibd (含 plugin.ibd)。以 mysql.ibd 是否存在
	// 作为"已初始化"的判据, 否则每次启动都会误判不完整而反复重建。
	needInit := false
	if !fileExists(dataDir) {
		needInit = true
	} else if !fileExists(filepath.Join(dataDir, "mysql.ibd")) {
		needInit = true
	}
	if needInit {
		if fileExists(dataDir) {
			bak := filepath.Join(mysqlRoot, "data.bak-"+time.Now().Format("20060102-150405"))
			fmt.Printf("  %s 检测到数据目录不完整, 备份到 %s 后重新初始化...\n", util.Yellow("[*]"), filepath.Base(bak))
			_ = os.Rename(dataDir, bak)
		}
		fmt.Println("  " + util.Yellow("[*]") + " 首次启动: 初始化 MySQL 数据目录 (可能需要数十秒) ...")
		init := exec.Command(mysqld, "--defaults-file="+iniPath, "--initialize-insecure", "--console")
		init.Dir = mysqlRoot // 初始化阶段 CWD 设为 mysql/，确保 mysql/share/ 等路径可被找到
		out, err := init.CombinedOutput()
		if err != nil {
			fmt.Println("  " + util.Red("[错误]") + " MySQL 初始化失败:", err)
			if len(out) > 0 {
				fmt.Println(string(out))
			}
			return
		}
		fmt.Println("  " + util.Green("[OK]") + " 数据目录初始化完成。")
	}

	// 确保 core/logs, core/run 存在 (日志与 pid 落点)。
	if err := os.MkdirAll(filepath.Join(exeDir, "logs"), 0o755); err != nil {
		fmt.Println("  " + util.Red("[错误]") + " 创建日志目录失败:", err)
		return
	}
	if err := os.MkdirAll(filepath.Join(exeDir, "run"), 0o755); err != nil {
		fmt.Println("  " + util.Red("[错误]") + " 创建 run 目录失败:", err)
		return
	}

	// 写 mysql_start.bat 到 core/ (随版本走), cd 到 mysql/ 目录保证 my.ini 相对路径正确。
	// 不重定向日志, 由 my.ini 的 log-error 写入 core/logs/mysql.log。
	batPath := filepath.Join(exeDir, "mysql_start.bat")
	bat := "@echo off\r\n" +
		"cd /D \"%~dp0..\\mysql\"\r\n" +
		"bin\\mysqld --defaults-file=my.ini --standalone\r\n"
	if err := os.WriteFile(batPath, []byte(bat), 0o644); err != nil {
		fmt.Println("  " + util.Red("[错误]") + " 写 mysql_start.bat 失败:", err)
		return
	}

	// 写 mysql_start.vbs 到 core/: 通过 WScript.Shell 以隐藏窗口 (style 0) 启动 bat,
	// 使 MySQL 真正脱离 env.exe 的进程树, 即使关闭 env.exe 也能存活。
	vbsPath := filepath.Join(exeDir, "mysql_start.vbs")
	// 内容里**不写绝对路径**：用 WScript.ScriptFullName 定位同目录的 mysql_start.bat
	// （与 mysql_start.bat 用 %~dp0 同一思路）。原来把 batPath 拼进字符串 ⇒
	// 生成物带着本机绝对路径入库，换机器 / 搬目录后那份 vbs 就失效了。
	vbs := "Set ws = CreateObject(\"WScript.Shell\")\r\n" +
		"here = CreateObject(\"Scripting.FileSystemObject\").GetParentFolderName(WScript.ScriptFullName)\r\n" +
		"ws.Run \"cmd /c \" & Chr(34) & here & \"\\mysql_start.bat\" & Chr(34), 0, false\r\n"
	if err := os.WriteFile(vbsPath, []byte(vbs), 0o644); err != nil {
		fmt.Println("  " + util.Red("[错误]") + " 写 mysql_start.vbs 失败:", err)
		return
	}

	// 通过 wscript 以隐藏窗口启动, env.exe 立即返回。
	launch := exec.Command("wscript", vbsPath)
	if err := launch.Run(); err != nil {
		fmt.Println("  " + util.Red("[错误]") + " 启动 mysql 失败:", err)
		return
	}
	fmt.Println("  " + util.Green("[启动]") + " mysql (端口 3306) 后台隐藏窗口: mysql_start.vbs, 日志: core\\logs\\mysql.log")

	// 记录 PID: 通过 netstat 找占用 3306 的 PID (MySQL 8.0+ Windows 会起两个进程,
	// 监听端口的是实际服务进程, 用 tasklist 第一个会误判)。
	time.Sleep(1500 * time.Millisecond)
	if pid := pidByPort(s.Port); pid > 0 {
		writePid("mysql", pid)
	}

	fmt.Println("  " + util.Yellow("[提示]") + " MySQL 已在后台启动, 稍后用 env.exe status 查看就绪状态。")
}

func stopMySQL() {
	binDir := filepath.Join(root, "mysql", "bin")
	mysqladmin := filepath.Join(binDir, "mysqladmin.exe")

	graceful := false
	if fileExists(mysqladmin) {
		args := append(MySQLAuthArgs(), "shutdown")
		if exec.Command(mysqladmin, args...).Run() == nil {
			graceful = true
		}
	}
	if graceful {
		fmt.Println("  " + util.Green("[停止]") + " mysql 已关闭 (数据落盘)。")
	}

	pid := readPid("mysql")
	if pid > 0 && pidAlive(pid) {
		killByPid(pid)
	}
	// 兜底: 按进程名结束所有 mysqld (MySQL Windows 版可能启动双进程, 只杀一个会漏)。
	if killByImage("mysqld.exe") && !graceful {
		fmt.Println("  " + util.Green("[停止]") + " mysql (mysqld.exe) 已结束。")
	} else if !graceful {
		fmt.Println("  [跳过] mysql 未在运行。")
	}
	removePid("mysql")
}

// 通过 netstat -ano 查找监听指定端口的 PID。
func pidByPort(port int) int {
	out, err := exec.Command("netstat", "-ano").Output()
	if err != nil {
		return 0
	}
	target := ":" + strconv.Itoa(port)
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if !strings.Contains(line, target) || !strings.Contains(line, "LISTENING") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 5 {
			if p, e := strconv.Atoi(fields[len(fields)-1]); e == nil {
				return p
			}
		}
	}
	return 0
}

func printDownloadTips() {
	fmt.Println(util.Red("[失败]") + " 环境不完整, 请下载并解压组件到对应目录:")
	fmt.Println()
	fmt.Println(util.Amber("----- 下载地址 -----"))
	fmt.Println("  " + util.Cyan("mysql") + ": https://dev.mysql.com/downloads/mysql/")
	fmt.Println("         " + util.Dim("适用 Windows x86 64-bit 的 ZIP Archive"))
	fmt.Println("  " + util.Cyan("etcd") + " : https://github.com/etcd-io/etcd/releases")
	fmt.Println("         " + util.Dim("文件名 etcd-vX.X.X-windows-amd64.zip"))
	fmt.Println("  " + util.Cyan("nats") + " : https://github.com/nats-io/nats-server/releases")
	fmt.Println("         " + util.Dim("文件名 nats-server-vX.X.X-windows-amd64.zip"))
	fmt.Println("  " + util.Cyan("redis") + ": https://github.com/redis-windows/redis-windows/releases")
	fmt.Println("         " + util.Dim("文件名 Redis-X.X.X-Windows-X64-msys2.zip"))
	fmt.Println()
	fmt.Println("  " + util.Dim("下载后分别重命名为 mysql / etcd / nats / redis 放在 env.exe 同级目录。"))
}
