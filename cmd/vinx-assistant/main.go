// vinx-assistant：Vinx 助手，单用户的微信收集箱。
package main

import (
	"fmt"
	"io"
	"os"
)

var version = "0.0.0-dev"

const usageText = `用法：vinx-assistant <子命令> [参数]

子命令：
  serve    常驻运行：收微信消息、定时整理、网页
  login    在终端扫码登录微信 ClawBot
  backup   导出数据库快照和附件（服务运行中也可以）
  version  显示版本

通用参数：--data 数据目录（VINX_DATA），serve 另有 --listen（VINX_LISTEN）
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "vinx-assistant", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	}
	fmt.Fprintf(stderr, "未知子命令：%s\n\n%s", args[0], usageText)
	return 2
}
