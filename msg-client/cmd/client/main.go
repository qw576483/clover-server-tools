// Command msg-client 是 clover 网关命令行调试客户端。
//
// 用法：
//
//	msg-client                       # 用 config.yaml（首次运行自动生成默认配置）
//	msg-client -config my.yaml       # 指定配置文件
//	msg-client -addr 127.0.0.1:8003  # 覆盖默认地址
//
// 连接流程（原生客户端）：
//  1. 检测网络环境
//  2. UDP 探测（可选）
//  3. QUIC 连接尝试
//  4. TCP 连接尝试（回退）
//
// 启动后是交互式命令循环，支持 connect / ls / types / send / help / quit。
// 连上网关后，后台读协程会把服务端下发的每一帧打印为 `<<< [消息名] JSON`。
//
// proto 自动发现：CLI 启动时递归扫描 config.yaml 里 proto.business 指向的文件夹，
// 解析其中所有 const MsgXxx/EMsgXxx 消息号与 Request/Reply/Notify 结构体；
// 引擎内核消息（EMsgLogin/EMsgResumeSession 等）已内置硬编码，无需引擎源码目录。
// ls 列出全部消息，send 按名字/号自动绑定 k=v 参数组包。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"msg-client/internal/authclient"
	"msg-client/internal/client"
	"msg-client/internal/config"
	"msg-client/internal/logwriter"
	"msg-client/internal/proto"
	"msg-client/internal/util"
)

var idx *proto.Index
var protoBusiness []string

// authCli 账号服 HTTP 客户端。账号服是登录链路的必经依赖：
// login / signup 都经它走 HTTP（游戏服不收账号密码，也不提供注册入口）。
// 由 main 按 config.yaml 的 auth_addr 初始化，运行期只读。
var authCli *authclient.Client

// 维护 requestID → msgID 映射：回包（msgID=0）时按 requestID 反查它对应的请求消息名。
var reqMsg sync.Map

// 受锁保护的当前连接持有者：REPL 主循环（send 等）与后台 pump 协程并发读写，
// 断线自动重连时 pump 负责替换，主循环始终拿到的是最新连接。
type liveConn struct {
	mu sync.Mutex
	cl *client.Client
}

func (l *liveConn) get() *client.Client {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cl
}

// 替换当前连接并返回旧连接（供调用方 Close，避免 socket/协程泄漏）。
func (l *liveConn) set(c *client.Client) *client.Client {
	l.mu.Lock()
	old := l.cl
	l.cl = c
	l.mu.Unlock()
	return old
}

// 保存本会话的登录凭据与会话恢复凭据：
//   - account/password：断线重连 resume 失败（如服务器重启导致 session_token 失效）时回退重新登录
//   - playerID/sessionToken：来自 EPushPlayerFullSync，优先走 EMsgResumeSession 无缝恢复
type loginMemo struct {
	mu           sync.Mutex
	account      string // 登录账号（signup/login 时记录）
	password     string // 登录密码
	playerID     string // EPushPlayerFullSync 下发的 player_id
	sessionToken string // 对应的 session_token
	// authToken 账号服签发的 JWT（登录成功后缓存）。
	// 断线重连回退重登时优先复用它，避免把明文密码再发一遍；它也过期时才回账号服重取。
	authToken string
}

var memo loginMemo

