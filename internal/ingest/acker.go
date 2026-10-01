package ingest

import (
	"fmt"
	"sync"
	"time"
)

// Acker 合并回执：最后一条之后静默 quiet 就发，从第一条算起最多等 maxWait。
type Acker struct {
	quiet, maxWait time.Duration
	mu             sync.Mutex
	labels         []string
	first, last    time.Time
}

func NewAcker(quiet, maxWait time.Duration) *Acker {
	return &Acker{quiet: quiet, maxWait: maxWait}
}

func (a *Acker) Add(now time.Time, label string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.labels) == 0 {
		a.first = now
	}
	a.labels = append(a.labels, label)
	a.last = now
}

// Due 到点时返回要发的文本并清空。
func (a *Acker) Due(now time.Time) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.labels) == 0 {
		return "", false
	}
	if now.Sub(a.last) < a.quiet && now.Sub(a.first) < a.maxWait {
		return "", false
	}
	var text string
	if len(a.labels) == 1 {
		text = "✓ 已收：" + a.labels[0]
	} else {
		text = fmt.Sprintf("✓ 已收 %d 条", len(a.labels))
	}
	a.labels = nil
	return text, true
}
