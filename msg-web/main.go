package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clover-server-tools/msg-web/internal/logwriter"
	"clover-server-tools/msg-web/internal/proto"

	"gopkg.in/yaml.v3"
)

// 配置文件结构。
type AppConfig struct {
	Addr    string `yaml:"addr"`
	WebPort int    `yaml:"web_port"`
	TLSCert string `yaml:"tls_cert"` // TLS 证书路径（PEM）；与 TLSKey 同为非空时启用 HTTPS 服务
	TLSKey  string `yaml:"tls_key"`  // TLS 私钥路径（PEM）
	// AuthAddr 账号服 HTTP 地址（如 http://127.0.0.1:8051）。**必填**：
	// 引擎只有一种登录模式（账号服校验），页面登录 / 注册都必须经本工具代理到它。
	AuthAddr string      `yaml:"auth_addr"`
	Proto    ProtoConfig `yaml:"proto"`
}

// proto 扫描路径（引擎消息已内置，只需配置业务 def 路径）。
type ProtoConfig struct {
	Business []string `yaml:"business"`
}

var (
	cfg     AppConfig
	idx     *proto.Index
	bizDirs []string
	// gatewayScheme 启动时探测到的网关 WS 端口协议（https / http）；空=探测不到（网关未启动）。
	// 前端据此派生 ws/wss，而不是只看页面协议——两者不一致时浏览器两种连接都会失败。
	gatewayScheme string
	// wtHash 启动时取到的 WebTransport 证书哈希（hex）；空=网关未启用 WT（浏览器 WT 必然失败）。
	wtHash string
)

// errTLSNotConfigured 表示「用户本就没配页面证书」（配置里两项留空），属正常降级，不告警。
var errTLSNotConfigured = errors.New("未配置页面证书")

