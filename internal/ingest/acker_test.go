package ingest

import (
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
)

func TestAckerSingle(t *testing.T) {
	a := NewAcker(5*time.Second, 30*time.Second)
	t0 := clock.At(2026, 10, 1, 9, 0)
	a.Add(t0, "待办｜交发票")
	if _, ok := a.Due(t0.Add(4 * time.Second)); ok {
		t.Fatal("due too early")
	}
	text, ok := a.Due(t0.Add(5 * time.Second))
	if !ok || text != "✓ 已收：待办｜交发票" {
		t.Fatalf("text=%q ok=%v", text, ok)
	}
	if _, ok := a.Due(t0.Add(10 * time.Second)); ok {
		t.Fatal("must reset after due")
	}
}

func TestAckerBurstMergesAndCapsAt30s(t *testing.T) {
	a := NewAcker(5*time.Second, 30*time.Second)
	t0 := clock.At(2026, 10, 1, 9, 0)
	for i := 0; i < 8; i++ { // 0–28 秒内每 4 秒一条，静默永远不满 5 秒
		now := t0.Add(time.Duration(i*4) * time.Second)
		a.Add(now, "x")
		if _, ok := a.Due(now); ok {
			t.Fatalf("due at i=%d", i)
		}
	}
	text, ok := a.Due(t0.Add(30 * time.Second))
	if !ok || text != "✓ 已收 8 条" {
		t.Fatalf("text=%q ok=%v", text, ok)
	}
}
