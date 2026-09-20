# robot

clover 的**机器人 / 自动化压测客户端**：批量登录 + 并发压测，产出一份可判定的量化报告。

同时是**给 AI / 脚本用的自动化入口** —— 输出可解析、退出码可判定、非交互不阻塞。

## 它是什么 / 不是什么

`msg-client` 与 `msg-web` 都是**单连接的交互式调试器**：人盯着一条连接，看它收发什么。
`robot` 是它们的**无人值守、N 连接的对照**：同一条登录链路，只是把「一个」变成「一批」，
并把结果变成能反复跑、能对比的数字。

| | msg-client / msg-web | robot |
|---|---|---|
| 连接数 | 1 | N（可 ramp） |
| 使用方式 | 人守着手敲 / 点按钮 | 无人值守，`--json` 给脚本 |
| 输出 | 逐帧打印 | 聚合报告（成功数 / 分位延迟 / 吞吐 / 失败归类） |
| 用途 | 联调某一条消息 | 测容量、测登录链路、跑回归基线 |

**不是**：不是反外挂工具，不是流量回放（不录制真实玩家报文），不生成"像人一样"的行为轨迹。

## 登录链路（与 msg-client 完全一致）

```
① 账号服 HTTP 换 JWT      POST {auth_addr}/auth/login   {account, password}
② 长连接登录              EMsgLogin{token}
③ 判定登录成功            ELoginReply{success=true}   ← 本工具的成功判定点
④ 可选：进游戏            EPushPlayerFullSync（账号有角色时引擎才推）
```

**第 ③ 步是刻意的选择**：成功率按 `ELoginReply.success` 算，而**不是**按有没有收到
`EPushPlayerFullSync`。原因见下面「判定口径」。

## 目录结构

```
robot/
  go.mod                 # module robot；依赖 quic-go + yaml.v3
  config.yaml            # 连接信息 + 压测默认参数（缺失自动生成）
  cmd/robot/main.go      # 入口
  internal/
    config/config.go     # 读 config.yaml
    client/client.go     # 线协议客户端（QUIC/TCP、帧编解码、心跳）
    authclient/authclient.go  # 账号服 HTTP 客户端（连接池按压测场景调大）
    load/
      robot.go           # 单个机器人的四阶段生命周期
      runner.go          # 批量编排 + 报告汇总
      stats.go           # 单机器人统计
      dist.go            # 延迟分布（蓄水池抽样 + 分位数）
    cli/                 # 命令行：run / signup / doctor
    util/                # Windows 控制台 UTF-8
```

## 配置文件（config.yaml）

```yaml
gateway: "127.0.0.1:8003"            # 网关 QUIC/UDP 地址
gateway_tcp: "127.0.0.1:8002"        # 网关 TCP 地址（QUIC 不可用 / 强制 tcp 时用）
auth_addr: "http://127.0.0.1:8051"   # 账号服 HTTP 地址

load:                                # 只是默认值，命令行参数优先
  robots: 10
  account_prefix: "robot_"
  account_start: 1
  password: "123123"
  transport: "auto"                  # auto | quic | tcp
  ramp_per_sec: 0                    # 0 = 不限（全并发）
  duration: "0s"                     # 0 = 登录成功即退
  msg_interval: "1s"
  login_timeout: "10s"
  signup_concurrency: 8
```

## 编译与运行

> 编译须在 `clover-server-tools/robot` 目录内执行；产物落在**当前目录**。

```bash
cd clover-server-tools/robot

go build -o robot     ./cmd/robot    # macOS / Linux
go build -o robot.exe ./cmd/robot    # Windows

./robot doctor
./robot run --robots 100
```

## 命令

| 命令 | 说明 |
|---|---|
| `doctor` | 连通性自检：网关 QUIC + 网关 TCP + 账号服 |
| `signup --start N --count M [--prefix p] [--concurrency 8]` | 批量注册账号（压测前铺数据） |
| `run [参数]` | 批量机器人：并发登录 + 可选持续压测 |

