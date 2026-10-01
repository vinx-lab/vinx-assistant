package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/vinx-lab/vinx-assistant/internal/app"
	"github.com/vinx-lab/vinx-assistant/internal/auth"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

// cmdPassword 设置、重置或清除网页密码，同时让所有登录失效。服务运行中也可以执行：网页每个请求都现读数据库。
//
//	vinx-assistant password            交互输入两次新密码（终端上不回显）
//	vinx-assistant password --stdin    从标准输入读一行作为新密码，不确认（脚本用）
//	vinx-assistant password --clear    清除密码，恢复无密码状态
func cmdPassword(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fromStdin := fs.Bool("stdin", false, "从标准输入读一行作为新密码（不确认）")
	clearPW := fs.Bool("clear", false, "清除密码，恢复成无密码状态")
	home, _ := os.UserHomeDir()
	cfg, err := parseConfig(fs, args, os.Getenv, home, false)
	if err != nil {
		return 2
	}
	if *fromStdin && *clearPW {
		fmt.Fprintln(stderr, "--stdin 和 --clear 不能同时使用")
		return 2
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if w := app.CheckPrivateDir(cfg.DataDir); w != "" {
		fmt.Fprintln(stderr, "警告：", w)
	}

	var pw string
	if !*clearPW {
		if pw, err = readNewPassword(ctx, stdin, stderr, *fromStdin); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := auth.CheckNew(pw); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer st.Close()
	if *clearPW {
		if err := st.ClearPassword(ctx); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "已清除网页密码，所有设备的登录已失效。")
		return 0
	}
	rec, err := auth.Hash(pw, 0)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	raw, err := rec.Encode()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := st.SetPassword(ctx, raw, ""); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "已设置网页密码，所有设备的登录已失效。")
	return 0
}

// readNewPassword 读新密码。once 时只读一行；否则提示输入两次并核对，标准输入是终端时关闭回显。
func readNewPassword(ctx context.Context, stdin io.Reader, prompt io.Writer, once bool) (string, error) {
	br := bufio.NewReader(stdin)
	if once {
		return readLine(ctx, br)
	}
	if f, ok := stdin.(*os.File); ok && isTerminal(f) {
		restore, err := echoOff()
		if err != nil {
			return "", fmt.Errorf("无法关闭终端回显（%v）；可改用：printf '%%s\\n' '新密码' | vinx-assistant password --stdin", err)
		}
		defer restore()
	}
	ask := func(label string) (string, error) {
		fmt.Fprint(prompt, label)
		s, err := readLine(ctx, br)
		fmt.Fprintln(prompt) // 回显关闭时回车不会换行
		return s, err
	}
	first, err := ask("新密码：")
	if err != nil {
		return "", err
	}
	second, err := ask("再输一次：")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("两次输入的密码不一致")
	}
	return first, nil
}

// readLine 读一行（去掉行尾换行）。读的时候按 Ctrl-C 能退出：信号被 NotifyContext 接住，这里看 ctx。
func readLine(ctx context.Context, br *bufio.Reader) (string, error) {
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := br.ReadString('\n')
		ch <- result{s, err}
	}()
	select {
	case <-ctx.Done():
		return "", errors.New("已取消")
	case r := <-ch:
		s := strings.TrimRight(r.s, "\r\n")
		if r.err != nil && (!errors.Is(r.err, io.EOF) || s == "") {
			return "", errors.New("没有读到密码")
		}
		return s, nil
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// echoOff 用 stty 关闭终端回显（不引入 x/term 依赖），返回恢复函数。只支持类 Unix 系统。
func echoOff() (func(), error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("Windows 不支持")
	}
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return nil, err
	}
	stty := func(arg string) error {
		c := exec.Command("stty", arg)
		c.Stdin = tty
		return c.Run()
	}
	if err := stty("-echo"); err != nil {
		tty.Close()
		return nil, err
	}
	return func() {
		stty("echo")
		tty.Close()
	}, nil
}