func main() {
	util.EnableUTF8Console()
	util.EnableANSIColors()
	if _, err := logwriter.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "日志初始化失败: %v\n", err)
		os.Exit(1)
	}
	addr := flag.String("addr", "", "网关接入地址（覆盖 config.yaml 的 addr；支持 QUIC/TCP 自动切换）")
	configPath := flag.String("config", "config.yaml", "配置文件路径（默认 config.yaml，找不到则生成默认配置）")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", util.Red("配置错误:"), err)
		os.Exit(1)
	}

	idx, err = proto.LoadIndex(cfg.Proto.Business)
	if err != nil {
		fmt.Printf("%s proto 解析失败，ls/send 自动绑定不可用: %v\n", util.Yellow("警告:"), err)
	}
	protoBusiness = cfg.Proto.Business

	// 账号服（必填）：login / signup 都走 HTTP 换 token。
	// 地址缺失属配置错误——静默退化会让「账号服必须启动」形同虚设，故直接退出。
	if cfg.AuthAddr == "" {
		fmt.Fprintf(os.Stderr, "%s 未配置账号服地址（config.yaml 的 auth_addr）\n", util.Red("配置错误:"))
		os.Exit(1)
	}
	authCli = authclient.New(cfg.AuthAddr)
	logwriter.Printf("%s %s%s\n", util.Cyan("账号服地址:"), util.Green(cfg.AuthAddr),
		util.Dim("（login/signup 走 HTTP 换 token）"))

	defaultAddr := cfg.Addr
	tcpAddr := cfg.TCPAddr
	if *addr != "" {
		defaultAddr = *addr
		// 如果指定了地址，尝试从地址中提取主机，使用配置中的 TCP 端口
		host, _, err := net.SplitHostPort(*addr)
		if err == nil {
			// 从 TCP 地址中提取端口
			_, tcpPort, _ := net.SplitHostPort(tcpAddr)
			if tcpPort != "" {
				tcpAddr = net.JoinHostPort(host, tcpPort)
			}
		}
	}

	logwriter.Println(util.Amber("msg-client —— clover 网关命令行调试客户端"))
	logwriter.Printf("%s %s%s\n", util.Cyan("默认网关地址:"), util.Green(defaultAddr), util.Dim("（可在交互模式用 connect 切换）"))
	logwriter.Printf("%s %s%s\n", util.Cyan("TCP 回退地址:"), util.Green(tcpAddr), util.Dim("（QUIC 失败时使用）"))
	if idx != nil && idx.Len() > 0 {
		logwriter.Printf("%s %s %s %s %s\n",
			util.Green("已加载 proto:"), util.Cyan(fmt.Sprintf("%d", idx.Len())), "条消息 /",
			util.Cyan(fmt.Sprintf("%d", len(idx.Structs()))), "个结构体（ls 查看，send 自动绑定参数）")
	} else {
		logwriter.Println(util.Yellow("未加载到 proto（检查 config.yaml 的 proto.business 路径）"))
	}
	printHelp(defaultAddr) // 启动默认展示帮助页，再进入自动连接
	runREPL(defaultAddr, tcpAddr)
}

func runREPL(defaultAddr string, tcpAddr string) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	var lc liveConn
	addr := defaultAddr

	c, err := client.Dial(addr, tcpAddr)
	if err != nil {
		logwriter.Printf("%s %s: %v\n", util.Yellow("自动连接失败"), util.Cyan(addr), err)
	} else {
		lc.set(c)
		go runPump(&lc, addr, tcpAddr)
		// 打印实际生效的连接地址：QUIC 走 addr，回退 TCP 时走 tcpAddr。
		logwriter.Printf("%s %s [%s]\n", util.Green("已自动连接到"), util.Cyan(connectedAddr(addr, tcpAddr, c)), util.Cyan(c.TransportType().String()))
	}

	for {
		fmt.Print(util.Amber("clover> "))
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		cmd, args := fields[0], fields[1:]

		switch cmd {
		case "help", "?":
			printHelp(addr)
		case "connect":
			old := lc.set(nil)
			if old != nil {
				_ = old.Close()
			}
			if len(args) > 0 {
				addr = args[0]
				// 从配置中获取对应的 TCP 地址
				host, _, err := net.SplitHostPort(addr)
				if err == nil {
					// 从 TCP 地址中提取端口
					_, tcpPort, _ := net.SplitHostPort(tcpAddr)
					if tcpPort != "" {
						tcpAddr = net.JoinHostPort(host, tcpPort)
					}
				}
			}
			c, err := client.Dial(addr, tcpAddr)
			if err != nil {
				logwriter.Printf("%s %s: %v\n", util.Red("连接失败"), util.Cyan(addr), err)
				continue
			}
			old = lc.set(c)
			if old != nil {
				_ = old.Close()
			}
			// 打印实际生效的连接地址：QUIC 走 addr，回退 TCP 时走 tcpAddr。
			logwriter.Printf("%s %s [%s]\n", util.Green("已连接到"), util.Cyan(connectedAddr(addr, tcpAddr, c)), util.Cyan(c.TransportType().String()))
		case "disconnect":
			old := lc.set(nil)
			if old != nil {
				_ = old.Close()
				logwriter.Printf("%s %s\n", util.Green("已断开"), util.Cyan(addr))
			} else {
				fmt.Println(util.Yellow("未连接"))
			}
		case "login":
			doLogin(&lc, args)
		case "signup":
			doSignup(&lc, args)
		case "ls", "list":
			doLS()
		case "about":
			doAbout(args)
		case "types":
			doTypes(args)
		case "send":
			doSend(&lc, args)
		case "send_pos":
			doSendPos(&lc, args)
		case "quit", "exit":
			logwriter.Println(util.Dim("bye"))
			old := lc.set(nil)
			if old != nil {
				_ = old.Close()
			}
			return
		case "reload":
			doReload()
		default:
			fmt.Printf("%s %s%s\n", util.Red("未知命令:"), cmd, util.Dim("（输入 help 查看）"))
		}
	}
	old := lc.set(nil)
	if old != nil {
		_ = old.Close()
	}
}

