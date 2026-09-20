# 本地 TLS 证书（mkcert）

本目录提供本地开发的 TLS 证书。**日常开发基本不用管它**——WebTransport 专用证书由网关自动签发与轮换。

> **出问题了？** 直接看 [排障.md](./排障.md)，那份是排查手册。本文只讲正常使用。

## 谁用哪张证书

| 接入 | 端口 | 协议 | 证书 |
|---|---|---|---|
| TCP | 8002 | TLS 1.3 | `server.pem` |
| WebSocket | 8001 TCP | wss | `server.pem` |
| QUIC（原生客户端）| 8003 UDP | QUIC | `server.pem` |
| **WebTransport** | **8001 UDP** | HTTP/3 | **`wt.pem`（自动生成）** |
| msg-web 页面 | 3020 | https | `server.pem` |

## 文件

| 文件 | 说明 |
|---|---|
| `certs/server.pem` / `server-key.pem` | 主证书，SAN 覆盖 `localhost`、`127.0.0.1`、`::1` |
| `certs/wt.pem` / `wt-key.pem` | WebTransport 专用证书，**首次启动自动生成、到期自动轮换**，不用手工签发（实际位于网关工程的 `certs/` 目录）|
| `mkcert-v1.4.4-windows-amd64.exe` | mkcert 可执行文件 |

- `server.pem` 有效期到 2028-11-29，到期重跑下方命令
- `wt.pem` 有效期 10 天，但网关自动轮换，不用管

## 快速开始

### 1. 首次使用：生成并安装证书（每台机器只需一次）

```powershell
cd D:\work\full-dev\clover-server-tools\mkcert

# 生成并安装本地根 CA（装进系统信任库）
.\mkcert-v1.4.4-windows-amd64.exe -install

# 生成主证书（SAN 覆盖本地回环地址）
.\mkcert-v1.4.4-windows-amd64.exe -cert-file certs\server.pem -key-file certs\server-key.pem localhost 127.0.0.1 ::1

# 同步到实际加载点（网关工程与 msg-web 各自一份）
Copy-Item certs\server.pem     D:\work\full-dev\clover-server-tools\msg-web\certs\ -Force
Copy-Item certs\server-key.pem D:\work\full-dev\clover-server-tools\msg-web\certs\ -Force
```

> **证书每台机器独立，绝不能跨机器拷贝。**
> mkcert 根 CA 在本机随机生成并装进本机信任库，A 机器的证书拷到 B 机器必然报
> `tls: unknown certificate`。所有 `*.pem` 已在 `.gitignore` 中。

### 2. 启动网关

在**网关工程**（业务侧自己的 server 目录）下编译并启动：

```powershell
cd <你的网关工程>
go build -o server.exe .
.\server.exe
```

启动日志里应当出现（哈希值每次可能不同）：

```
gateway: WebTransport certificate hash=9fae1499...（经 /wt-cert-hash 下发）
```

首次启动会自动在 `server/certs/` 下生成 `wt.pem`、`wt-key.pem`。

### 3. 启动 msg-web

```powershell
cd D:\work\full-dev\clover-server-tools\msg-web
go build -o msg-web.exe .
.\msg-web.exe
```

输出：

```
msg-web 已启动 https://localhost:3020（WS直连 wss://127.0.0.1:8001/ws）
```

### 4. 打开页面确认

浏览器访问 `https://localhost:3020`，左侧日志出现这两行**即为正常**：

```
已固定服务器证书 sha256:9fae1499…
✓ WebTransport 连接成功
```

看到 `回退到 WebSocket 连接...` 说明功能还能用但已退化，去 [排障.md](./排障.md) 查。

## 配置

网关工程的 `configs/all/server.yaml`：

```yaml
gateway:
  tls_cert:  "certs/server.pem"      # 主证书，TCP/WS/QUIC 共用
  tls_key:   "certs/server-key.pem"
  enable_wt: true                    # 是否启用 WebTransport
  wt_cert:   ""                      # WT 专用证书路径；空 = certs/wt.pem
  wt_key:    ""                      # WT 专用私钥路径；空 = certs/wt-key.pem
  wt_pin:    true                    # 证书固定；本地 true，生产 false
```

改完**必须重新编译并重启**才生效。

代码侧（`gwcore.Config`）：

```go
cert, _ := tls.LoadX509KeyPair("certs/server.pem", "certs/server-key.pem")
gwcore.Config{
    TLSConfig:   &tls.Config{Certificates: []tls.Certificate{cert}},
    WTTLSConfig: wtTLS,  // 自动生成的 ECDSA 短有效期证书；nil 时回退 TLSConfig
    WTCertHash:  wtHash, // 其 SHA-256(hex)，经 /wt-cert-hash 下发
}
```

## 生产环境

不要用本证书上线。改用 Let's Encrypt 等公共 CA 证书配到 `tls_cert` / `tls_key`，并：

```yaml
gateway:
  wt_pin: false   # 公共 CA 证书走标准 PKI，不做固定
```

设 `false` 后 WT 复用主证书，不再生成专用证书、不再发布哈希。

> 原因：证书固定要求有效期 < 2 周，与 CA 签发的 90 天证书不兼容。

## 为什么 WT 要单独一张证书

两句话版本（完整根因见 [排障.md](./排障.md)）：

- 浏览器对 WebTransport（走 QUIC）的证书校验**不接受本地自建根**，且 `--ignore-certificate-errors` 也无效，所以"让浏览器信任证书"这条路对 WT 走不通；
- 唯一通路是 W3C 的 `serverCertificateHashes` 证书固定，而它要求证书**必须 ECDSA P-256（禁止 RSA）且有效期 < 2 周**——这与 WS 所需的"受信任长期证书"天然冲突，只能拆成两张。
