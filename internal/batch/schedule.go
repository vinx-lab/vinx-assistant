package batch

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

const keyLastSlot = "batch.last_slot"

func parseHM(s string) (int, int, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

func slotsAround(now time.Time, times []string, days ...int) []time.Time {
	now = now.In(clock.Zone)
	var out []time.Time
	for _, d := range days {
		day := now.AddDate(0, 0, d)
		for _, s := range times {
			h, m, ok := parseHM(s)
			if !ok {
				continue
			}
			out = append(out, time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, clock.Zone))
		}
	}
	return out
}

// DueSlot 判断是否到了整理时间：取昨天和今天不晚于 now 的最晚时间点，比 lastSlot 新就到点。
func DueSlot(now time.Time, times []string, lastSlot string) (string, bool) {
	var latest time.Time
	for _, t := range slotsAround(now, times, -1, 0) {
		if !t.After(now) && t.After(latest) {
			latest = t
		}
	}
	if latest.IsZero() {
		return "", false
	}
	slot := latest.Format(timeLayout)
	return slot, slot > lastSlot
}

// NextSlot 返回 now 之后最近的整理时间。
func NextSlot(now time.Time, times []string) (time.Time, bool) {
	var next time.Time
	for _, t := range slotsAround(now, times, 0, 1) {
		if t.After(now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	return next, !next.IsZero()
}

// Scheduler 按设置里的整理时间触发批次，实现 app.Ticker。批次在后台运行，不阻塞其他定时任务。
type Scheduler struct {
	Runner *Runner
	Store  *store.Store
	Log    *slog.Logger
	wg     sync.WaitGroup
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Scheduler) Tick(ctx context.Context, now time.Time) {
	settings, err := s.Store.LoadSettings(ctx)
	if err != nil {
		s.log().Error("读取设置失败", "err", err)
		return
	}
	last, _, err := s.Store.GetKV(ctx, keyLastSlot)
	if err != nil {
		s.log().Error("读取上次整理时间失败", "err", err)
		return
	}
	slot, due := DueSlot(now, settings.Schedule.BatchTimes, last)
	if !due {
		return
	}
	if last == "" {
		// 第一次启动：只把最近一个已过的时间点记为已处理，不立刻整理，避免用户没留意就产生 AI 费用。
		if err := s.Store.SetKV(ctx, keyLastSlot, slot); err != nil {
			s.log().Error("初始化整理时间点失败", "err", err)
		}
		s.log().Info("首次启动，跳过已过的整理时间点", "slot", slot)
		return
	}
	// 先记下这个时间点，避免批次出错时每 30 秒重跑一次。
	if err := s.Store.SetKV(ctx, keyLastSlot, slot); err != nil {
		s.log().Error("保存整理时间点失败", "err", err)
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		rep, err := s.Runner.Run(ctx)
		if err != nil {
			LogRunError(s.log(), "定时整理", err, "slot", slot)
			return
		}
		s.log().Info("定时整理", "slot", slot, "report", rep.String())
	}()
}

// Wait 等后台批次结束（测试和退出时用）。
func (s *Scheduler) Wait() { s.wg.Wait() }

// LogRunError 记录 Run 的错误：取消（服务退出）和租约被接管是正常终止，只打 Info。
func LogRunError(log *slog.Logger, what string, err error, args ...any) {
	if errors.Is(err, context.Canceled) || errors.Is(err, errLeaseLost) {
		log.Info(what+"已中止", append(args, "reason", err.Error())...)
		return
	}
	log.Error(what+"失败", append(args, "err", err)...)
}
