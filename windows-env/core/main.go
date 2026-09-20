// Command env 是 Clover Windows 开发环境管理工具的入口。
// 仅做初始化与子命令分发, 具体逻辑见 internal/{svc,cli}。
package main

import (
	"os"

	"clover-env/internal/cli"
	"clover-env/internal/svc"
	"clover-env/internal/util"
)

func main() {
	util.EnableUTF8Console()
	util.EnableANSIColors()
	svc.Init()
	cli.Run(os.Args[1:])
}
