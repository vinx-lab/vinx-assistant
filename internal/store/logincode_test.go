package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
)

func TestLoginRequired(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	if on, err := st.LoginRequired(ctx); err != nil || on {
		t.Fatalf("默认应关闭：%v %v", on, err)
	}
	// 0004 起设密码不再自动打开
	st.SetPassword(ctx, "rec", "")
	if on, _ := st.LoginRequired(ctx); on {
		t.Fatal("设密码不应打开开关")
	}
	for _, v := range []bool{true, false, true} {
		if err := st.SetLoginRequired(ctx, v); err != nil {
			t.Fatal(err)
		}
		if on, _ := st.LoginRequired(ctx); on != v {
			t.Fatalf("want %v", v)
		}
	}
}

// 升级：迁移 0007 让已设密码的部署自动打开「需要登录」，没设密码的保持关闭，已有开关的不动。
func TestUpgradeTurnsOnLoginRequired(t *testing.T) {
	cases := []struct {
		name     string
		password bool
		existing string // 升级前已有的开关值；空表示没有
		want     bool
	}{
		{"已设密码", true, "", true},
		{"没设密码", false, "", false},
		{"已手动关闭", true, "0", false},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), "t.db")
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		// 模拟 0003 时代的库：0007 还没跑过、没有开关键
		st.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = '0007_login_required.sql'`)
		st.db.ExecContext(ctx, `DELETE FROM settings WHERE key = 'login_required'`)
		if c.password {
			st.SetPassword(ctx, `{"alg":"pbkdf2-sha256"}`, "")
		}
		if c.existing != "" {
			st.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('login_required', ?)`, c.existing)
		}
		st.Close()
		st, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if on, _ := st.LoginRequired(ctx); on != c.want {
			t.Errorf("%s: got %v", c.name, on)
		}
		st.Close()
	}
}

func TestLoginCodes(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	now := clock.At(2026, 10, 1, 9, 0)
	exp := now.Add(2 * time.Minute)

	if err := st.CreateLoginCode(ctx, "c1", "b1", now, exp); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.LoginCodeActive(ctx, "c1", now); !ok {
		t.Fatal("c1 应有效")
	}
	if ok, _ := st.LoginCodeMatches(ctx, "b1", "c1", now); !ok {
		t.Fatal("b1 的验证码应是 c1")
	}
	if ok, _ := st.LoginCodeMatches(ctx, "b2", "c1", now); ok {
		t.Fatal("别的浏览器不匹配")
	}
	poll := func(b string, at time.Time) LoginCodeState {
		t.Helper()
		s, err := st.PollLoginCode(ctx, b, at)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if poll("b1", now) != LoginCodePending || poll("nobody", now) != LoginCodeGone {
		t.Fatal("未确认应 pending，不认识的浏览器应 gone")
	}
	// 数字对不上不确认
	if ok, _ := st.ConfirmLoginCode(ctx, "c9", now); ok {
		t.Fatal("c9 不应匹配")
	}
	if ok, _ := st.ConfirmLoginCode(ctx, "c1", now.Add(time.Minute)); !ok {
		t.Fatal("c1 应确认")
	}
	// 只能确认一次
	if ok, _ := st.ConfirmLoginCode(ctx, "c1", now.Add(time.Minute)); ok {
		t.Fatal("重复确认")
	}
	// 绑定浏览器：别的浏览器拿不到
	if poll("b2", now.Add(time.Minute)) != LoginCodeGone {
		t.Fatal("别的浏览器不应拿到")
	}
	// 确认后过期一点也能换（宽限），换一次就作废
	if poll("b1", exp.Add(30*time.Second)) != LoginCodeConfirmed {
		t.Fatal("应换到会话")
	}
	if poll("b1", exp.Add(30*time.Second)) != LoginCodeGone {
		t.Fatal("换完应作废")
	}

	// 过期：未确认的到期作废，也不能再确认
	st.CreateLoginCode(ctx, "c2", "b2", now, exp)
	if ok, _ := st.ConfirmLoginCode(ctx, "c2", exp); ok {
		t.Fatal("过期后不应确认")
	}
	if poll("b2", exp) != LoginCodeGone {
		t.Fatal("过期应 gone")
	}

	// 同一浏览器重新取码，旧的作废
	st.CreateLoginCode(ctx, "c3", "b3", now, exp)
	st.CreateLoginCode(ctx, "c4", "b3", now, exp)
	if ok, _ := st.LoginCodeActive(ctx, "c3", now); ok {
		t.Fatal("同一浏览器的旧验证码应作废")
	}

	// 数量上限：最多 5 个，超出时最早的作废
	st.db.ExecContext(ctx, `DELETE FROM login_codes`)
	for i, b := range []string{"x1", "x2", "x3", "x4", "x5", "x6"} {
		st.CreateLoginCode(ctx, "k"+b, b, now.Add(time.Duration(i)*time.Second), exp)
	}
	if n, _ := st.CountLoginCodes(ctx); n != MaxActiveLoginCodes {
		t.Fatalf("count = %d", n)
	}
	if ok, _ := st.LoginCodeActive(ctx, "kx1", now); ok {
		t.Fatal("最早的应作废")
	}
	if ok, _ := st.LoginCodeActive(ctx, "kx6", now); !ok {
		t.Fatal("最新的应有效")
	}

	// 查询次数上限
	st.CreateLoginCode(ctx, "p", "bp", now, exp)
	for i := 0; i < MaxLoginCodePolls; i++ {
		if poll("bp", now) != LoginCodePending {
			t.Fatalf("第 %d 次查询", i+1)
		}
	}
	if poll("bp", now) != LoginCodeGone {
		t.Fatal("超过查询上限应作废")
	}
	if ok, _ := st.ConfirmLoginCode(ctx, "p", now); ok {
		t.Fatal("作废后不应确认")
	}
	st.CreateLoginCode(ctx, "d", "bd", now, exp)
	st.DeleteLoginCode(ctx, "bd")
	if poll("bd", now) != LoginCodeGone {
		t.Fatal("删除后应 gone")
	}
}
