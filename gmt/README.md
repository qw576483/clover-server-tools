# gmt —— Clover 游戏运营后台

独立进程的运营后台。Go 服务端、页面模板、前端逻辑全部是重写的；
**但主题静态资源是从老后台直接拷过来的**，这一点必须先说清楚（见下节）。

```bash
cd gmt
go run . -conf conf/app.yaml
# 打开 http://127.0.0.1:9000
# 默认账号 admin / admin123（**只在库里没有 admin 时**自动创建，登录后请立刻改口令）
# 登录页有算式验证码（自绘 PNG，5 分钟有效、一次性）
```

存储是 MySQL，配置与 `clover-server-engine` 对齐（同一套 yaml 键名、同一个驱动）。

## 数据存在哪

gmt 自己的数据（账号/角色/菜单/机器/区服/封禁留档/礼包批次/操作日志）存 MySQL：

```yaml
data:
  auto_create_table: true      # 启动执行 CREATE TABLE IF NOT EXISTS
  mysql:
    host: "127.0.0.1"
    port: 3306
    user: "root"
    pass: ""
    db_name: "clover-gmt"      # 独立库
    charset: "utf8mb4"
    auto_create: true          # 库不存在时自动建库
    parse_time: true
```

* 与引擎一致的地方：`data.mysql.*` 字段名与 `mysql.MySQLConfig` 一一对应；
  端口/字符集/时区/连接池/各项超时的默认值同一张表（`store/mysql.go`）；
  DSN 拼装方式、建库方式（`CREATE DATABASE IF NOT EXISTS` + 库名白名单校验）、
  `mysql connected: host:port db=... max_open=... max_idle=...` 这类日志写法都照引擎来。
* 表名**不带前缀**（独立库，库内没有别的东西要区分）：`account`、`role`、`menu`、
  `machine`、`server`、`ban`、`gift`、`audit`。保留字改名的做法也一样
  （`order → orders` 同理，角色说明列叫 `remark`，避开 `desc`）；
  有一条回归测试会拿 `information_schema.KEYWORDS` 检查表名没撞保留字。
* DDL 是**手写的、显式的**（`internal/store/schema.go`），不靠反射生成 SQL；
  时间列用 `DATETIME`（引擎约定），Go 侧仍是 int64 秒，转换在 `store` 边上做。
  启动时只建表、不自动改表：发现「Go 有字段、库里没列」只告警并给出 ALTER 语句。
* `data.auto_create_table=false` 时不自建表，但仍会在启动时检查表是否齐全，缺表直接起不来。
* 连不上库、库名/账号没配，进程立刻退出并打印 `data.mysql` 提示——不做「悄悄写到别处」的降级。
* 玩家、邮件、公告、订单等**游戏侧数据一律不进这个库**，由 `internal/gameclient` 调接口取。

## 登录与验证码

* 口令不存明文：`sha256(server.secret : 账号名 : 口令)`，`server.secret` 同时是会话 cookie 的签名密钥，
  改掉它 = 所有人立即掉线。
* 会话是 HMAC 签名的 cookie（12 小时），没有服务端 session 表。
* 验证码：登录页每次显示一张算式图（如 `3 + 6 = ?`），答对才继续。实现要点：
  自绘点阵字形生成 PNG（不引第三方库、不依赖字体文件）、答案用 HMAC 签名放在 cookie 里
  （**cookie 里不含答案本身**，否则脚本读自己的 cookie 就能拿到答案）、
  5 分钟有效、验证后立即作废（无论对错）。
* 同一 IP 连续 5 次口令错误会被暂时拒绝，防止脚本猛试。

## 资源来源（照搬了什么，没照搬什么）

`web/static/` 下这些文件是从老后台 `C:\Work\Atlantic\GmAdmin\public\static` **逐文件拷贝**的，
已用哈希逐个核对，字节一致（不是"参考"，是同一个文件）：

| 文件 | 用途 |
| --- | --- |
| `css/bootstrap.min.css`、`css/app.min.css` | 主题（Shreyu / Bootstrap 4，2023 年的构建产物） |
| `js/vendor.min.js`、`js/app.min.js` | jQuery 系依赖 + 主题交互（含 feather 图标渲染） |
| `js/sweetalert2.all.min.js` | 弹窗/提示 |
| `libs/select2/*`、`libs/flatpickr/*` | 下拉多选、日期选择 |
| `images/favicon.ico` | 页签图标 |

也就是说：**界面观感就是老后台那一套**。真要换掉，需要重写 CSS/交互（见文末"待定"）。

已删除从老后台拷来、但全站没有任何引用的资源（约 4.5MB）：
`css/icons.min.css`（unicons 字体图标，页面用的是 feather）、
`fonts/`（unicons / summernote / Cerebri Sans 共 33 个文件）、`images/users/avatar-*.jpg`、
以及和老 `favicon.ico` 内容重复的 `images/logo.png`（顶栏 logo 改为内联 SVG）。

## 与老后台（ThinkPHP 版）的关系

