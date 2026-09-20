# Clover Windows 开发环境

本目录为 `clover-server-engine` 及其上层业务工程本地开发提供一键式依赖环境（Windows）。
包含四个开箱即用的中间件：`etcd`、`nats`、`redis`、`mysql`。

> ⚠️ **中间件二进制不入库**：`etcd/` `mysql/` `nats/` `redis/` 四个目录合计约 1.8 GB，属第三方发行物，仓库根 `.gitignore` 已排除。
> 新克隆本仓库后这四个目录是空的，需自行把对应中间件解压到各自目录；
> 启动脚本与配置（`core/*.bat`、`core/*.vbs`、`core/my.ini`）随仓库提供，无需另找。

环境由 Go 编译的 `core/env.exe` 统一管理，直接以命令行方式调用，无需外层 bat 包装。

**特点：**
- 一键启动/停止 `etcd` / `nats` / `redis` / `mysql`。
- MySQL 通过 `mysql_start.vbs` 以隐藏窗口方式在后台启动，**不会弹出控制台窗口**。
- `env.exe` 退出或关闭终端后，服务仍然保持运行。
- 自动向上查找组件目录，无论从 `core/` 还是命令行任意位置启动，都能定位到正确的 `windows-env/`。

## 目录结构

```
windows-env/
├── README.md
├── core/             # 工具目录 (自包含, Go 工程, 分层见下)
│   ├── env.exe       # 编译产物: 环境管理 CLI
│   ├── my.ini        # MySQL 基准配置 (你直接提交维护的文件, 工具只读取部署, 从不生成/覆盖)
│   ├── go.mod
│   ├── main.go       # 入口: 仅初始化 + 调 cli.Run (薄)
│   └── internal/        # 私有业务包, 按领域拆分且各自可单测
│       ├── util/          # 小工具 (UTF-8 控制台等)
│       │   └── console.go    # EnableUTF8Console (防中文乱码)
│       ├── svc/          # 服务模型/检查/后台拉起/停止/MySQL 生命周期
│       │   ├── svc.go        # Service 模型 + 路径/端口/PID/进程工具
│       │   └── lifecycle.go  # start/stop/info/restart 编排 + MySQL 特例
│       └── cli/           # 子命令解析与分发 + mysql-cmd/redis-cmd 客户端入口
│           └── cli.go
│   ├── logs/         # 运行日志 (启动后自动生成: etcd.log / nats.log / redis.log / mysql.log)
│   └── run/          # PID 记录 (启动后自动生成: *.pid)
├── my.ini             # MySQL 实际生效配置 (env 根目录, 由 env.exe 首次启动时生成, 可自行编辑)
├── etcd/              # etcd 二进制 (etcd.exe / etcdctl.exe / etcdutl.exe)
│   └── default.etcd/  # etcd 数据目录（自动生成）
├── nats/              # nats-server.exe
├── redis/             # redis-server.exe + redis.conf
└── mysql/             # MySQL ZIP 解压目录
    ├── bin/           # mysqld.exe / mysql.exe / mysqladmin.exe
    ├── data/          # MySQL 数据目录（首次启动初始化生成）
    └── back.*.my.ini  # my.ini 的自动备份 (与基准不一致时生成, 可安全删除)
```

> **关于 `my.ini`**（三处位置）：
> - `core/my.ini`：**你提交维护的基准配置**。直接放进仓库、自行编辑（例如调内存、加参数），工具**只读取、从不生成也不覆盖**它；若缺失则启动报错。
> - `windows-env/my.ini`（env 根目录）：**实际生效配置**，MySQL 启动读取它，由基准部署而来。
> - `mysql/back.<YYYYMMDD.HHMMSS>.my.ini`：**备份**，仅当根目录 `my.ini` 与你的基准不一致时生成。
>
> 每次 `start mysql` 时，env 会用你的基准（`core/my.ini`）**部署**到根目录的 `my.ini`：
> - 根目录不存在 → 直接由基准复制生成；
> - 已存在且**相同** → 跳过部署；
> - 已存在且**不同** → 先把旧文件备份到 `mysql/back.*.my.ini`，再由基准覆盖（**以你的基准为准**，保证 MySQL 用基准配置启动，避免旧参数让新版本 `mysqld` 启动即 abort）。
> 想持久自定义：直接改 `core/my.ini`（已提交进仓库），下次启动会自动部署生效；不要只改根目录那份，因为它每次都会被基准覆盖。

## 组件与默认端口

| 组件   | 目录    | 默认端口                  | 用途                          |
|--------|---------|---------------------------|-------------------------------|
| etcd   | `etcd/` | 2379 (client) / 2380 (peer) | 分布式 KV / 锁 / 服务注册     |
| nats   | `nats/` | 4222 (client)            | 消息队列（玩家数据同步广播）  |
| redis  | `redis/`| 6379                     | 数据缓存层                    |
| mysql  | `mysql/`| 3306                     | 持久化存储（账号 / 玩家 / 业务） |

> 端口与 `clover-server-engine` 默认配置完全对齐。

## 使用方法

### 1. 首次准备（仅第一次）
下载地址已**内置在 `core/env.exe` 中**：当检测到某组件缺失时，会自动打印对应官方下载地址与文件名。也可直接参考下面汇总，获取 Windows 压缩包后**重命名**放入本目录：