// 返回实际生效的连接地址：QUIC 成功走 QUIC 地址，回退 TCP 时走 TCP 回退地址。
func connectedAddr(quicAddr, tcpAddr string, cl *client.Client) string {
	if cl != nil && cl.TransportType() == client.TransportTCP {
		return tcpAddr
	}
	return quicAddr
}

// 长驻后台泵循环：打印服务端消息，并在连接意外断开时自动重连 + 重放登录。
// 整个 CLI 生命周期只启动一个（启动时 go 一次）；connect/disconnect 替换 lc 的连接，
// 旧连接读协程因 Close 退出 → recv 关闭 → 内层 range 退出 → 回到外层取最新连接，无重复协程。
func runPump(lc *liveConn, addr string, tcpAddr string) {
	const maxAutoReconnect = 0 // 0 表示持续重试，直到用户 disconnect 或 quit
	reconnects := 0
	lastConnect := time.Now()
	for {
		c := lc.get()
		if c == nil {
			// 无连接（未 connect / 已 disconnect）：空转等待。
			time.Sleep(200 * time.Millisecond)
			continue
		}
		// recv channel 关闭即连接断开：无论读协程投没投 Err（EOF 正常关闭不投），
		// range 退出都要视为断开，否则会掉进"死连接空转"，send 持续报 aborted。
		closedByErr := false
		for inc := range c.Recv() {
			if inc.Err != nil {
				logwriter.Printf("\n%s %v\n", util.Red(">>> 连接断开:"), inc.Err)
				closedByErr = true
				break
			}
			printIncoming(c, inc)
		}
		// 连接已被替换（connect/disconnect/已有重连）→ 不重连，取最新连接，重连计数归零。
		if lc.get() != c {
			reconnects = 0
			continue
		}
		if !closedByErr {
			logwriter.Printf("\n%s 连接已断开（正常关闭）\n", util.Red(">>>"))
		}
		// 连接结实地存活过一段时间才断开 => 视为稳定，重连计数归零；
		// 否则（即连即断）累加，防止"连上即断"造成的无限重连刷屏。
		if time.Since(lastConnect) >= 5*time.Second {
			reconnects = 0
		}
		reconnects++
		if maxAutoReconnect > 0 && reconnects > maxAutoReconnect {
			logwriter.Printf("%s 连续自动重连 %d 次仍即连即断，停止重连（输入 connect 手动重连）\n", util.Red(">>>"), maxAutoReconnect)
			lc.set(nil)
			return
		}
		// 意外断开：自动重连（每 2s 重试，直到成功或用户退出）。
		logwriter.Printf("%s 正在自动重连 %s ...\n", util.Yellow(">>>"), util.Cyan(addr))
		var nc *client.Client
		var err error
		for {
			nc, err = client.Dial(addr, tcpAddr)
			if err == nil {
				break
			}
			logwriter.Printf("%s 重连失败: %v（2 秒后重试...）\n", util.Yellow(">>>"), err)
			time.Sleep(2 * time.Second)
			if lc.get() == nil {
				return // 用户 disconnect / quit
			}
		}
		old := lc.set(nc)
		if old != nil {
			_ = old.Close() // 关闭旧连接：连带停掉旧的 UDP 保活，避免持续无效令牌刷屏
		}
		lastConnect = time.Now()
		logwriter.Printf("%s 已自动重连 [%s]\n", util.Green(">>>"), util.Cyan(nc.TransportType().String()))
		resumeSession(nc) // 凭 session_token 恢复会话，无需重新登录
	}
}

