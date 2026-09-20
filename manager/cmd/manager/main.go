// Command manager 是 clover 的集群进程编排工具：查看节点、查看状态、操作节点、滚动发布编排。
//
// 编译（产物落在 manager 目录）：
//
//	cd manager
//	go build -o manager ./cmd/manager        # macOS / Linux
//	go build -o manager.exe ./cmd/manager    # Windows
package main

import (
	"os"

	"github.com/qw576483/clover-server-tools/manager/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
