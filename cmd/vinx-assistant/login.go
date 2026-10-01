package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

// cmdLogin 在终端扫码登录；--import 从已有的凭证 JSON（与探针格式相同）导入。
func cmdLogin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	importPath := fs.String("import", "", "从凭证 JSON 文件导入（bot_token、ilink_user_id、baseurl）")
	home, _ := os.UserHomeDir()
	cfg, err := parseConfig(fs, args, os.Getenv, home, false)
	if err != nil {
		return 2
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer st.Close()
	sess := session.New(st, nil, nil)

	if *importPath != "" {
		b, err := os.ReadFile(*importPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		var c ilink.Cred
		if err := json.Unmarshal(b, &c); err != nil || c.BotToken == "" || c.UserID == "" {
			fmt.Fprintln(stderr, "凭证文件格式不对：需要 bot_token 和 ilink_user_id")
			return 1
		}
		if c.LoginAt.IsZero() {
			c.LoginAt = time.Now()
		}
		if err := sess.SaveCred(ctx, c); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "凭证已导入。")
		return 0
	}

	l := ilink.NewLogin(nil)
	qr, err := l.Start(ctx, sess.TokenHistory(ctx)) // 上游：上送最近 10 个 bot_token
	if err != nil {
		fmt.Fprintln(stderr, "获取二维码失败：", err)
		return 1
	}
	if q, err := qrcode.New(qr.Content, qrcode.Low); err == nil {
		fmt.Fprintln(stdout, q.ToSmallString(false))
	}
	fmt.Fprintf(stdout, "用微信扫码。二维码显示不正常时，用浏览器打开：\n%s\n\n", qr.Content)
	in := bufio.NewReader(stdin)
	verify := func(context.Context) (string, error) {
		fmt.Fprint(stdout, "输入手机微信上显示的数字：")
		line, err := in.ReadString('\n')
		return strings.TrimSpace(line), err
	}
	cred, err := l.Wait(ctx, qr, verify, func(s string) { fmt.Fprintln(stdout, "状态：", s) })
	if errors.Is(err, ilink.ErrAlreadyBound) {
		fmt.Fprintln(stderr, "这个 Bot 已绑定在另一份凭证上。如有旧凭证 JSON，可用 `vinx-assistant login --import 文件` 导入。")
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, "登录失败：", err)
		return 1
	}
	cred.LoginAt = time.Now()
	if err := sess.SaveCred(ctx, cred); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "登录成功。正在运行的 serve 会在 30 秒内用上新凭证。")
	return 0
}