// 打印一条服务端下发消息。
func printIncoming(c *client.Client, inc client.Incoming) {
	// 回包 msgID 恒为 0：按 requestID 反查它对应的请求，还原消息名。
	name := msgName(inc.MsgID)
	color := msgColor(inc.MsgID)
	if inc.MsgID == 0 && inc.RequestID != 0 {
		if v, ok := reqMsg.Load(inc.RequestID); ok {
			if origID, ok2 := v.(uint32); ok2 {
				name = msgName(origID) + " ·回包"
				color = msgColor(origID)
				// resume 会话失败（如服务器重启致 token 失效）：清空 session_token，
				// 让下一次自动重连回退到 EMsgLogin 重新登录，避免陷入 resume 死循环。
				if origID == client.EMsgResumeSession {
					var rr struct {
						Success bool   `json:"success"`
						Err     string `json:"err"`
					}
					if json.Unmarshal(inc.Body, &rr) == nil && !rr.Success {
						memo.mu.Lock()
						memo.sessionToken = ""
						memo.playerID = ""
						memo.mu.Unlock()
						errStr := ""
						if rr.Err != "" {
							errStr = "（" + rr.Err + "）"
						}
						logwriter.Printf("\n%s 会话已失效%s，自动回退重新登录\n", util.Yellow("警告:"), errStr)
					}
				}
			}
			reqMsg.Delete(inc.RequestID)
		}
	}
	// 网关在会话绑定 owner 成功时下发 EMsgUDPBindGrant 令牌帧：客户端须凭令牌
	// （而非自报账号）建立常驻裸 UDP 通道并上报端点，服务端不可靠推送（位置同步等）
	// 才会打到本客户端；未持有令牌就上报会被网关拒绝。
	if inc.MsgID == client.EMsgUDPBindGrant {
		token := string(inc.Body)
		if err := c.BindUDP(token); err != nil {
			logwriter.Printf("\n%s 建立常驻 UDP 通道: %v\n", util.Yellow("警告:"), err)
		} else {
			logwriter.Printf("\n%s 已凭令牌建立常驻 UDP 通道并上报端点，服务端不可靠推送将经该通道下发\n",
				util.Green(">>>"))
		}
	}
	// 登录/建角成功后引擎下发 EPushPlayerFullSync，携带 session_token 与 player_id：
	// 保存下来，断线重连后用 EMsgResumeSession 无缝恢复会话（而非重新注册/登录）。
	if inc.MsgID == client.EPushPlayerFullSync {
		var fs struct {
			PlayerID     string `json:"player_id"`
			SessionToken string `json:"session_token"`
		}
		if err := json.Unmarshal(inc.Body, &fs); err == nil && fs.SessionToken != "" {
			memo.mu.Lock()
			memo.playerID = fs.PlayerID
			memo.sessionToken = fs.SessionToken
			memo.mu.Unlock()
		}
	}
	logwriter.Printf("\n%s [%s]\n%s\n%s ", util.Green("<<<"), color(name), client.TryPretty(inc.Body), util.Amber("clover> "))
}

// loginBody 构造 EMsgLogin 请求体：先 HTTP 向账号服换取 JWT，只发 token。
//
// 游戏服不再接收账号密码——引擎只有「账号服校验」这一种登录模式。
// 换取成功后把 token 缓存进 memo，供断线重连时优先复用。
func loginBody(account, password string) ([]byte, error) {
	res, err := authCli.Login(account, password)
	if err != nil {
		return nil, err
	}
	if !res.Success {
		return nil, fmt.Errorf("账号服: %s", res.Err)
	}
	memo.mu.Lock()
	memo.authToken = res.Token
	memo.mu.Unlock()
	return json.Marshal(map[string]string{"token": res.Token})
}

// doLogin 登录。用法：login <account> <password>
//
// 固定两步：先从账号服 HTTP 换 token，再发 EMsgLogin{token} 给游戏服。
//
// 提供本命令而非要求手敲 send EMsgLogin：需要先自行取 token，
// 两步手写容易出错，且 token 需要缓存供重连复用。
func doLogin(lc *liveConn, args []string) {
	if len(args) < 2 {
		fmt.Println(util.Red("用法: login <account> <password>"))
		return
	}
	account, password := args[0], args[1]

	body, err := loginBody(account, password)
	if err != nil {
		fmt.Println(util.Red("登录失败: ") + err.Error())
		return
	}
	c := lc.get()
	if c == nil {
		fmt.Println(util.Yellow("未连接（先 connect）"))
		return
	}
	reqID, err := c.Send(client.EMsgLogin, body)
	if err != nil {
		fmt.Println(util.Red("发送失败: ") + err.Error())
		return
	}
	reqMsg.Store(reqID, client.EMsgLogin)

	memo.mu.Lock()
	memo.account, memo.password = account, password
	memo.mu.Unlock()
}

