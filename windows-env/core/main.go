// Command env 是 Clover Windows 开发环境管理工具的入口。
// 仅做初始化与子命令分发, 具体逻辑见 internal/{svc,cli}。
package main

import (
	"os"

	"github.com/qw576483/clover-server-tools/windows-env/core/internal/cli"
	"github.com/qw576483/clover-server-tools/windows-env/core/internal/svc"
	"github.com/qw576483/clover-server-tools/windows-env/core/internal/util"
)

func main() {
	util.EnableUTF8Console()
	util.EnableANSIColors()
	svc.Init()
	cli.Run(os.Args[1:])
}
