package batch

import (
	"context"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/llm/llmtest"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestDueSlot(t *testing.T) {
	times := []string{"08:00", "20:00"}
	cases := []struct {
		name string
		now  time.Time
		ts   []string
		last string
		slot string
		due  bool
	}{
		{"首次启动", clock.At(2026, 10, 1, 7, 59), times, "", "2026-09-30 20:00", true},
		{"到点", clock.At(2026, 10, 1, 8, 0), times, "2026-09-30 20:00", "2026-10-01 08:00", true},
		{"已跑过", clock.At(2026, 10, 1, 9, 0), times, "2026-10-01 08:00", "2026-10-01 08:00", false},
		{"跨零点", clock.At(2026, 10, 2, 1, 0), times, "2026-10-01 08:00", "2026-10-01 20:00", true},
		{"停机多次只补一次", clock.At(2026, 10, 3, 10, 0), times, "2026-10-01 08:00", "2026-10-03 08:00", true},
		{"非法格式忽略", clock.At(2026, 10, 1, 9, 0), []string{"25:00", "abc", " 8:30 "}, "", "2026-10-01 08:30", true},
		{"没有时间点", clock.At(2026, 10, 1, 9, 0), nil, "", "", false},
	}
	for _, c := range cases {
		slot, due := DueSlot(c.now, c.ts, c.last)
		if slot != c.slot || due != c.due {
			t.Errorf("%s: DueSlot = %q %v, want %q %v", c.name, slot, due, c.slot, c.due)
		}
	}
}

func TestNextSlot(t *testing.T) {
	times := []string{"20:00", "08:00"}
	if got, ok := NextSlot(clock.At(2026, 10, 1, 9, 0), times); !ok || !got.Equal(clock.At(2026, 10, 1, 20, 0)) {
		t.Errorf("next = %v", got)
	}
	if got, ok := NextSlot(clock.At(2026, 10, 1, 21, 0), times); !ok || !got.Equal(clock.At(2026, 10, 2, 8, 0)) {
		t.Errorf("next after last = %v", got)
	}
	if _, ok := NextSlot(clock.At(2026, 10, 1, 9, 0), []string{"x"}); ok {
		t.Error("invalid times must give no next slot")
	}
}

func TestSchedulerRunsOncePerSlot(t *testing.T) {
	e := newTEnv(t, func(s *model.Settings) { s.Schedule.BatchTimes = []string{"08:00"} })
	e.add(t, &model.Item{RawText: "a"})
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{{"id": 1, "category": "idea"}}}))
	s := &Scheduler{Runner: e.r, Store: e.st}
	ctx := context.Background()
	e.st.SetKV(ctx, "batch.last_slot", "2026-09-30 08:00")

	e.clk.Set(clock.At(2026, 10, 1, 8, 0))
	s.Tick(ctx, e.clk.Now())
	s.Wait()
	if len(e.llm.Requests()) != 1 {
		t.Fatalf("requests = %d", len(e.llm.Requests()))
	}
	s.Tick(ctx, e.clk.Now().Add(time.Minute))
	s.Wait()
	if v, _, _ := e.st.GetKV(ctx, "batch.last_slot"); v != "2026-10-01 08:00" {
		t.Fatalf("last_slot = %q", v)
	}
	e.clk.Set(clock.At(2026, 10, 2, 8, 0))
	s.Tick(ctx, e.clk.Now())
	s.Wait()
	if v, _, _ := e.st.GetKV(ctx, "batch.last_slot"); v != "2026-10-02 08:00" {
		t.Fatalf("next day last_slot = %q", v)
	}
}

func TestSchedulerFirstStartDoesNotRun(t *testing.T) {
	e := newTEnv(t, func(s *model.Settings) { s.Schedule.BatchTimes = []string{"08:00"} })
	e.add(t, &model.Item{RawText: "a"})
	s := &Scheduler{Runner: e.r, Store: e.st}
	ctx := context.Background()
	s.Tick(ctx, clock.At(2026, 10, 1, 9, 0))
	s.Wait()
	if n := len(e.llm.Requests()); n != 0 {
		t.Fatalf("first start must not run, requests = %d", n)
	}
	if v, _, _ := e.st.GetKV(ctx, "batch.last_slot"); v != "2026-10-01 08:00" {
		t.Fatalf("last_slot = %q", v)
	}
}