// doSignup 注册。用法：signup <account> <password>
//
// 注册是账号服的职责（游戏服不接收注册报文），全程走 HTTP。
// lc 参数保留是为了与其它命令处理器签名一致，本命令不经过长连接。
func doSignup(_ *liveConn, args []string) {
	if len(args) < 2 {
		fmt.Println(util.Red("用法: signup <account> <password>"))
		return
	}
	account, password := args[0], args[1]

	res, err := authCli.Signup(account, password)
	if err != nil {
		fmt.Println(util.Red("注册失败: ") + err.Error())
		return
	}
	if !res.Success {
		fmt.Println(util.Red("注册失败: ") + res.Err)
		return
	}
	fmt.Println(util.Green("注册成功（账号服）: ") + res.Owner +
		util.Dim("  接着执行 login 完成游戏侧登录"))
}

// 重连成功后恢复登录态：
//   - 有 session_token 时优先 EMsgResumeSession 无缝恢复（适用于网络闪断、服务端未重启）；
//   - 无有效 token（如服务器重启致 token 失效）时回退 EMsgLogin 重新登录。
func resumeSession(c *client.Client) {
	memo.mu.Lock()
	pid, tok, acc, pwd := memo.playerID, memo.sessionToken, memo.account, memo.password
	authTok := memo.authToken
	memo.mu.Unlock()

	if pid != "" && tok != "" {
		body := []byte(fmt.Sprintf(`{"player_id":%q,"session_token":%q}`, pid, tok))
		if reqID, err := c.Send(client.EMsgResumeSession, body); err == nil {
			reqMsg.Store(reqID, client.EMsgResumeSession)
		}
		return
	}
	if acc != "" {
		// 优先复用缓存的 JWT；它已过期时回账号服重新换取。
		var body []byte
		if authTok != "" {
			body, _ = json.Marshal(map[string]string{"token": authTok})
		} else if b, err := loginBody(acc, pwd); err == nil {
			body = b
		}
		if len(body) > 0 {
			if reqID, err := c.Send(client.EMsgLogin, body); err == nil {
				reqMsg.Store(reqID, client.EMsgLogin)
			}
		}
	}
}

// 把消息号转成可读名；优先查 proto 索引。
func msgName(id uint32) string {
	if idx != nil {
		if m := idx.MessageByID(id); m != nil {
			return fmt.Sprintf("%s(%d)", m.Name, id)
		}
	}
	switch id {
	case client.EMsgLogin:
		return "EMsgLogin(2)"
	case client.EMsgResumeSession:
		return "EMsgResumeSession(3)"
	case client.EMsgBindUDP:
		return "EMsgBindUDP(5)"
	case client.EMsgUDPBindGrant:
		return "EMsgUDPBindGrant(6)"
	case client.EPushPlayerFullSync:
		return "EPushPlayerFullSync(4001)"
	case client.EPushAlert:
		return "EPushAlert(4002)"
	case client.EMsgError:
		return "EMsgError(0xFFFFFFFF)"
	}
	if id > client.InternalMsgMax {
		return fmt.Sprintf("业务消息(%d)", id)
	}
	return fmt.Sprintf("引擎消息(%d)", id)
}

// 判断消息号是否属于引擎区间（<=InternalMsgMax 或为 EMsgError 特殊值）。
func isEngineMsg(id uint32) bool { return id <= client.InternalMsgMax || id == client.EMsgError }

// 返回引擎/业务消息的对应配色函数。
func msgColor(id uint32) func(string) string {
	if isEngineMsg(id) {
		return util.Cyan
	}
	return util.Magenta
}

// 根据名字前缀判定消息类型：msg / reply / push。
func msgType(name string) string {
	if strings.HasPrefix(name, "ReplyMsg") || strings.HasPrefix(name, "EReply") {
		return "reply"
	}
	if strings.HasPrefix(name, "Push") || strings.HasPrefix(name, "EPush") {
		return "push"
	}
	if strings.HasPrefix(name, "Msg") || strings.HasPrefix(name, "EMsg") {
		return "msg"
	}
	return "?"
}