| 维度 | 老后台 | 这里 |
| --- | --- | --- |
| 代码组织 | 23 个 controller + view 目录 130 多个模板，每个功能一套「controller + view + JS」 | 声明式模块：列/查询/表单/行按钮是声明，列表页与弹窗只有一套实现 |
| 菜单 | `atlantic_gm_menu` 表里用 SQL 手工 INSERT 维护 | 菜单 = 已注册模块的投影，启动时按 URL 对账同步（有则改名/图标/排序，无则新增） |
| 权限 | `Base.php` 里把 `/Controller/action` 拿去和角色 `menuIds` 对应菜单的 `url` 做包含匹配（另有每控制器 `$rbac` 别名表） | 同样是「权限 = 菜单 URL」，前缀匹配并收敛到 `auth.CanAccess` 一处 |
| 时间列 | 混用 int 时间戳与 datetime | 统一 `DATETIME`（引擎约定），Go 侧 int64 秒，转换收在 store 边界 |
| 后台自身数据 | MySQL：`atlantic_gm_account/role/menu/machine/server/...`，DDL 散在 SQL 文件里，靠手工执行 | MySQL 独立库 `clover-gmt`，表名不带前缀，DDL 显式写在 `store/schema.go`，配置与 clover-server-engine 同构 |
| 机器运维 | 用 SSH 连机器、改配置文件 | **没有实现**（只保留了机器档案的增删改查） |
| 区服运维 | 批量开/关服、重载、把区服列表上传给 coop | **没有实现**（只保留区服档案的增删改查） |
| 玩家/公告/邮件/订单 | 一部分走 HTTP 调 auth/game，一部分直连库 | 一律走 HTTP（`internal/gameclient`），本地不留副本 |

## 已实现模块

| 分组 | 模块 | 说明 |
| --- | --- | --- |
| 运维管理 | `machine` 机器、`server` 区服 | 纯档案管理，保存不触发任何游戏侧动作 |
| 玩家管理 | `player` 玩家查询（自定义页）、`ban` 封禁 | 封禁/解封会调游戏侧（auth 或 game 节点） |
| 运营管理 | `bulletin` 公告、`gift` 礼包批次、`mail` 发邮件（自定义页）、`order` 订单 | 公告/订单数据在游戏侧；礼包可批量生成兑换码 |
| 系统设置 | `account` 后台账号、`role` 角色权限、`menu` 菜单、`audit` 操作日志 | 日志只读；超管角色（ID=1）不可删除 |

## 尚未实现（老后台有、这里没有）

版本包与热更任务（`VersionPack`）、配表 TSV（`GameTsv`）、语言包（`Language`）、
包 ID 定义（`PackageId`）、游戏账号（`GameAccount`）、游戏数据（`GameData`）、
游戏日志（`GameLog`）、内部充值（`GmRecharge`）、活动（`GameActivity`）、
开发者工具（`Developer`）、机器 SSH 运维、区服批量开/关服与重载、
玩家数据修改、区服邮件与邮件列表查询、封禁条件查询。

需要哪些，按下面的「新增一个模块」补即可（有游戏侧接口的走 `gameclient`，纯档案的直接声明）。

## 新增一个模块

只写一个声明（约 40 行），列表页、新增/编辑弹窗、删除、分页、排序、查询、
操作日志、菜单项全部自动有：

```go
func init() {
    modules.Register(&modules.Module{
        Key: "server", Name: "区服管理", Icon: "layers", Group: "ops",
        Search:  []modules.Field{{Key: "name", Label: "服名"}},
        Columns: []modules.Column{{Key: "name", Title: "服名"}},
        Form:    []modules.Field{{Key: "name", Label: "服名", Required: true}},
    })
}
```

数据在游戏侧、或者页面形态特殊时，用 `modules.RegisterPage` 注册自定义页面 —— 菜单与权限与其它模块一致。

## 接真实游戏服

`conf/app.yaml` 里把 `remote.driver` 改成 `http` 并填好四个地址：

```yaml
remote:
  driver: http
  endpoints:
    auth: http://127.0.0.1:8081
    game: http://127.0.0.1:8082
    coop: http://127.0.0.1:8083
    log:  http://127.0.0.1:8084
```

`driver: mock` 时不发任何真实请求，返回示例数据，可以纯前端联调与演示。

## 约定

* 接口一律返回 `{code, message, data}`；`code != 0` 前端统一提示，业务代码只写正常路径。
* 写操作先落本地再同步游戏侧 —— 宁可「后台有记录、游戏没生效」（可重试），
  也不要「游戏生效了、后台没记录」（无从追溯）。
* 每个写操作都进操作日志，含操作人、IP、模块与内容。
* 默认口令 `admin123` 仅用于本地起服，上线前必须改；`server.secret` 一改，所有会话立即失效。

## 待定

* 前端要不要脱离老主题：现在用的是照搬来的 Bootstrap 4 + Shreyu（移动端/暗色/组件风格都受它限制）。
  可换成一版自写的小 CSS（不引第三方包），或按设计系统重做。
