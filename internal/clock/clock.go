// Package clock 提供可注入的时钟。所有取当前时间的地方都通过 Clock，测试用 Fake。
package clock

import (
	"sync"
	"time"
)

// Zone 固定为上海时间。上海没有夏令时，用固定偏移就不依赖系统的 tzdata。
var Zone = time.FixedZone("CST", 8*3600)

type Clock interface {
	Now() time.Time
}

type Real struct{}

func (Real) Now() time.Time { return time.Now().In(Zone) }

type Fake struct {
	mu sync.Mutex
	t  time.Time
}

func NewFake(t time.Time) *Fake { return &Fake{t: t.In(Zone)} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	f.t = t.In(Zone)
	f.mu.Unlock()
}

func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

// DayString 返回上海时间的日期 "2006-01-02"。
func DayString(t time.Time) string { return t.In(Zone).Format(time.DateOnly) }

// At 构造上海时间的某一分钟，测试和日期计算用。
func At(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, Zone)
}
