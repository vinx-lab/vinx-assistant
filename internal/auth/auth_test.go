package auth

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
)

func TestHashVerify(t *testing.T) {
	r, err := Hash("correct horse", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if r.Alg != Algorithm || r.Iter != 1000 || len(r.Salt) != 16 || len(r.Hash) != 32 {
		t.Fatalf("record = %+v", r)
	}
	r2, _ := Hash("correct horse", 1000)
	if bytes.Equal(r.Salt, r2.Salt) || bytes.Equal(r.Hash, r2.Hash) {
		t.Fatal("两次哈希的盐应不同")
	}
	raw, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "correct horse") {
		t.Fatal("记录里不应有明文")
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		pw   string
		want bool
	}{
		{"correct horse", true},
		{"correct horsE", false},
		{"", false},
		{"correct horse ", false},
		{strings.Repeat("x", 5000), false},
	} {
		if v := got.Verify(c.pw); v != c.want {
			t.Errorf("Verify(%q) = %v", c.pw, v)
		}
	}
	// 记录被篡改或不认识的算法
	for _, bad := range []Record{
		{Alg: "md5", Iter: 1000, Salt: r.Salt, Hash: r.Hash},
		{Alg: Algorithm, Iter: 0, Salt: r.Salt, Hash: r.Hash},
		{Alg: Algorithm, Iter: maxIterations + 1, Salt: r.Salt, Hash: r.Hash},
		{Alg: Algorithm, Iter: 1000, Hash: r.Hash},
	} {
		if bad.Verify("correct horse") {
			t.Errorf("%+v 不应通过", bad)
		}
	}
	for _, raw := range []string{"", "{", `{"alg":""}`, `{"alg":"pbkdf2-sha256"}`} {
		if _, err := Decode(raw); err == nil {
			t.Errorf("Decode(%q) 应报错", raw)
		}
	}
}

func TestDefaultIterations(t *testing.T) {
	if DefaultIterations < 600000 {
		t.Fatalf("迭代次数 %d 太少", DefaultIterations)
	}
	r, err := Hash("12345678", 0)
	if err != nil || r.Iter != DefaultIterations || !r.Verify("12345678") {
		t.Fatalf("%v %d", err, r.Iter)
	}
}

func TestCheckNew(t *testing.T) {
	for pw, ok := range map[string]bool{"": false, "1234567": false, "12345678": true, "密码密码密码密码": true, "密码密码密码密": false, strings.Repeat("a", MaxLen): true, strings.Repeat("a", MaxLen+1): false} {
		if err := CheckNew(pw); (err == nil) != ok {
			t.Errorf("CheckNew(%q) = %v", pw, err)
		}
	}
}

func TestToken(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if a == b || len(a) != 43 {
		t.Fatalf("%q %q", a, b)
	}
	if h := TokenHash(a); len(h) != 64 || h == TokenHash(b) || h != TokenHash(a) || strings.Contains(h, a) {
		t.Fatalf("hash %q", h)
	}
}

// fail 预占一次并记为失败，返回是否触发锁定；锁定中返回 false。
func fail(l *Limiter) (locked bool, rejected bool) {
	tk, d := l.Begin()
	if d > 0 {
		return false, true
	}
	return l.Done(tk, false), false
}

func TestLimiter(t *testing.T) {
	clk := clock.NewFake(clock.At(2026, 10, 1, 9, 0))
	l := NewLimiter(clk)

	// 9 次不锁
	for i := 0; i < MaxFails-1; i++ {
		if lk, _ := fail(l); lk {
			t.Fatalf("第 %d 次就锁了", i+1)
		}
	}
	if l.Locked() != 0 {
		t.Fatal("不应锁定")
	}
	// 窗口滑过去：早先的失败不再计数
	clk.Advance(FailWindow)
	if lk, _ := fail(l); lk {
		t.Fatal("窗口外的失败不应计数")
	}
	for i := 0; i < MaxFails-2; i++ {
		if lk, _ := fail(l); lk {
			t.Fatalf("窗口内第 %d 次就锁了", i+2)
		}
	}
	if lk, _ := fail(l); !lk {
		t.Fatal("窗口内第 10 次应锁定")
	}
	if d := l.Locked(); d != LockFor || Minutes(d) != 15 {
		t.Fatalf("locked %v", d)
	}
	if _, rej := fail(l); !rej {
		t.Fatal("锁定中应直接拒绝")
	}
	clk.Advance(14*time.Minute + 30*time.Second)
	if d := l.Locked(); Minutes(d) != 1 {
		t.Fatalf("剩余 %v", d)
	}
	clk.Advance(30 * time.Second)
	if l.Locked() != 0 {
		t.Fatal("15 分钟后应解锁")
	}
	// 解锁后重新计数
	if lk, rej := fail(l); lk || rej {
		t.Fatal("解锁后第一次失败不应锁")
	}
	for i := 0; i < MaxFails-1; i++ {
		fail(l)
	}
	if l.Locked() == 0 {
		t.Fatal("应再次锁定")
	}
	l.Reset()
	if l.Locked() != 0 {
		t.Fatal("Reset 后应解锁")
	}
}

func TestLimiterReservation(t *testing.T) {
	clk := clock.NewFake(clock.At(2026, 10, 1, 9, 0))
	l := NewLimiter(clk)

	// 并发：同时预占，最多 MaxFails 个拿到名额，其余直接拒绝
	var tickets []Ticket
	for i := 0; i < MaxFails*3; i++ {
		if tk, d := l.Begin(); d == 0 {
			tickets = append(tickets, tk)
		}
	}
	if len(tickets) != MaxFails {
		t.Fatalf("拿到名额 %d 个", len(tickets))
	}
	// 前 9 个失败，第 10 个（触发锁定的那个）密码正确：归还占位、解除锁定
	for _, tk := range tickets[:MaxFails-1] {
		if l.Done(tk, false) {
			t.Fatal("非触发锁定的失败不应报告锁定")
		}
	}
	l.Done(tickets[MaxFails-1], true)
	if l.Locked() != 0 {
		t.Fatal("触发锁定的那次密码正确时应解除锁定")
	}
	// 之前 9 次失败还在窗口里：下一次失败就锁定
	if lk, _ := fail(l); !lk {
		t.Fatal("第 10 次失败应锁定")
	}

	// 成功只归还自己的占位，不清掉别的失败
	l.Reset()
	a, _ := l.Begin()
	b, _ := l.Begin()
	l.Done(a, true)
	l.Done(b, false)
	for i := 0; i < MaxFails-2; i++ {
		if lk, _ := fail(l); lk {
			t.Fatalf("第 %d 次就锁了", i+2)
		}
	}
	if lk, _ := fail(l); !lk {
		t.Fatal("累计 10 次失败应锁定")
	}
}