- **mysql**：https://dev.mysql.com/downloads/mysql/ （Windows x86 64-bit 的 ZIP Archive）
- **etcd** ：https://github.com/etcd-io/etcd/releases （etcd-vX.X.X-windows-amd64.zip）
- **nats** ：https://github.com/nats-io/nats-server/releases （nats-server-vX.X.X-windows-amd64.zip）
- **redis**：https://github.com/redis-windows/redis-windows/releases （Redis-X.X.X-Windows-X64-msys2.zip）

解压后分别命名为 `mysql` / `etcd` / `nats` / `redis` 放在本目录（与 `core/` 同级）。

### 2. 使用方式

**方式 A：双击 `core\env.exe`（推荐，交互菜单）**
双击后程序进入交互菜单，窗口常驻不闪退，输入选项即可：

```
Clover Windows 开发环境 - 交互模式 (输入 q 退出, 输入 ? 看帮助)
--------------------------------------------------
 1) open    启动全部
 2) close   停止全部
 3) status  查看状态
 ?) help    帮助
 客户端: cmysql / credis
 直接输入服务名可单独启动: etcd / nats / redis / mysql
    也可直接输入命令, 如: open / close / status / cmysql / mysql
--------------------------------------------------
> _
```

- 输入 `1`/`2`/`3`/`?` 快捷键直接执行；
- 也可直接敲命令（如 `open redis`、`close`、`cmysql`、`mysql`）；
- 输入 `q` 退出。

**方式 B：命令行**
- **启动**：`core\env.exe start`（可加服务名，默认全部）。
- **停止**：`core\env.exe stop`（可加服务名，默认全部）。
- 其余子命令见下表。

`env.exe` 子命令：

| 命令 | 说明 |
|------|------|
| `env.exe start [name]` | 检查并启动服务，`name` 可选：`etcd`/`nats`/`redis`/`mysql`，默认全部 |
| `env.exe stop [name]`  | 停止服务（默认全部） |
| `env.exe restart [name]` | 重启服务（默认全部） |
| `env.exe info` \| `status` | 查看各服务运行状态、端口、PID、组件是否就绪（`status` 是 `info` 别名） |
| `env.exe mysql-cmd [args]`  | 打开 MySQL 客户端（别名 `cmysql`）；无参→交互窗口，带参→直跑（如 `mysql-cmd -e "SELECT 1;"`） |
| `env.exe redis-cmd [args]`  | 打开 redis-cli（别名 `credis`）；无参→交互窗口，带参→直跑（如 `redis-cmd ping`） |
| 裸服务名 `etcd`/`nats`/`redis`/`mysql` | 直接启动对应服务（等价于 `open <name>`）；env.exe 仅内置 `cmysql`/`credis` 客户端，etcd 可用 `etcd/` 目录内的 `etcdctl.exe` 手动操作，nats 未捆绑 CLI |
| `env.exe help` | 显示帮助 |

> `mysql-cmd` / `redis-cmd`（或简写 `cmysql` / `credis`）是统一的客户端入口：不带参数时新开一个控制台窗口进入交互模式；带参数时直接执行并把结果打到当前终端。env.exe 仅封装这两类客户端；etcd 可用 `etcd/` 目录内的 `etcdctl.exe` 手动操作，nats 未捆绑 CLI。

`env.exe` 会先完成**全部环境检查**（缺失则打印下载地址并退出），再统一后台拉起服务，不再开一堆独立窗口，日志与 PID 都收在 `core/logs/` 与 `core/run/`。MySQL 首次启动自动生成 `my.ini` + 初始化数据目录（root 为**空密码**；本工具不修改 root 密码）。MySQL 在 Windows 上可能同时出现两个 `mysqld.exe` 进程，`env.exe` 会记录真正监听 3306 的那个 PID。

### 3. 验证
启动成功后，可在命令行验证：

```bat
mysql\bin\mysql.exe -uroot -e "SELECT VERSION();"
redis\redis-cli.exe -p 6379 ping        :: 返回 PONG
etcd\etcdctl.exe endpoint health         :: 查看集群健康
core\env.exe info                       :: 查看四个服务状态
```

## 默认账号

| 组件 | 用户名 | 密码     | 备注                            |
|------|--------|----------|---------------------------------|
| mysql | `root` | 空（无密码） | 本地开发默认；工具不修改 root 密码 |

etcd / nats / redis 均**无密码**，仅监听 `127.0.0.1`。

## 常见问题

- **MySQL 弹窗**：确保使用的是最新编译的 `core/env.exe`；旧版本曾在 bat 中使用 `--console` 导致窗口弹出，新版本已移除。
- **MySQL 启动后 `env.exe` 退出、服务也停了**：检查是否用的是新版 `vbs` 隐藏启动；旧版 `spawn()` 拉起的方式在关闭终端时会被系统带掉。
- **命令无反应 / 报错**：打开 `cmd` 进入本目录手动运行 `core\env.exe info` 看完整报错（通常是某组件没下载或端口被占用），按提示处理即可。
- **`mysqld` 初始化失败**：删除 `mysql\data` 与根目录 `my.ini`（及其 `back.*.my.ini` 备份）后重跑 `start` 即可重新初始化。
- **端口被占用**：`env.exe info` 会显示哪个端口被占；多半是上次没正常停止，运行 `stop` 即可。
- **MySQL 连不上（业务工程报 `connection refused`）**：先 `env.exe info` 确认 mysql 为「运行中」，再看 `core\logs\mysql.log` 有无报错。

## 重新编译（可选）
若需修改 `env.exe` 源码：

```bat
cd windows-env/core
go build -o env.exe .
```
