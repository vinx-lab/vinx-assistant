package store

import (
	"context"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
)

func TestPasswordRecord(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	if v, err := st.PasswordRecord(ctx); err != nil || v != "" {
		t.Fatalf("初始 %q %v", v, err)
	}
	if err := st.SetPassword(ctx, `{"alg":"x"}`, ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.PasswordRecord(ctx); v != `{"alg":"x"}` {
		t.Fatalf("got %q", v)
	}
	// 密码记录不影响其他设置
	if _, err := st.LoadSettings(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearPassword(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.PasswordRecord(ctx); v != "" {
		t.Fatalf("清除后 %q", v)
	}
}

func TestSessions(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	now := clock.At(2026, 10, 1, 9, 0)
	exp := now.Add(30 * 24 * time.Hour)
	for _, h := range []string{"a", "b", "c"} {
		if err := st.CreateSession(ctx, h, now, exp); err != nil {
			t.Fatal(err)
		}
	}
	valid := func(h string, at time.Time) bool {
		t.Helper()
		ok, err := st.SessionValid(ctx, h, at)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	cases := []struct {
		name string
		h    string
		at   time.Time
		want bool
	}{
		{"有效", "a", now, true},
		{"不存在", "zz", now, false},
		{"到期前一秒", "b", exp.Add(-time.Second), true},
		{"到期", "b", exp, false},
	}
	for _, c := range cases {
		if got := valid(c.h, c.at); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
	// 过期的 b 查一次就被删掉了
	if n, _ := st.CountSessions(ctx); n != 2 {
		t.Fatalf("sessions = %d", n)
	}

	// 吊销单个
	if err := st.DeleteSession(ctx, "a"); err != nil || valid("a", now) {
		t.Fatalf("delete a: %v", err)
	}
	// 改密码：保留 c，其余吊销
	st.CreateSession(ctx, "d", now, exp)
	if err := st.SetPassword(ctx, "rec", "c"); err != nil {
		t.Fatal(err)
	}
	if !valid("c", now) || valid("d", now) {
		t.Fatal("SetPassword 应只保留 c")
	}
	// 新建会话时惰性清理过期的
	later := exp.Add(time.Hour)
	st.CreateSession(ctx, "old", now, now.Add(time.Minute))
	if err := st.CreateSession(ctx, "e", later, later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountSessions(ctx); n != 1 {
		t.Fatalf("清理后 sessions = %d", n)
	}
	// 全部吊销
	st.CreateSession(ctx, "f", later, later.Add(time.Hour))
	if err := st.DeleteAllSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountSessions(ctx); n != 0 {
		t.Fatalf("sessions = %d", n)
	}
	// 清除密码也吊销全部
	st.CreateSession(ctx, "g", later, later.Add(time.Hour))
	st.ClearPassword(ctx)
	if n, _ := st.CountSessions(ctx); n != 0 {
		t.Fatalf("ClearPassword 后 sessions = %d", n)
	}
}