func main() {
	enableUTF8Console() // 必须在任何输出之前：否则中文诊断在 GBK 控制台下是乱码
	if err := setupLogAndPID(); err != nil {
		log.Printf("日志初始化失败: %v", err)
	}

	cfgPath := flag.String("config", "config.yaml", "配置文件路径（默认 config.yaml）")
	forceHTTP := flag.Bool("http", false, "强制以明文 HTTP 提供页面（忽略 tls_cert / tls_key）")
	flag.Parse()
	loadConfig(*cfgPath)

	// 账号服是登录链路的必经依赖：地址缺失属配置错误。
	// 「页面直接发账号密码给游戏服」的降级路径已被引擎废弃，故启动即失败，不静默继续。
	if cfg.AuthAddr == "" {
		fatal("配置错误: 未配置账号服地址（config.yaml 的 auth_addr）")
	}

	cfgDir := filepath.Dir(*cfgPath)
	for _, d := range cfg.Proto.Business {
		bizDirs = append(bizDirs, resolvePath(cfgDir, d))
	}
	// 证书路径也相对配置文件解析，避免受启动目录影响而读不到（从而退回首 http）。
	cfg.TLSCert = resolvePath(cfgDir, cfg.TLSCert)
	cfg.TLSKey = resolvePath(cfgDir, cfg.TLSKey)

	// 页面证书预检：文件缺失 / 损坏 / 不成对时退化为明文 HTTP 继续启动。
	// 原实现把路径直接交给 ListenAndServeTLS，失败即 log.Fatalf 退出——双击 exe 时窗口
	// 一闪而过，用户只看到「闪退」，真正原因只留在 logs/ 里（certs/ 被 .gitignore 排除，
	// 新克隆 / 清理过证书的机器必踩）。这里改为「能起得来 + 说清原因 + 给出补救路径」。
	if *forceHTTP {
		log.Println("已按 -http 强制使用明文 HTTP：忽略 tls_cert / tls_key")
		cfg.TLSCert, cfg.TLSKey = "", ""
	} else if err := tlsPrecheck(cfg.TLSCert, cfg.TLSKey); err != nil {
		if !errors.Is(err, errTLSNotConfigured) {
			for _, line := range tlsFallbackHint(cfg.TLSCert, err) {
				log.Println(line)
			}
		}
		cfg.TLSCert, cfg.TLSKey = "", ""
	}

	rebuildIndex()

	http.Handle("/", http.FileServer(http.Dir(webDir(cfgDir))))
	http.HandleFunc("/api/messages", handleMessages)
	http.HandleFunc("/api/config", handleConfig)
	http.HandleFunc("/api/reload", handleReload)
	// 账号服代理（注册 / 登录）。前缀式注册：/api/auth/login、/api/auth/signup。
	http.HandleFunc("/api/auth/", handleAuthProxy)

	addr := fmt.Sprintf(":%d", cfg.WebPort)
	// 页面协议：本工具自身是否以 HTTPS 提供页面（证书可用则 https）。
	pageScheme := "http"
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		pageScheme = "https"
	}
	// 网关协议：探测 WS 端口实际是加密还是明文。**不能只看页面协议**——
	// demo 默认给网关留空 tls_cert（保证 Unity 裸 TCP 客户端可用），此时页面若是 https，
	// 派生出的 wss:// 会打到一个明文端口，浏览器只会报「握手失败」；反过来 https 页面里的
	// ws:// 又会被当混合内容拦截。探不到（网关未启动）时按页面协议假设，连接时自会暴露。
	gatewayScheme, wtHash = probeGateway(cfg.Addr)
	if gatewayScheme == "" {
		gatewayScheme = pageScheme
	}
	wsScheme := map[string]string{"https": "wss", "http": "ws"}[gatewayScheme]
	wsURL := fmt.Sprintf("%s://%s/ws", wsScheme, cfg.Addr)
	server := &http.Server{Addr: addr}
	// 先显式绑定端口，成功后再宣告「已启动」：端口被占用是最常见的启动失败，
	// 若直接交给 ListenAndServe*，只会留一行 server error 就退出——双击启动时表现为
	// 「窗口一闪」，真正原因（谁占了端口）一个字都看不到。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fatalListenFail(addr, err)
	}
	log.Printf("msg-web 已启动 %s://localhost%s（页面=%s，网关=%s，WS 直连 %s）",
		pageScheme, addr, pageScheme, gatewayScheme, wsURL)
	if wtHash != "" {
		log.Printf("WebTransport 证书哈希: %s…（经 /wt-cert-hash 动态下发，运行期自动轮换）", wtHash[:16])
	} else if gatewayScheme == "http" {
		// 明文网关（demo 默认 tls_cert 留空，为保 Unity 裸 TCP）是 WT 起不来的**最常见**原因，
		// 且此时 enable_wt / wt_pin 配了也不生效（引擎把 WT 准备工作整体放在 tls_cert 非空条件下）。
		// 直接点明原因，避免用户去改 enable_wt 白折腾。
		log.Println("WebTransport 不可用：网关为明文（gateway.tls_cert 未配置，引擎仅在配 TLS 时才启动 WT）—— 属预期，浏览器走 ws://")
	} else {
		// 网关是 https 却没下发哈希：多为 enable_wt: false / wt_pin: false / WT 启动失败。
		log.Println("WebTransport 不可用：网关未下发证书哈希（需网关 gateway.enable_wt: true，且 WS 端口能取到 /wt-cert-hash）")
	}
	if pageScheme != gatewayScheme {
		log.Printf("⚠ 协议不匹配：页面为 %s，网关 WS 为 %s —— 浏览器两种连接都会失败。", pageScheme, gatewayScheme)
		if pageScheme == "https" {
			log.Println("  二选一：① 给网关配 gateway.tls_cert / tls_key（wss 与 WebTransport 均可用）；")
			log.Println("          ② 以 web.exe -http 重启（页面 http + ws:// 可用，WebTransport 不可用）。")
		} else {
			log.Println("  二选一：① 给 msg-web 配 certs（https 页面）；② 关掉网关 TLS。")
		}
	}
	var serveErr error
	if pageScheme == "https" {
		serveErr = server.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
	} else {
		serveErr = server.Serve(ln)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		fatal("server error: %v", serveErr)
	}
}

