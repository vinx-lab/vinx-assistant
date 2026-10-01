// vinx-assistant：Vinx 助手，单用户的微信收集箱。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/app"
	"github.com/vinx-lab/vinx-assistant/internal/backup"
	"github.com/vinx-lab/vinx-assistant/internal/batch"
	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/enrich"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

var version = "0.0.0-dev"

const usageText = `用法：vinx-assistant <子命令> [参数]

子命令：
  serve    常驻运行：收微信消息、定时整理、网页
  login    在终端扫码登录微信 ClawBot
  batch    立即跑一次 AI 整理（服务运行中也可以，两边不会重复整理）
  backup   导出数据库快照和附件（服务运行中也可以）
  password 设置或重置网页密码（服务运行中也可以）；所有登录随之失效
             --stdin 从标准输入读一行作为新密码，--clear 清除密码
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	home, _ := os.UserHomeDir()
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "vinx-assistant", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(stderr)
		cfg, err := parseConfig(fs, args[1:], os.Getenv, home, true)
		if err != nil {
			return 2
		}
		log := slog.New(slog.NewTextHandler(stdout, nil))
		a, err := app.New(cfg, log)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer a.Close()
		if err := a.Run(ctx); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "login":
		return cmdLogin(ctx, args[1:], os.Stdin, stdout, stderr)
	case "password":
		return cmdPassword(ctx, args[1:], os.Stdin, stdout, stderr)
	case "batch":
		fs := flag.NewFlagSet("batch", flag.ContinueOnError)
		fs.SetOutput(stderr)
		cfg, err := parseConfig(fs, args[1:], os.Getenv, home, false)
		if err != nil {
			return 2
		}
		st, err := store.Open(cfg.DBPath())
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer st.Close()
		r := &batch.Runner{Store: st, Clock: clock.Real{}, Fetcher: enrich.NewFetcher(nil), MediaDir: cfg.MediaDir(),
			Log: slog.New(slog.NewTextHandler(stderr, nil))}
		rep, err := r.Run(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "整理失败：", err)
			return 1
		}
		fmt.Fprintln(stdout, rep.String())
		return 0
	case "backup":
		fs := flag.NewFlagSet("backup", flag.ContinueOnError)
		fs.SetOutput(stderr)
		out := fs.String("o", "", "输出文件（默认当前目录 vinx-assistant-backup-时间.tar.gz）")
		cfg, err := parseConfig(fs, args[1:], os.Getenv, home, false)
		if err != nil {
			return 2
		}
		if *out == "" {
			*out = "vinx-assistant-backup-" + time.Now().Format("20060102-150405") + ".tar.gz"
		}
		if w := app.CheckPrivateDir(cfg.DataDir); w != "" {
			fmt.Fprintln(stderr, "警告：", w)
		}
		st, err := store.Open(cfg.DBPath())
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer st.Close()
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := backup.Write(ctx, st, cfg.MediaDir(), f); err != nil {
			f.Close()
			os.Remove(*out)
			fmt.Fprintln(stderr, "备份失败：", err)
			return 1
		}
		if err := f.Close(); err != nil {
			os.Remove(*out)
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "已备份到", *out)
		return 0
	}
	fmt.Fprintf(stderr, "未知子命令：%s\n\n%s", args[0], usageText)
	return 2
}
