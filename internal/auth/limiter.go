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
// 只在内存里：服务重启或命令行改密码后清零。
type Limiter struct {
	Clock clock.Clock

	mu     sync.Mutex
	fails  []time.Time
	locked time.Time // 锁定到这个时间；零值表示没锁
}

func NewLimiter(c clock.Clock) *Limiter {
	if c == nil {
		c = clock.Real{}
	}
	return &Limiter{Clock: c}
}

// Locked 返回剩余锁定时间；没锁时返回 0。
func (l *Limiter) Locked() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locked.IsZero() {
		return 0
	}
	if d := l.locked.Sub(l.Clock.Now()); d > 0 {
		return d
	}
	l.locked = time.Time{}
	return 0
}

// Fail 记一次失败；这次失败触发锁定时返回 true。
func (l *Limiter) Fail() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Clock.Now()
	kept := l.fails[:0]
	for _, t := range l.fails {
		if now.Sub(t) < FailWindow {
			kept = append(kept, t)
		}
	}
	l.fails = append(kept, now)
	if len(l.fails) >= MaxFails {
		l.fails = nil
		l.locked = now.Add(LockFor)
		return true
	}
	return false
}

// Reset 清空计数和锁定（登录成功、密码被重置时）。
func (l *Limiter) Reset() {
	l.mu.Lock()
	l.fails, l.locked = nil, time.Time{}
	l.mu.Unlock()
}

// Minutes 把剩余时间换成向上取整的分钟数，给页面显示。
func Minutes(d time.Duration) int {
	return int((d + time.Minute - 1) / time.Minute)
}