### run 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--robots N` | 10 | 机器人数量 |
| `--prefix p` | `robot_` | 账号前缀；第 i 个机器人用 `前缀+(start+i)` |
| `--start N` | 1 | 账号序号起点 |
| `--password p` | `123123` | 统一密码 |
| `--transport` | `auto` | `auto`（QUIC 优先，失败回退 TCP）/ `quic` / `tcp` |
| `--ramp N` | 0 | 每秒启动多少个机器人；**0 = 不限（全并发）** |
| `--duration d` | `0s` | 登录后保持在线时长；**0 = 登录成功即退**（纯登录压测） |
| `--msg ID` | 0 | 登录后周期发送的业务消息号；0 = 不发（只挂着） |
| `--body JSON` | `{}` | 周期消息体，支持 `{i}` 占位（替换为机器人序号） |
| `--interval d` | `1s` | 周期消息间隔 |
| `--once` | false | 周期消息只发一次 |
| `--unreliable` | false | 走不可靠通道发送（QUIC Datagram / 裸 UDP） |
| `--setup ID:JSON` | — | 登录后发送**一次**的消息（如建角），**可重复** |
| `--signup` | false | 账号不存在时自动注册（会往账号库写数据） |
| `--require-fullsync` | false | 要求必须收到 `EPushPlayerFullSync` 才算成功 |
| `--login-timeout d` | `10s` | 单机器人「连上 → 登录完成」的总超时 |
| `--dial-timeout d` | `5s` | 单次拨号超时 |
| `--probe-timeout d` | `2s` | QUIC 探测超时 |
| `--yes` | false | 跳过确认（非交互环境配合 `--signup` 使用） |

### 示例

```bash
# 1) 开工自检
robot doctor

# 2) 铺 100 个账号
robot signup --start 1 --count 100 --yes

# 3) 100 个机器人全并发登录（测登录链路容量）
robot run --robots 100

# 4) 每秒起 20 个，登录后保持 60 秒，每秒发一条业务消息
robot run --robots 100 --ramp 20 --duration 60s --msg 10001 --body '{"n":"r{i}"}'

# 5) 先建角再进游戏（登录后发一次 10001，然后必须等到全量同步）
robot run --robots 50 --ramp 5 --duration 30s \
  --setup '10001:{"name":"r{i}","server_id":1}' --require-fullsync

# 6) 脚本判定
robot run --robots 50 --json
# exit 0=全成功  1=全军覆没  2=用法错误  3=部分成功
```

## 判定口径（重要，别按直觉猜）

**① 登录成功 = 收到 `ELoginReply{success:true}`**

不是 `EPushPlayerFullSync`。引擎只在「登录成功**且已有角色**，或创建角色成功后」才推全量同步
（`internal/transport/event/push.go` 的 `EPlayerFullSyncNotify` 注释）。
账号没有角色时登录是**成功**的，只是没有全量同步 —— 把后者当失败，会让一次正常的登录压测全红。

要验证完整链路（登录 + 建角 + 进游戏）时加 `--require-fullsync`，并配合 `--setup` 先把角色建出来。

**② `duration=0` 时，机器人登录成功即退出**

这是「登录压测」模式：测的是**能同时登上来多少人**，不是在线承载。
要测在线承载（内存 / 定时器 / 心跳 / 广播）用 `--duration`。

**③ 截止时刻 = 启动耗时（ramp）+ duration**

所有机器人共用同一个窗口。把 ramp 算进去，是为了让**全部上线之后**有一段稳定的并发期
—— 否则排在后面的机器人在线时间会被 ramp 吃掉，压测窗口名不副实。

**④ 报告里的 `sent` 与 `recv` 不是一回事**

`sent` 是**本工具写出去的**，`recv` 是**收到的全部帧**（含登录回包、`EMsgUDPBindGrant`、推送）。
单机器人不发业务消息、保持 5 秒的基线是 `recv = 2`：登录回包 + UDP 绑定令牌。

**⑤ 服务端对「没注册 handler 的消息号」不回包**

引擎 `dispatch` 在无 handler 时打一条 `logic: no handler for msgID N` 然后返回，**不发回包**。
所以拿一个业务没实现的消息号压测，会看到 `sent` 涨、`recv` 不涨、RTT 只有登录那一个样本。
这不是工具丢帧（见报告里的 `dropped` / `unmatched_reply`，正常恒为 0）。
长时间跑还会触发 `pending_overflow`（等待表给内存封顶，属预期）。

