# clover-server-tools

Clover 服务端的**开发与运维工具集**：本地依赖环境、网关调试客户端、压测机器人、集群编排工具、运营后台、本地 TLS 证书。

每个工具都是**独立可编译的 Go 程序**（各自带 `go.mod`），互不依赖，按需单独构建使用。

## 工具一览

| 目录 | 是什么 | 启动方式 |
|---|---|---|
| `windows-env/` | Windows 本地一键依赖环境：`etcd` / `nats` / `redis` / `mysql` 四件套，由 `core/env.exe` 统一启停（MySQL 以隐藏窗口后台启动，关闭终端后服务仍在跑） | `cd windows-env/core && go build -o env.exe . && ./env.exe start` |
| `msg-client/` | 网关**命令行调试客户端**：连网关收发消息，联调登录与业务消息；另含 `viewprobe` 视图探针 | `cd msg-client && go run ./cmd/client` |
| `msg-web/` | 网关 **Web 可视化**测试工具：浏览器 WebSocket / WebTransport 直连网关（0ms 代理开销），页面上查看消息、组包收发 | `cd msg-web && go run .`（默认 https://127.0.0.1:3020） |
| `robot/` | **机器人 / 自动化压测**客户端：批量登录 + 并发压测（可 ramp），输出可判定的聚合报告（成功数 / 分位延迟 / 吞吐 / 失败归类），支持 `--json` 给脚本用 | `cd robot && go run ./cmd/robot` |
| `manager/` | 集群**进程编排**工具：从 etcd 读集群拓扑（节点目录 + 服务实例），下发 drain（灰度下线）、网关上游切换、优雅退出，并做滚动发布编排 | `cd manager && go run ./cmd/manager` |
| `gmt/` | 游戏**运营后台**（Web）：账号 / 角色 / 菜单 / 机器 / 区服 / 封禁留档 / 礼包批次 / 操作日志，数据存 MySQL（配置键名与 `clover-server-engine` 对齐） | `cd gmt && go run . -conf conf/app.yaml`（默认 http://127.0.0.1:9000，首次自动创建 `admin/admin123`） |
| `mkcert/` | 本地开发 **TLS 证书**说明与排障手册（不含证书文件） | 见 [`mkcert/README.md`](mkcert/README.md)、[`mkcert/排障.md`](mkcert/排障.md) |

> 每个工具目录下都有自己的 `README.md`：配置字段、命令行参数、输出格式以那一份为准。

## 快速开始

```bash
git clone https://github.com/qw576483/clover-server-tools.git
cd clover-server-tools

# 1. 起本地依赖（Windows）：MySQL / Redis / NATS / etcd
cd windows-env/core
go build -o env.exe .
./env.exe start

# 2. 起一个调试客户端，连网关联调
cd ../../msg-client
go run ./cmd/client
```

## 二进制与证书不入库

- 各工具的 `*.exe` 由源码 `go build` 生成，**仓库不提交二进制**，请按上表自行编译。
- `mkcert/mkcert-v*.exe` 是第三方工具，需从 [mkcert releases](https://github.com/FiloSottile/mkcert/releases) 下载后放入 `mkcert/`。
- 本地 TLS 证书（`server.pem` / `wt.pem`）由 mkcert 在**本机生成**，同样不入库；生成方式见 `mkcert/README.md`。WebTransport 专用证书由网关自动签发与轮换，日常开发无需手动处理。

## 相关仓库

| 仓库 | 说明 |
|---|---|
| [clover-server-engine](https://github.com/qw576483/clover-server-engine) | Go 服务端引擎（本仓库各工具的服务对象） |
| [clover-doc](https://github.com/qw576483/clover-doc) | 框架文档，`server/tools/` 下有各工具的说明页 |
| [clover-tools](https://github.com/qw576483/clover-tools) | 打表工具 / AI skill 等**开发期**工具 |
