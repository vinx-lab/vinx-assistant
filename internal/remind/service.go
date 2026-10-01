package remind

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/notify"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

const window = 10 * time.Minute

type Service struct {
	Store     *store.Store
	Notifier  notify.Notifier
	Clock     clock.Clock
	Log       *slog.Logger
	LateLimit time.Duration // 超过这么久才发现到期的，不再单独推送

	mu       sync.Mutex
	inflight map[int64]bool
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Service) late() time.Duration {
	if s.LateLimit <= 0 {
		return time.Hour
	}
	return s.LateLimit
}

func (s *Service) now() time.Time {
	if s.Clock == nil {
		return clock.Real{}.Now()
	}
	return s.Clock.Now().In(clock.Zone)
}

// digestAt 今天（上海时间）的摘要时刻；设置不合法时用 09:00。
func digestAt(now time.Time, hhmm string) time.Time {
	now = now.In(clock.Zone)
	t, err := time.Parse("15:04", hhmm)
	h, m := 9, 0
	if err == nil {
		h, m = t.Hour(), t.Minute()
	}
	return time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
}

// Tick 实现 app.Ticker。
func (s *Service) Tick(ctx context.Context, now time.Time) {
	now = now.In(clock.Zone)
	if err := s.tickDigest(ctx, now); err != nil {
		s.log().Error("每日摘要失败", "err", err)
	}
	if err := s.tickDue(ctx, now); err != nil {
		s.log().Error("到期提醒失败", "err", err)
	}
}

func (s *Service) tickDigest(ctx context.Context, now time.Time) error {
	set, err := s.Store.LoadSettings(ctx)
	if err != nil {
		return err
	}
	at := digestAt(now, set.Schedule.DigestTime)
	if now.Before(at) {
		return nil
	}
	if has, err := s.Store.HasDigest(ctx, at); err != nil || has {
		return err
	}
	data, err := s.Store.DigestData(ctx, now)
	if err != nil {
		return err
	}
	r := &store.Reminder{Kind: "digest", ScheduledAt: at, Body: BuildDigest(now, data)}
	if err := s.Store.InsertReminder(ctx, r); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return nil
		}
		return err
	}
	return s.send(ctx, []int64{r.ID}, r.Body, now)
}

func (s *Service) tickDue(ctx context.Context, now time.Time) error {
	until := time.Unix((now.Unix()/600+1)*600, 0).In(now.Location())
	cands, err := s.Store.DueCandidates(ctx, until)
	if err != nil {
		return err
	}
	groups := map[int64][]model.Item{}
	var keys []int64
	for _, it := range cands {
		due := *it.DueAt
		if now.Sub(due) > s.late() {
			err := s.Store.InsertReminder(ctx, &store.Reminder{Kind: "due", ItemID: it.ID, ScheduledAt: due, State: "dropped", LastError: "到期太久才发现，不单独推送"})
			if err != nil && !errors.Is(err, store.ErrDuplicate) {
				return err
			}
			continue
		}
		k := due.Unix() / int64(window/time.Second)
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], it)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		g := groups[k]
		fire := false
		for _, it := range g {
			if !it.DueAt.After(now) {
				fire = true
			}
		}
		if !fire {
			continue
		}
		body := BuildDue(g, now, HeaderDue)
		var ids []int64
		for _, it := range g {
			r := &store.Reminder{Kind: "due", ItemID: it.ID, ScheduledAt: *it.DueAt, Body: body}
			if err := s.Store.InsertReminder(ctx, r); err != nil {
				if errors.Is(err, store.ErrDuplicate) {
					continue
				}
				return err
			}
			ids = append(ids, r.ID)
		}
		if len(ids) > 0 {
			if err := s.send(ctx, ids, body, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// send 发一次；失败标 deferred，等下一次入站消息时随回执补发。
func (s *Service) send(ctx context.Context, ids []int64, body string, now time.Time) error {
	if err := s.Notifier.Send(ctx, body); err != nil {
		s.log().Warn("提醒发送失败，等下次收到消息时补发", "err", err)
		return s.Store.MarkReminders(ctx, ids, "deferred", err.Error(), now)
	}
	return s.Store.MarkReminders(ctx, ids, "sent", "", now)
}

// TakeDeferred 实现 ingest.Deferred：取出待补发的提醒，合成一段文字。
func (s *Service) TakeDeferred(ctx context.Context) (string, func(context.Context, bool) error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight == nil {
		s.inflight = map[int64]bool{}
	}
	now := s.now()
	rows, err := s.Store.RemindersByState(ctx, "deferred")
	if err != nil {
		return "", nil, err
	}
	var digests []store.Reminder
	var drop, keep []int64
	var dueItems []model.Item
	for _, r := range rows {
		if s.inflight[r.ID] {
			continue
		}
		if r.Kind == "digest" {
			digests = append(digests, r)
			continue
		}
		it, err := s.Store.GetItem(ctx, r.ItemID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", nil, err
		}
		if err != nil || it.Category != model.CatTodo || it.Status != model.StatusOpen || it.DueAt == nil || it.DueAt.Unix() != r.ScheduledAt.Unix() {
			drop = append(drop, r.ID)
			continue
		}
		dueItems = append(dueItems, *it)
		keep = append(keep, r.ID)
	}
	var parts []string
	if len(digests) > 0 {
		for _, r := range digests[:len(digests)-1] {
			drop = append(drop, r.ID)
		}
		data, err := s.Store.DigestData(ctx, now)
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, BuildDigest(now, data))
		keep = append(keep, digests[len(digests)-1].ID)
	}
	if len(dueItems) > 0 {
		parts = append(parts, BuildDue(dueItems, now, HeaderResend))
	}
	if err := s.Store.MarkReminders(ctx, drop, "dropped", "补发前已失效", now); err != nil {
		return "", nil, err
	}
	if len(keep) == 0 {
		return "", nil, nil
	}
	for _, id := range keep {
		s.inflight[id] = true
	}
	commit := func(ctx context.Context, sent bool) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, id := range keep {
			delete(s.inflight, id)
		}
		if sent {
			return s.Store.MarkReminders(ctx, keep, "sent", "", s.now())
		}
		return s.Store.MarkReminders(ctx, keep, "deferred", "补发失败", s.now())
	}
	return strings.Join(parts, "\n\n"), commit, nil
}