// fatal 打印致命错误后延迟退出：双击 exe 时控制台是本进程临时新建的，进程一退窗口就消失，
// 用户只看到「闪退」。此时多停一步等用户看清原因；从已有终端启动则窗口本就不会关，直接退出，
// 也就不会在 CI / 重定向 stdin 的场景里卡住。
func fatal(format string, args ...any) {
	log.Printf(format, args...)
	fatalWait()
}

// fatalListenFail 监听失败（绝大多数是端口被占用）的诊断：讲清原因，并给出可执行的处理命令。
func fatalListenFail(addr string, err error) {
	log.Printf("⚠ 启动失败：无法监听 %s（Web 端口没起来）", addr)
	log.Printf("  底层错误: %v", err)
	if isAddrInUse(err) {
		port := addr[strings.LastIndex(addr, ":")+1:]
		log.Printf("  最常见原因：端口已被占用——上一次的 web.exe 还在跑，或双击启动了两次。")
		log.Printf("  处理 Windows：netstat -ano | findstr :%s  然后  taskkill /PID <PID> /F", port)
		log.Printf("  处理 macOS/Linux：lsof -i :%s", port)
		log.Printf("  或改 config.yaml 的 web_port 换一个端口后重启。")
	}
	fatalWait()
}

// fatalWait 退出前的最后一步：仅当控制台为本进程独享（双击 / 新开窗口启动）时等待按键，
// 其余情况（已有终端、无控制台、stdin 被重定向）直接退出。
func fatalWait() {
	if isOwnConsole() {
		fmt.Fprint(os.Stderr, "\n按 Enter 键退出…")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(1)
}

// isAddrInUse 判断监听失败是否因端口被占用。
//
// 不写成 errors.Is(err, syscall.EADDRINUSE)：该常量的数值在 Windows（WSAEADDRINUSE=10048）
// 与 Unix（98/48）并不一致，判等容易失效，故按 Go 暴露的错误文案匹配（两平台各一条）。
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "address already in use") || // Unix
		strings.Contains(msg, "Only one usage of each socket address") // Windows
}

func setupLogAndPID() error {
	if _, err := logwriter.Init(); err != nil {
		return fmt.Errorf("初始化日志失败: %w", err)
	}

	pidPath := filepath.Join("logs", "pid.txt")
	pid := os.Getpid()
	if err := os.WriteFile(pidPath, fmt.Appendf(nil, "%d\n", pid), 0644); err != nil {
		log.Printf("写入 pid 文件失败: %v", err)
	}
	return nil
}

func loadConfig(path string) {
	cfg = AppConfig{
		Addr:    "",
		WebPort: 0,
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("config %s 未找到，使用默认配置", path)
		return
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Printf("config 解析错误: %v", err)
		return
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:8001" // 必须配置，否则无法连接网关
	}
	if cfg.WebPort == 0 {
		cfg.WebPort = 3020
	}
}

func resolvePath(cfgDir, target string) string {
	if target == "" || filepath.IsAbs(target) {
		return target
	}
	return filepath.Join(cfgDir, target)
}

// tlsPrecheck 预检页面证书是否可用于 HTTPS。
//
// 两种「未配置」要分开：两项都留空 = 用户主动走明文（正常降级，返回 errTLSNotConfigured，
// 调用方不打告警）；只配一项、文件不存在、或证书私钥不成对 = 真问题，要打告警。
func tlsPrecheck(certPath, keyPath string) error {
	switch {
	case certPath == "" && keyPath == "":
		return errTLSNotConfigured
	case certPath == "" || keyPath == "":
		return fmt.Errorf("tls_cert 与 tls_key 必须同时配置（cert=%q key=%q）", certPath, keyPath)
	}
	if _, err := os.Stat(certPath); err != nil {
		return fmt.Errorf("证书文件不可用: %s", certPath)
	}
	if _, err := os.Stat(keyPath); err != nil {
		return fmt.Errorf("私钥文件不可用: %s", keyPath)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return fmt.Errorf("证书/私钥无法加载（损坏或不成对）: %w", err)
	}
	return nil
}