## 报告

人类可读与 `--json` 两种形态，**信息量一致**（不允许某个数字只出现在一边）：

```
运行    3 个机器人 · 传输 auto · ramp 1/s · 保持 6s
        网关 127.0.0.1:8003（tcp 127.0.0.1:8002）· 账号服 http://127.0.0.1:8051

连接    成功 3 / 3
        延迟(ms)     P50 3 · P90 3 · P99 3 · 最大 4
        传输层    quic 3

登录    成功 3 / 3
        长连接(ms)    P50 1 · P90 1 · P99 1 · 最大 1
        账号服(ms)    P50 54 · P90 54 · P99 54 · 最大 54

会话    全量同步 0 / 3 · 断线 0
        在线(ms)     P50 6940 · ...

流量    发送 8 · 接收 6 · 发送失败 0
        吞吐 1 msg/s
        RTT(ms)    P50 1 · ...

结论    全部机器人连接 + 登录成功。
```

**为什么要分三段延迟**：登录由「账号服 HTTP 换 token」+「长连接 EMsgLogin」两段组成，
混成一个数字就分不清是**账号服慢**还是**游戏服慢**。

## 退出码（脚本靠它判断，不解析文案）

| 码 | 含义 |
|---|---|
| `0` | 全部机器人连接 + 登录成功 |
| `1` | 运行失败：配置错误，或**全军覆没**（一个都没成功） |
| `2` | 用法错误（参数非法，或非交互环境下未带 `--yes`） |
| `3` | 部分成功：有机器人失败但并非全部 |

> 「全军覆没」判 `1` 而不是 `3`：那说明环境根本没搭好（网关没起 / 账号全错），
> 报成"部分成功"会让人误以为"多跑几次就好"。

## 会被"压测结果"骗的几处（先读这段再下结论）

### 1) 网关的连接级准入 ≠ 服务端容量上限

网关有两个**连接建立**层面的闸门（见 `pkg/.../gwcore` 的 `Config`）：

- `gateway.max_conns_per_sec` —— 每秒新建连接数上限
- `gateway.queue_cap` / `queue_release_per_sec` —— 等候队列（**不启用排队时超限直接拒**）

超限时网关在**首帧**就 `Close` 连接，日志是：

```
WARN gwcore: admission rejected for c-30 (rate/queue)
```

客户端的表现是「连接建好了，但登录阶段连接被服务端关掉」
（QUIC 报 `Application error 0x0 (remote)`，TCP 报 `EOF`）。

**robot 会认出这个模式并在报告里给诊断提示。** 它**不是**登录链路故障：
把 `--ramp` 逐步调小，失败显著减少即可确认。

验证记录（本地起服实测，3 个机器人）：

| `--ramp` | 结果 |
|---|---|
| 不限（全并发） | 3/3 连上，1/3 登录成功 |
| 2 | 2/3 登录成功 |
| 1 | **3/3 成功** 与 **1/3 成功** 都复现过 |

> 「`--ramp 1` 也会被拒」这件事本身就是结论的一部分：**准入阈值在边界处不稳定**，
> 不会在某个 ramp 上干净地降到 0。要压真实容量，得先调大 `max_conns_per_sec`。别再往"调小 ramp"上死磕。

### 2) 消息级限流：单连接 64 帧/秒

网关默认 `ratelimit.GCRAPolicy(64, 128)`（见 `internal/app/bootstrap.go`）。
`--interval` 小于约 15ms 就会撞上它。**这不是服务端吞吐上限，是你自己发包太快**。
robot 在检测到这种情况时会先打一行提示。

### 3) 本机压测会先打满自己的资源

- 客户端侧：每个机器人一个 QUIC 连接 + 若干 socket；几千个要调 `ulimit -n`。
- 账号服 HTTP：`authclient` 已把连接池调大（`MaxIdleConnsPerHost: 4096`）。
  Go 默认是 **2**，不调的话几百个机器人换 token 会退化成串行 —— 你测的就成了自己的连接池。

### 4) 报告里的三个"可信度"指标

