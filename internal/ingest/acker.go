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
	return a.takeLocked(), true
}

// Flush 忽略静默期，立即返回已积累的回执并清空（关机收尾用）。
func (a *Acker) Flush() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.labels) == 0 {
		return "", false
	}
	return a.takeLocked(), true
}

// takeLocked 生成回执文本并清空；调用方持锁且 labels 非空。
func (a *Acker) takeLocked() string {
	var text string
	if len(a.labels) == 1 {
		text = "✓ 已收：" + a.labels[0]
	} else {
		text = fmt.Sprintf("✓ 已收 %d 条", len(a.labels))
	}
	a.labels = nil
	return text
}
