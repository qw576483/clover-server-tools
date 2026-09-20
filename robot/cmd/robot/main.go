// Command robot 是 clover 的机器人 / 自动化压测客户端。
//
// 用法：
//
//	robot doctor                                          # 连通性自检
//	robot signup --start 1 --count 100 --yes              # 批量铺账号
//	robot run --robots 100                                # 100 并发登录压测
//	robot run --robots 100 --ramp 20 --duration 60s \
//	      --msg 10001 --body '{"n":"r{i}"}' --json        # 登录后持续压测
//
// 定位：msg-client / msg-web 是「单连接的交互式调试器」，robot 是它们的
// 「无人值守、N 连接的自动化对照」。走的是同一条登录链路：
//
//	① 账号服 HTTP 换 JWT        POST {auth_addr}/auth/login
//	② 长连接登录                EMsgLogin{token}
//	③ 进入在线态                ELoginReply{success,owner}（判定点）
//	                            EPushPlayerFullSync（可选，账号有角色时才有）
//
// 与 msg-client 的差异（都是刻意的，改之前先读 internal/client 的包注释）：
//   - 全程静默，不打印 —— N 条连接下任何一行打印都会变成刷屏；
//   - 不做 EMsgBindUDP 绑定 —— 每个机器人多一个常驻 socket 不划算；
//   - 结果是一份可判定的报告（人类可读 / --json 两种形态，信息量一致）。
package main

import (
	"os"

	"robot/internal/cli"
	"robot/internal/util"
)

func main() {
	util.EnableUTF8Console() // Windows 控制台切 UTF-8，否则报告里的中文是乱码
	os.Exit(cli.Run(os.Args[1:]))
}
