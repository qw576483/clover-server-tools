# msg-web

clover 网关 **Web 可视化测试工具**：浏览器 WebSocket / WebTransport 直连网关（0ms 代理开销），
在页面上查看消息、组包收发，方便联调登录与业务消息。
目录结构参考 `table/core`（`main.go` + `internal/{proto,client,logwriter}` + `web/` 前端 + `config.yaml`）。

## 目录结构

```
msg-web/
  go.mod                      # module clover-server-tools/msg-web；依赖 gopkg.in/yaml.v3
  main.go                     # 入口：加载配置 → 解析 proto → 静态页 + API 服务
  config.yaml                 # 配置：网关地址 + 端口 + TLS 证书 + proto 源文件夹
  web/                        # 前端静态页（index.html / app.js / style.css）
  certs/                      # 工具页面自身的 TLS 证书（mkcert 副本）
  internal/
    proto/                    # 递归扫描 proto 文件夹，解析消息号 + Request/Reply/Notify 结构体
    client/                   # WebTransport / WebSocket 相关辅助
    logwriter/                # 日志输出
```

## 配置文件（config.yaml）

```yaml
addr: 127.0.0.1:8001        # 引擎网关地址（web 页面直连；WS/WT 复用同一端口）
web_port: 3020              # 工具页面监听端口
tls_cert: certs/server.pem  # 工具页面自身 HTTPS 证书（mkcert 副本）
tls_key:  certs/server-key.pem
proto:
  business: []              # 业务侧自定义消息的 def 目录（相对本文件），按需添加
```

- 路径相对**配置文件所在目录**解析（与 `table/core` 一致）。
- 引擎消息（C2S / 回包 / 推送）已内置，无需配置引擎源码路径；`proto.business` 只需列出业务 def 目录。
- 证书两项同为空、**文件缺失、或证书与私钥不成对**时退回明文 http（明文网关调试用）。
  启动会打印原因与两条补救路径，**不会直接退出**——原先直接把路径交给 `ListenAndServeTLS`，
  失败即 `log.Fatalf` 退出，双击 exe 时表现为「闪退」，真实原因只留在 `logs/` 里
  （`certs/*.pem` 被 `.gitignore` 排除，新克隆 / 清理过证书的机器必踩）。
- `web.exe -http` 可强制明文（忽略 `tls_cert` / `tls_key`）。
- 启动会探测网关 WS 端口的实际协议并打印；**页面协议与网关协议不一致时给出告警**——不一致时
  浏览器两条通道都会失败（页面 https + 网关明文 → `wss://` 打明文端口被拒，`ws://` 又被当混合内容拦截）。

## 编译与运行

> 编译须在 `clover-server-tools/msg-web` 目录内执行；产物（可执行文件）会生成在**当前目录**，
> 统一命名为 `web` / `web.exe`，勿用其它名字。

```bash
# 先进入目录
cd clover-server-tools/msg-web

# 编译（产物落在当前目录，统一命名为 web / web.exe，勿用其它名字）
go build -o web     .        # macOS / Linux
go build -o web.exe .        # Windows

# 运行（浏览器打开 http://localhost:3020 或 https://localhost:3020）
./web              # 或 Windows: web.exe
./web -config my.yaml
./web -http        # 强制明文 HTTP（忽略证书）：页面 http + ws:// 连明文网关；WebTransport 不可用
```

## API

| 路径 | 说明 |
|------|------|
| `/` | 前端静态页（`web/` 目录） |
| `/api/messages` | 已加载消息列表（含 req/reply/notify 字段、方向统计） |
| `/api/config` | 当前配置（网关地址、端口、WebTransport 证书哈希、`gateway_scheme` = 启动时探测到的网关 WS 协议） |
| `/api/reload` | 重新扫描 proto 源文件，热加载消息定义 |