// tlsFallbackHint 退化到明文 HTTP 时要打出来的说明：原因 + 影响 + 两条补救路径。
// 提示里必须带「怎么生成证书」，否则用户只知道坏了、不知道下一步做什么。
func tlsFallbackHint(certPath string, cause error) []string {
	dir := filepath.Dir(certPath)
	if dir == "" || dir == "." {
		dir = "certs"
	}
	return []string{
		"⚠ 页面 HTTPS 已禁用，改用明文 HTTP 启动（原因：" + cause.Error() + "）",
		"  影响：页面协议为 http → 派生 ws:// 连明文网关；浏览器 WebTransport 不可用。",
		"  修复：按 mkcert/README.md 签发证书，把 server.pem / server-key.pem 放到 " + dir + "，",
		"        然后重启本工具；若网关侧也配了 gateway.tls_cert，则页面 https + WS 会走 wss。",
	}
}

func rebuildIndex() {
	var err error
	idx, err = proto.LoadIndex(bizDirs)
	if err != nil {
		log.Printf("proto 索引构建失败: %v", err)
	} else {
		log.Printf("proto 索引: %d 条消息（含内置引擎消息）", idx.Len())
	}
}

// 重新扫描 proto 源文件，重建消息索引。
func handleReload(w http.ResponseWriter, r *http.Request) {
	newIdx, err := proto.LoadIndex(bizDirs)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	idx = newIdx
	writeJSON(w, map[string]any{"ok": true, "count": idx.Len()})
}

// 返回静态页面目录，优先与配置文件同目录，避免启动目录不同导致找不到 web。
func webDir(cfgDir string) string {
	if d := resolvePath(cfgDir, "web"); d != "" {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return d
		}
	}
	return "./web"
}

// --- API handlers ---

func handleMessages(w http.ResponseWriter, r *http.Request) {
	type msgField struct {
		Name      string `json:"name"`
		Type      string `json:"type"`
		JSONKey   string `json:"json_key"`
		IsNumeric bool   `json:"is_numeric"`
		IsBool    bool   `json:"is_bool"`
	}
	type msgInfo struct {
		Name     string     `json:"name"`
		Value    int        `json:"value"`
		Dir      string     `json:"dir"`
		DirLabel string     `json:"dir_label"`
		IsEngine bool       `json:"is_engine"`
		Source   string     `json:"source"`
		Req      []msgField `json:"req"`
		Reply    []msgField `json:"reply"`
		Notify   []msgField `json:"notify"`
	}

	toFields := func(fields []proto.Field) []msgField {
		out := make([]msgField, 0, len(fields))
		for _, f := range fields {
			out = append(out, msgField{
				Name:      f.Name,
				Type:      f.Type,
				JSONKey:   f.JSONKey(),
				IsNumeric: f.IsNumericType(),
				IsBool:    f.IsBoolType(),
			})
		}
		return out
	}

	list := make([]msgInfo, 0)
	if idx != nil {
		for _, m := range idx.Messages() {
			list = append(list, msgInfo{
				Name:     m.Name,
				Value:    int(m.ID),
				Dir:      m.Dir,
				DirLabel: proto.DirLabel(m.Dir),
				IsEngine: proto.IsEngine(m.Name),
				Source:   m.Source,
				Req:      toFields(m.Req),
				Reply:    toFields(m.Reply),
				Notify:   toFields(m.Notify),
			})
		}
	}

	counts := map[string]int{"c2s": 0, "s2c": 0, "both": 0, "?": 0}
	for _, m := range list {
		counts[m.Dir]++
	}

	writeJSON(w, map[string]any{
		"messages": list,
		"count":    len(list),
		"counts":   counts,
	})
}

