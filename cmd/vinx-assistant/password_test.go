package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/auth"
	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

func TestPasswordCommand(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	runPW := func(stdin string, args ...string) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code := cmdPassword(ctx, append([]string{"--data", dir}, args...), strings.NewReader(stdin), &out, &errb)
		return code, out.String(), errb.String()
	}
	record := func() string {
		t.Helper()
		st, err := store.Open(filepath.Join(dir, "vinx-assistant.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		v, err := st.PasswordRecord(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	cases := []struct {
		name  string
		stdin string
		args  []string
		code  int
		msg   string
	}{
		{"空行", "\n", []string{"--stdin"}, 1, "不能为空"},
		{"空输入", "", []string{"--stdin"}, 1, "没有读到密码"},
		{"两次不一致", "password-one\npassword-two\n", nil, 1, "不一致"},
		{"参数冲突", "", []string{"--stdin", "--clear"}, 2, "只能用一个"},
		{"参数冲突 2", "", []string{"--clear", "--no-login"}, 2, "只能用一个"},
	}
	for _, c := range cases {
		code, _, errOut := runPW(c.stdin, c.args...)
		if code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%s: code %d stderr %q", c.name, code, errOut)
		}
	}

	// 交互方式（标准输入不是终端时逐行读）：两次一致
	if code, out, errOut := runPW("first password\nfirst password\n"); code != 0 || !strings.Contains(out, "已设置") || !strings.Contains(errOut, "再输一次") {
		t.Fatalf("交互：%d %q %q", code, out, errOut)
	}
	rec, err := auth.Decode(record())
	if err != nil || !rec.Verify("first password") || rec.Iter < auth.DefaultIterations {
		t.Fatalf("记录：%v iter=%d", err, rec.Iter)
	}

	// 先放一个会话，--stdin 重置后应被吊销
	st, err := store.Open(filepath.Join(dir, "vinx-assistant.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st.CreateSession(ctx, "h", now, now.Add(time.Hour))
	st.Close()
	code, out, _ := runPW("second password\r\n", "--stdin")
	if code != 0 || strings.Contains(out, "second password") {
		t.Fatalf("--stdin：%d %q", code, out)
	}
	if rec, _ := auth.Decode(record()); !rec.Verify("second password") {
		t.Fatal("--stdin 没生效（应去掉行尾 \\r\\n）")
	}
	st, _ = store.Open(filepath.Join(dir, "vinx-assistant.db"))
	if n, _ := st.CountSessions(ctx); n != 0 {
		t.Fatalf("sessions = %d", n)
	}
	st.Close()

	if code, out, _ := runPW("", "--clear"); code != 0 || !strings.Contains(out, "已清除") {
		t.Fatalf("--clear：%d %q", code, out)
	}
	if v := record(); v != "" {
		t.Fatalf("清除后 %q", v)
	}
}

// --clear 在微信不可用时同时关闭「需要登录」；--no-login 只关开关；单个字符的密码可以设置。
func TestPasswordLoginSwitch(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	runPW := func(stdin string, args ...string) (int, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code := cmdPassword(ctx, append([]string{"--data", dir}, args...), strings.NewReader(stdin), &out, &errb)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, errb.String())
		}
		return code, out.String()
	}
	open := func() *store.Store {
		st, err := store.Open(filepath.Join(dir, "vinx-assistant.db"))
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	required := func() bool {
		st := open()
		defer st.Close()
		on, _ := st.LoginRequired(ctx)
		return on
	}
	setRequired := func(on bool) {
		st := open()
		st.SetLoginRequired(ctx, on)
		st.Close()
	}

	runPW("x\n", "--stdin")
	st := open()
	raw, _ := st.PasswordRecord(ctx)
	st.Close()
	if rec, _ := auth.Decode(raw); !rec.Verify("x") {
		t.Fatal("单个字符的密码应能设置")
	}

	setRequired(true)
	if _, out := runPW("", "--no-login"); !strings.Contains(out, "已关闭") || required() {
		t.Fatalf("--no-login：%q", out)
	}
	st = open()
	if v, _ := st.PasswordRecord(ctx); v == "" {
		t.Fatal("--no-login 不应动密码")
	}
	st.Close()

	// 没有微信凭证：--clear 同时关闭开关
	setRequired(true)
	if _, out := runPW("", "--clear"); !strings.Contains(out, "同时关闭") || required() {
		t.Fatalf("--clear 无微信：%q", out)
	}

	// 微信可用：--clear 不动开关
	st = open()
	session.New(st, nil, nil).SaveCred(ctx, ilink.Cred{BotToken: "t", BotID: "b", UserID: "u", BaseURL: "http://127.0.0.1:1"})
	st.SetLoginRequired(ctx, true)
	st.Close()
	runPW("y\n", "--stdin")
	if _, out := runPW("", "--clear"); strings.Contains(out, "同时关闭") || !required() {
		t.Fatalf("--clear 有微信：%q", out)
	}
}