// 去掉消息名中的已知前缀，提取业务关键词。
func stripPrefix(name string) string {
	for _, p := range []string{"ReplyMsg", "EReply", "EPush", "Reply", "Push", "EMsg", "Msg"} {
		if strings.HasPrefix(name, p) {
			return name[len(p):]
		}
	}
	return name
}

// 列出所有 C2S 消息（不含 Reply / Push），包含字段信息。
func doLS() {
	if idx == nil || idx.Len() == 0 {
		fmt.Println(util.Yellow("(无 proto 数据：检查 config.yaml 的 proto.business 路径)"))
		return
	}
	var msgs []*proto.Message
	for _, m := range idx.Messages() {
		typ := msgType(m.Name)
		if typ == "reply" || typ == "push" {
			continue
		}
		msgs = append(msgs, m)
	}
	printMsgList(msgs)
}

// 打印消息列表（about 用）。
func printMsgList(msgs []*proto.Message) {
	for _, m := range msgs {
		src := ""
		if m.Source != "" {
			src = " [" + m.Source + "]"
		}
		c := msgColor(m.ID)
		typ := msgType(m.Name)
		var fieldStr string
		switch typ {
		case "msg":
			fieldStr = fmt.Sprintf("req[%s]", proto.FieldList(m.Req))
		case "reply":
			fieldStr = fmt.Sprintf("reply[%s]", proto.FieldList(m.Reply))
		case "push":
			fieldStr = fmt.Sprintf("notify[%s]", proto.FieldList(m.Notify))
		default:
			fieldStr = "-"
		}
		fmt.Printf("%s %s %s %s\n",
			c(fmt.Sprintf("%-26s", m.Name+src)),
			util.Green(fmt.Sprintf("%-12d", m.ID)),
			c(fmt.Sprintf("%-6s", typ)),
			fieldStr)
	}
}

// 列出所有名称中包含指定关键词的消息。
func doAbout(args []string) {
	if idx == nil || idx.Len() == 0 {
		fmt.Println(util.Yellow("(无 proto 数据)"))
		return
	}
	if len(args) < 1 {
		fmt.Println(util.Dim("用法: about <消息名|关键词>"))
		return
	}
	keyword := strings.ToLower(stripPrefix(args[0]))
	if keyword == "" {
		fmt.Println(util.Yellow("无法提取关键词"))
		return
	}
	var matched []*proto.Message
	for _, m := range idx.Messages() {
		if strings.Contains(strings.ToLower(m.Name), keyword) {
			matched = append(matched, m)
		}
	}
	if len(matched) == 0 {
		fmt.Printf("%s 未找到含 \"%s\" 的消息\n", util.Yellow("提示:"), keyword)
		return
	}
	hdr := func(s string, w int) string { return util.Cyan(fmt.Sprintf("%-*s", w, s)) }
	fmt.Printf("%s 关键词: %s（%d 条）\n", util.Amber("about"), util.Cyan(keyword), len(matched))
	fmt.Printf("%s %s %s %s\n", hdr("NAME", 26), hdr("ID", 12), hdr("TYPE", 6), util.Cyan("FIELDS"))
	fmt.Println(util.Dim(strings.Repeat("-", 86)))
	printMsgList(matched)
}

// 列出所有解析到的结构体（按类别），或查指定结构体字段。
// 用法：types              列出全部（按类别分组）
//
//	types <Name>       查看指定结构体的字段
func doTypes(args []string) {
	if idx == nil {
		fmt.Println(util.Dim("(无 proto 数据)"))
		return
	}
	if len(args) > 0 {
		s := idx.StructByName(args[0])
		if s == nil {
			fmt.Printf("%s %s\n", util.Red("未找到结构体:"), util.Cyan(args[0]))
			return
		}
		fmt.Printf("%s %s %s\n", util.Cyan(s.Name), util.Dim("["+s.Kind+"]"), util.Dim("("+s.Source+")"))
		for _, f := range s.Fields {
			fmt.Printf("  %s %s %s\n", util.Cyan(fmt.Sprintf("%-16s", f.Name)), util.Green(fmt.Sprintf("%-12s", f.Type)), util.Dim("json:\""+f.JSON+"\""))
		}
		return
	}
	groups := map[string][]*proto.Struct{}
	for _, s := range idx.Structs() {
		groups[s.Kind] = append(groups[s.Kind], s)
	}
	for _, kind := range []string{"request", "reply", "notify", "other"} {
		ss := groups[kind]
		if len(ss) == 0 {
			continue
		}
		fmt.Printf("\n%s %s %s %s\n", util.Amber("=="), util.Cyan(kind), util.Green(fmt.Sprintf("(%d)", len(ss))), util.Amber("=="))
		for _, s := range ss {
			fmt.Printf("  %s %s\n", util.Cyan(fmt.Sprintf("%-28s", s.Name)), util.Dim(proto.FieldList(s.Fields)))
		}
	}
}