| 指标 | 含义 | 不为 0 说明 |
|---|---|---|
| `dropped` | 接收通道满被本工具丢弃的帧 | **工具侧问题**，`recv` 与 RTT 会偏低 |
| `unmatched_reply` | 收到但配不上任何请求的回包 | 多为已超时放弃的请求 |
| `pending_overflow` | 等回包表溢出被清理的条数 | 大量请求没回包（对端没 handler 就不回，属预期） |

正常压测这三个都应为 0。

## AI / 脚本自动化契约

以下五条是**稳定契约**（改实现时要一起守住），与 `manager` 同构：

1. **机器可读**：`--json` 把整个报告以**单个 JSON 对象**写到 stdout。
2. **错误也结构化**：`--json` 下失败同样输出 JSON（`{"ok":false,"error":"..."}`）。
3. **绝不阻塞等输入**：stdin 非终端时，会往服务端写数据的操作（`signup` / `run --signup`）
   必须显式带 `--yes`，否则**立即**返回 `ExitUsage(2)`。
4. **退出码可判定**：见上表。
5. **流分离**：报告走 stdout；**进度与诊断走 stderr**，`--json` 下进度静默。
   —— 进度条绝不能污染 stdout，否则 `robot run --json | jq` 会直接解析失败。

### AI 用法示例

```powershell
# 开工自检
$d = (.\robot.exe doctor --json 2>$null | Out-String | ConvertFrom-Json)
if (-not $d.ok) { "环境未就绪：$($d.gateway_quic.detail)" }

# 压测并按退出码判定
.\robot.exe run --robots 100 --ramp 10 --duration 60s --json 2>$null | Out-File report.json
if ($LASTEXITCODE -ne 0) { "有机器人失败，看 report.json 的 failures 与 errors" }
```

## 与 msg-client 的差异（都是刻意的）

| | 差异 | 原因 |
|---|---|---|
| 输出 | 全程静默，不打印 | N 条连接下任何一行打印都会变成刷屏 |
| UDP 绑定 | **不做** `EMsgBindUDP` | 每个机器人多一个常驻 socket + 保活协程，N 上去就是纯浪费；QUIC 的不可靠上行直接走 Datagram |
| 不可靠发送 | TCP 模式下走裸 UDP（0x55）**单向上行** | 不绑定就拿不到回推端点，但压测关心的是"服务端吃不吃得下"，上行够用 |
| 重连 | 不自动重连 | 压测要测的是「第一次能不能成功」；自动重连会把失败数掩盖掉 |
| 会话加密 | **不声明** `encrypt` | 通道加密是客户端声明制：不声明服务端就不下发 `session_key`，本次会话保持明文（压测关心吞吐，不想给每条连接加一层加解密开销） |
| TCP + TLS | 自动判定 | 网关 TCP 口可能走 TLS（`tcp_tls_disabled=false`）或明文；robot 在**进程内首次连接**探测一次并复用结论（TLS 优先 → 明文回退），不必额外配开关 |

## 测试

```bash
go test ./...
```

- `internal/client`：裸 TCP mock 服务端验证帧格式（`[1B type][4B len][payload]`）、
  requestID 配对与 `Recv` 投递；另验证「无 UDP 地址时不可靠发送必须报错而非静默成功」。
- `internal/load`：延迟分布的零值可用性（守一个曾经真出过的 bug）、分位数、蓄水池封顶、合并。

## 常见问题

**全部机器人登录失败，错误是「账号或密码错误」**
账号不存在。先 `robot signup`，或给 `run` 加 `--signup`。

**连上了但登录阶段被关连接**
优先看报告里的**诊断**段：多半是网关连接级准入（见上文「会被压测结果骗的几处」第 1 条）。
调小 `--ramp` 验证。

**`doctor` 显示 QUIC 不通但 TCP 通**
网关没起 QUIC 或 UDP 端口被占。用 `--transport tcp` 仍可压测。

**几百个机器人时本机报 `too many open files`**
调 `ulimit -n`；或用 `--transport tcp` 降低单连接资源占用。

**账号想清理**
本工具注册的账号就是 `前缀+序号`（默认 `robot_1`…），按前缀在账号库里删即可。
