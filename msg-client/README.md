# msg-client

clover 网关**命令行调试客户端**：用命令行连上网关，发送 / 接收消息，方便联调登录与业务消息。
目录结构参考 [clover-tools 仓库的 `table/core`](https://github.com/qw576483/clover-tools/tree/main/table/core)（`cmd/<name>/main.go` + `internal/{config,proto,client}` + `config.yaml`）。

## 线协议（与 clover-server-engine 对齐，自包含实现）

- **传输层帧**（网关客户端接入一致）：`[1B 帧类型][4B 大端长度][payload]`
  - 帧类型：`0`=数据，`1`=ping，`2`=pong；本工具**只处理数据帧**，ping / pong 一律忽略（不回 pong）。
- **客户端帧**（payload）：`[4B 大端 requestID][4B 大端 msgID][body]`，body 为 JSON。
  - `requestID != 0` → 请求/回包，客户端按 `requestID` 配对；普通回包 `msgID` 恒为 `0`。
  - `requestID == 0` → 推送，客户端按 `msgID` 路由。
  - 错误回包保留特殊值 `EMsgError=0xFFFFFFFF`。
- 关键 opcode：`EMsgLogin=2`、`EMsgResumeSession=3`、`EMsgBindUDP=5`（客户端上报常驻裸 UDP 端点）、`EMsgUDPBindGrant=6`（网关下发绑定令牌）、`EPushPlayerFullSync=4001`、`EPushAlert=4002`、`EPushDataSync=4003`、`EMsgError=0xFFFFFFFF`（1 号原为 `EMsgSignup`，已作废保留）；
  业务消息号需 `>= 10001`（引擎占 `[1,10000]`，`InternalMsgMax=10000`）。

## 目录结构

```
msg-client/
  go.mod                      # module github.com/qw576483/clover-server-tools/msg-client；依赖 gopkg.in/yaml.v3 + github.com/quic-go/quic-go（QUIC 线路）
  config.yaml                 # 配置：网关地址 + proto 源文件夹
  cmd/client/main.go          # 入口：加载配置 → 解析 proto → REPL
  internal/
    config/config.go          # 读 config.yaml（yaml.v3），路径解析为绝对路径，缺失自动生成默认
    proto/parse.go            # 递归扫描 proto 文件夹，解析 Msg 消息号 + Request/Reply/Notify 结构体
    client/client.go          # TCP 客户端 + 线协议编解码 + 内置引擎 opcode/类型
    authclient/               # 账号服 HTTP 客户端（注册 / 登录换 token）
    logwriter/                # 会话日志落盘
    util/                     # 终端输出工具：ANSI 配色 + Windows 控制台适配（UTF-8/虚拟终端）
```

## 配置文件（config.yaml）

```yaml
addr: "127.0.0.1:8003"              # 默认网关地址（QUIC/UDP；启动 -addr 可覆盖）
tcp_addr: "127.0.0.1:8002"          # TCP 回退地址（QUIC 连接失败时使用）
auth_addr: "http://127.0.0.1:8051"  # 账号服 HTTP 地址：登录 / 注册都经它走 HTTP

proto:
  # 引擎内核消息**已内置**在 CLI 里（opcode + ELoginRequest/Reply/Notify 结构体），
  # 不再需要配置引擎源码路径——引擎独立发布后本工具照样可用。
  # 这里只配业务 proto 文件夹（业务侧自定义的 body 类型，按需添加；可多个）。
  business: []
```

- 路径相对**配置文件所在目录**解析（与 [clover-tools 仓库的 `table/core`](https://github.com/qw576483/clover-tools/tree/main/table/core) 一致）。
- CLI 启动时递归扫描这些文件夹下的 `*.go`（跳过 `_test.go`），自动提取：
  - `const MsgXxx / EMsgXxx uint32 = N` 消息号（含 `0x` 十六进制）；
  - 所有 `type Xxx struct {...}` 中带 `json:"tag"` 的字段，按名字后缀分类为
    **Request**(Req/Request) / **Reply**(Reply/Resp) / **Notify**(Notify/Push) / 其它；
  - 消息号自动关联 `base+Req/Request`、`base+Reply/Resp`、`base+Notify/Push`（base=去掉 `Msg`/`EMsg` 前缀）。
- 引擎 / 业务往里加新消息或结构体，**CLI 启动即自动识别，无需改 CLI**。

## 编译与运行

> 编译须在 `msg-client` 目录内执行；产物（可执行文件）会生成在**当前目录**（即本目录），不要编到项目根目录。

```bash
# 先进入目录
cd msg-client

# 编译（产物落在当前目录，统一命名为 client / client.exe，勿用其它名字）
go build -o client     ./cmd/client      # macOS / Linux
go build -o client.exe ./cmd/client      # Windows

# 用 config.yaml（首次运行若不存在会自动生成默认配置）
./client            # 或 Windows: client.exe
# 指定配置 / 覆盖地址
./client -config my.yaml -addr 127.0.0.1:9001
```

## 交互命令

| 命令 | 说明 |
|------|------|
| `help` / `?` | 显示帮助 |
| `connect [addr]` | 连接网关（缺省用启动 `-addr`；会先断开旧连接） |
| `disconnect` | 断开当前连接 |
| `login <account> <password>` | 一键登录：先 HTTP 换 token，再发 `EMsgLogin{token}` |
| `signup <account> <password>` | 注册（走账号服 HTTP；**不会**自动登录，需再执行 `login`） |
| `ls` / `list` | 列出所有已加载消息（方向 + req/reply/notify 字段） |
| `about <MsgName\|id>` | 查看单条消息的说明与字段 |
| `types [Name]` | 列出所有 Request/Reply/Notify 结构体（按类别）；或查指定结构体字段 |
| `send <MsgName\|id> [k=v ...]` | 按消息名或号**自动绑定字段**组 JSON 包发送 |
| `send <id> <raw-json>` | 兜底：索引里没有该消息时直接发原始 JSON |
| `send_pos ...` | 位置同步类消息的快捷发送 |
| `reload` | 重新扫描 proto 目录 |
| `quit` / `exit` | 断开并退出 |

连上网关后，后台读协程会把服务端下发的每一帧自动打印为：

```
<<< [MsgLoginReply]
{
  "owner": "alice",
  "token": "tok-xyz",
  "success": true
}
```

### 示例

```
clover> ls
NAME                       ID           DIR   REQ / REPLY / NOTIFY
--------------------------------------------------------------------------------------
EMsgLogin [engine]        2            both  req[token]  reply[owner,token,success,err]  notify[-]
MsgCreatePlayer [biz]     10001        both  req[name,server_id]  reply[success,player_id,err]  notify[-]
...
clover> types
== request (2) ==  ...
== reply (3) ==    ...
== notify (1) ==   NotifyPush  target,msg_id
clover> login clover 123123
>>> 已发送 EMsgLogin (id=2): {"token":"eyJhbGciOi..."}
clover> send MsgCreatePlayer name=clover server_id=1
>>> 已发送 MsgCreatePlayer (id=10001): {"name":"clover","server_id":1}
```

## body 类型能否改？（重要结论）

- **EMsgLogin 的 body 类型由引擎固定为 `proto.ELoginRequest`**（只有 `token` 一个字段，
  账号密码只发给账号服 HTTP；且消息号 ≤ `InternalMsgMax`（10000）被 `g.On` 拒绝，demo 无法覆盖）。
  CLI 经 `login <account> <password>` 或 `send EMsgLogin token=...` 收发，字段由 `ls` / `types` 动态识别。
- **业务消息（≥10001）的 body 类型由业务侧 `def` 包自定义**（如 `CreatePlayerReq`/`CreatePlayerReply`），
  业务可随意增改；改完加进 `config.yaml` 的 `proto.business` 即被 `ls` / `send` 自动识别。