// 通用发送。支持两种方式：
//  1. send <MsgName|msgID> k=v k=v ...  —— 自动从 proto 索引绑定字段组 JSON 包
//  2. send <msgID> <raw-json>           —— 索引里找不到该消息时的兜底（直接发原始 JSON）
func doSend(lc *liveConn, args []string) {
	cl := lc.get()
	if cl == nil {
		fmt.Println(util.Yellow("尚未连接，请先 connect"))
		return
	}
	if len(args) < 1 {
		fmt.Println(util.Dim("用法: send <MsgName|msgID> [k=v ...]   或   send <msgID> <raw-json>"))
		return
	}
	target := args[0]

	if idx != nil {
		if m := idx.Lookup(target); m != nil {
			body := proto.BuildBody(m.Req, args[1:])
			// 不再从 body 里抓 account/password 记入 memo：登录请求体只剩 token
			// （账号密码只发给账号服），凭据由 login 命令负责记录。
			reqID, err := cl.Send(m.ID, body)
			if err != nil {
				fmt.Printf("%s %v\n", util.Red("发送失败:"), err)
				return
			}
			reqMsg.Store(reqID, m.ID) // 记录 requestID→msgID，供回包反查消息名
			fmt.Printf("%s %s (id=%d): %s\n", util.Green(">>> 已发送"), util.Cyan(m.Name), m.ID, string(body))
			return
		}
	}

	// 兜底：索引里没有该消息，按 `send <msgID> <json>` 原样发送。
	id := parseUint32(target)
	if id <= client.InternalMsgMax {
		fmt.Printf("%s 业务消息号应 >= %d（引擎内部消息号被保留）\n", util.Yellow("提示:"), client.InternalMsgMax+1)
	}
	if len(args) < 2 {
		fmt.Println(util.Yellow("索引中无此消息，兜底用法: send <msgID> <raw-json>"))
		return
	}
	body := []byte(strings.Join(args[1:], " "))
	reqID, err := cl.Send(id, body)
	if err != nil {
		fmt.Printf("%s %v\n", util.Red("发送失败:"), err)
		return
	}
	reqMsg.Store(reqID, id) // 兜底路径同样记录，回包才能反查
	fmt.Printf("%s msgID=%d: %s\n", util.Green(">>> 已发送"), id, body)
}

// 发送不可靠位置数据（走 QUIC Datagram 或裸 UDP，可丢包、无回包）。
// 支持：send_pos <MsgName|msgID> [k=v ...]   或   send_pos <msgID> <raw-json>
func doSendPos(lc *liveConn, args []string) {
	cl := lc.get()
	if cl == nil {
		fmt.Println(util.Yellow("尚未连接，请先 connect"))
		return
	}
	if len(args) < 1 {
		fmt.Println(util.Dim("用法: send_pos <MsgName|msgID> [k=v ...]   或   send_pos <msgID> <raw-json>"))
		return
	}
	target := args[0]

	if idx != nil {
		if m := idx.Lookup(target); m != nil {
			body := proto.BuildBody(m.Req, args[1:])
			if err := cl.SendUnreliable(m.ID, body); err != nil {
				fmt.Printf("%s %v\n", util.Red("发送失败(不可靠):"), err)
				return
			}
			ch := "QUIC Datagram"
			if cl.TransportType() == client.TransportTCP {
				ch = "裸 UDP(0x55)"
			}
			fmt.Printf("%s %s (id=%d) [%s]: %s\n", util.Green(">>> 已发送(不可靠)"), util.Cyan(m.Name), m.ID, ch, string(body))
			return
		}
	}

	// 兜底：索引里没有该消息，按 `send_pos <msgID> <json>` 原样发送。
	id := parseUint32(target)
	if len(args) < 2 {
		fmt.Println(util.Yellow("索引中无此消息，兜底用法: send_pos <msgID> <raw-json>"))
		return
	}
	body := []byte(strings.Join(args[1:], " "))
	if err := cl.SendUnreliable(id, body); err != nil {
		fmt.Printf("%s %v\n", util.Red("发送失败(不可靠):"), err)
		return
	}
	fmt.Printf("%s msgID=%d: %s\n", util.Green(">>> 已发送(不可靠)"), id, body)
}

