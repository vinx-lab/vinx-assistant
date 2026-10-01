package auth

import (
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
)

const (
	// FailWindow 内累计失败 MaxFails 次后锁定 LockFor。
	FailWindow = 15 * time.Minute
	MaxFails   = 10
	LockFor    = 15 * time.Minute
)

// Limiter 是全站共用的密码失败计数（只有一个用户，按来源地址分开没有意义；挂在反向代理后面时来源也都一样）。
// 只在内存里：服务重启或密码记录变化后清零。
//
// 每次校验前用 Begin 预占一次失败（检查锁定和占位在同一把锁里），校验通过再用 Done 归还。
// 这样并发请求也绕不过上限：一个锁定周期里最多有 MaxFails 次尝试真正去校验密码。
type Limiter struct {
	Clock clock.Clock

	mu     sync.Mutex
	fails  []time.Time // 窗口内的失败，含正在校验的预占
	locked time.Time   // 锁定到这个时间；零值表示没锁
}

// Ticket 是一次预占。
type Ticket struct {
	at    time.Time
	locks bool // 这次预占触发了锁定
}

func NewLimiter(c clock.Clock) *Limiter {
	if c == nil {
		c = clock.Real{}
	}
	return &Limiter{Clock: c}
}

// lockedLocked 返回剩余锁定时间；锁定到期时清空计数。调用方持有 mu。
func (l *Limiter) lockedLocked(now time.Time) time.Duration {
	if l.locked.IsZero() {
		return 0
	}
	if d := l.locked.Sub(now); d > 0 {
		return d
	}
	l.locked, l.fails = time.Time{}, nil
	return 0
}

// Locked 返回剩余锁定时间；没锁时返回 0。
func (l *Limiter) Locked() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lockedLocked(l.Clock.Now())
}

// Begin 预占一次尝试。锁定中时返回剩余锁定时间（> 0），此时不得校验密码。
// 预占使窗口内的尝试数达到 MaxFails 时立即锁定，后来的请求直接被拒；这一次仍可校验。
func (l *Limiter) Begin() (Ticket, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Clock.Now()
	if d := l.lockedLocked(now); d > 0 {
		return Ticket{}, d
	}
	kept := l.fails[:0]
	for _, t := range l.fails {
		if now.Sub(t) < FailWindow {
			kept = append(kept, t)
		}
	}
	l.fails = append(kept, now)
	tk := Ticket{at: now}
	if len(l.fails) >= MaxFails {
		l.locked = now.Add(LockFor)
		tk.locks = true
	}
	return tk, 0
}

// Done 结束一次预占。ok（密码正确）时归还这次占位，若是它触发的锁定也一并解除；
// 失败时占位留着。返回 true 表示这次失败让账户进入了锁定（调用方记一条日志）。
func (l *Limiter) Done(tk Ticket, ok bool) bool {
	if !ok {
		return tk.locks
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, t := range l.fails {
		if t.Equal(tk.at) {
			l.fails = append(l.fails[:i], l.fails[i+1:]...)
			break
		}
	}
	if tk.locks {
		l.locked = time.Time{}
	}
	return false
}

// Reset 清空计数和锁定（密码被重置时）。
func (l *Limiter) Reset() {
	l.mu.Lock()
	l.fails, l.locked = nil, time.Time{}
	l.mu.Unlock()
}

// Minutes 把剩余时间换成向上取整的分钟数，给页面显示。
func Minutes(d time.Duration) int {
	return int((d + time.Minute - 1) / time.Minute)
}