// handleAuthProxy 把页面的注册 / 登录请求转发给账号服。
//
// 为什么需要这层代理：账号服是独立 HTTP 服务（如 :8051），页面直接 fetch 到别的端口
// 属跨域，需要账号服配置 CORS；而本工具已经有一个同源后端，转发一次比让账号服
// 理解「测试工具」更省事，也避免在账号服上放开宽泛的 Access-Control-Allow-Origin。
//
// 路径映射：/api/auth/{login,signup} → {auth_addr}/auth/{login,signup}
func handleAuthProxy(w http.ResponseWriter, r *http.Request) {
	if cfg.AuthAddr == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "未配置账号服（config.yaml 的 auth_addr）"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, map[string]any{"ok": false, "error": "仅支持 POST"})
		return
	}

	// 只放行白名单动作：避免本服务沦落成任意 URL 的转发器（SSRF）。
	action := strings.TrimPrefix(r.URL.Path, "/api/auth/")
	switch action {
	case "login", "signup":
	default:
		writeJSON(w, map[string]any{"ok": false, "error": "未知的账号服操作: " + action})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "读取请求体失败"})
		return
	}

	target := strings.TrimRight(cfg.AuthAddr, "/") + "/auth/" + action
	httpCli := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpCli.Post(target, "application/json", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("连接账号服失败（%s）: %v", cfg.AuthAddr, err),
		})
		return
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "读取账号服响应失败"})
		return
	}
	// 原样透传账号服 JSON：前端按 success/owner/token/err 解析，与账号服契约保持一致，
	// 免得中间层再定义一套字段映射（多一层就多一处漂移风险）。
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write(raw)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	protoBusiness := []string{}
	if cfg.Proto.Business != nil {
		protoBusiness = cfg.Proto.Business
	}
	writeJSON(w, map[string]any{
		"addr":           cfg.Addr,
		"web_port":       cfg.WebPort,
		"auth_enabled":   cfg.AuthAddr != "",
		"proto_business": protoBusiness,
		"cert_hash":      fetchWTCertHash(cfg.Addr),
		// gateway_scheme 供前端按**网关实际协议**派生 ws/wss（而不是只看页面协议）。
		// 空=启动时没探到（网关未启动），前端退回按页面协议派生。
		"gateway_scheme": gatewayScheme,
	})
}

// 向网关拉取 WebTransport 证书的固定哈希，供前端建连时固定证书。
//
// 浏览器 WebTransport 走 QUIC，Chromium 不接受本机自建根证书，只能用
// serverCertificateHashes 固定。该哈希由网关以 GET /wt-cert-hash 提供（与 WS 同端口），
// 这里代为中转，避免前端跨域。
//
// **先 https 后 http**：哈希本身不是秘密，而网关 WS 端口在 demo 默认配置下是明文的
// （tls_cert 留空以保证 Unity 裸 TCP 客户端可用）。只试 https 会在这个最常见的组合下
// 恒取不到哈希，前端随之退到 PKI 路径 → 自签 WT 证书必然握手失败。
//
// 取不到时返回空串：前端随即退回「不固定」的普通 PKI 路径，
// 在已使用公共 CA 证书的部署下正是期望行为。
func fetchWTCertHash(addr string) string {
	if addr == "" {
		return ""
	}
	client := &http.Client{Timeout: 3 * time.Second}
	for _, scheme := range []string{"https", "http"} {
		if h, ok := fetchWTCertHashOnce(client, scheme, addr); ok {
			return h
		}
	}
	return ""
}

// fetchWTCertHashOnce 按指定协议取一次哈希；ok=false 表示该协议下不可达 / 响应不合法。
func fetchWTCertHashOnce(client *http.Client, scheme, addr string) (string, bool) {
	resp, err := client.Get(scheme + "://" + addr + "/wt-cert-hash")
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if err != nil {
		return "", false
	}
	var out struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", false
	}
	return out.Hash, true
}

// probeGateway 探测网关 WS 端口实际使用的协议（https / http），并顺带取回 WT 证书哈希。
// 探不到时返回空串（网关未启动 / 端口不通），调用方据此跳过一致性判断。
func probeGateway(addr string) (scheme, hash string) {
	if addr == "" {
		return "", ""
	}
	client := &http.Client{Timeout: 2 * time.Second}
	for _, s := range []string{"https", "http"} {
		if h, ok := fetchWTCertHashOnce(client, s, addr); ok {
			return s, h
		}
	}
	return "", ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(v)
}