func doReload() {
	newIdx, err := proto.LoadIndex(protoBusiness)
	if err != nil {
		fmt.Printf("%s proto 重载失败: %v\n", util.Red("错误:"), err)
		return
	}
	idx = newIdx
	fmt.Printf("%s %s %s %s %s\n",
		util.Green("proto 已重载:"), util.Cyan(fmt.Sprintf("%d", idx.Len())), "条消息 /",
		util.Cyan(fmt.Sprintf("%d", len(idx.Structs()))), "个结构体")
}

func printHelp(addr string) {
	sep := util.Amber("============================================================")
	fmt.Println(sep)
	fmt.Println(util.Amber("  msg-client —— 网关命令行调试客户端"))
	fmt.Println(sep)
	fmt.Println()
	fmt.Println(util.Dim("消息号与字段均来自启动扫描的 proto 索引，引擎改 opcode 无需重编 CLI"))

	fmt.Println()
	fmt.Println(util.Cyan("命令:"))
	fmt.Printf("  %-28s %s\n", util.Cyan("help"), util.Dim("显示本帮助"))
	fmt.Printf("  %-28s %s\n", util.Cyan("connect [addr]"), util.Dim(fmt.Sprintf("连接网关（缺省用 %s）", addr)))
	fmt.Printf("  %-28s %s\n", util.Cyan("disconnect"), util.Dim("断开当前连接"))
	fmt.Printf("  %-28s %s\n", util.Cyan("ls / list"), util.Dim("列出所有已加载的协议消息（含字段信息）"))
	fmt.Printf("  %-28s %s\n", util.Cyan("about <Name|关键词>"), util.Dim("列出所有名称含关键词的消息"))
	fmt.Printf("  %-28s %s\n", util.Cyan("types [Name]"), util.Dim("列出结构体，或查指定结构体字段"))
	fmt.Printf("  %-28s %s\n", util.Cyan("send <Name|id> [k=v ...]"), util.Dim("按消息名/号自动绑定字段发送"))
	fmt.Printf("  %-28s %s\n", util.Cyan("send <id> <raw-json>"), util.Dim("兜底：直接发送原始 JSON"))
	fmt.Printf("  %-28s %s\n", util.Cyan("send_pos <Name|id> [k=v ...]"), util.Dim("不可靠发送（QUIC Datagram / 裸UDP 0x55），位置同步用"))
	fmt.Printf("  %-28s %s\n", util.Cyan("quit / exit"), util.Dim("断开并退出"))
	fmt.Printf("  %-28s %s\n", util.Cyan("reload"), util.Dim("重新扫描 proto 源文件，热加载消息定义"))

	fmt.Println()
	fmt.Println(util.Cyan("示例:"))
	fmt.Println("  " + util.Green("connect "+addr))
	fmt.Println("  " + util.Green("ls"))
	fmt.Println("  " + util.Green("login clover 123123") + util.Dim("  一键登录（先向账号服换 token，再发 EMsgLogin）"))
	fmt.Println("  " + util.Green("signup clover 123123") + util.Dim("  注册（走账号服 HTTP）"))
	fmt.Println("  " + util.Green("send EMsgLogin token=tok-xyz") + util.Dim("  直接发登录报文（body 只有 token）"))
	fmt.Println("  " + util.Green("about Login"))
	fmt.Println("  " + util.Green("types EMsgLogin"))

	fmt.Println()
	fmt.Println(util.Dim("连上后自动打印服务端下发帧：<<< [EMsgLogin] {...}  引擎=Cyan  业务=Magenta"))
	fmt.Println(util.Dim("EMsgLogin body 由引擎固定；业务消息(>=10001) body 由 def 包定义，自动识别。"))
	fmt.Println(sep)
}

func parseUint32(s string) uint32 {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}
