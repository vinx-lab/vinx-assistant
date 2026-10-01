package remind

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/notify"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

const (
	window = 10 * time.Minute
	// stalePending 建行后这么久还是 pending，说明上次进程在发送和记状态之间退出了，转为补发。
	stalePending = 5 * time.Minute
	markTimeout  = 5 * time.Second
)

// ContextSource 提供最近一次入站消息的 context_token 及其时间（*session.Session 实现）。
type ContextSource interface {
	Context(ctx context.Context) (string, time.Time, bool, error)
}

type Service struct {
	Store     *store.Store
	Notifier  notify.Notifier
	Clock     clock.Clock
	Log       *slog.Logger
	LateLimit time.Duration // 超过这么久才发现到期的，不再单独推送；默认 1 小时

	// Session 可选：给出 context_token 的时间。token 超过 MaxContextAge（默认 15 小时）
	// 时发送注定失败，直接标 deferred，不白白消耗一次请求（微信对连续请求有限流）。
	Session       ContextSource
	MaxContextAge time.Duration

	mu sync.Mutex
	// inflight 是已被 TakeDeferred 取出、还没 commit 的提醒。只在内存里：
	// 取出后、commit 前进程崩溃，这些行仍是 deferred，重启后最多再补发一次（可接受）。
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

func (s *Service) maxContextAge() time.Duration {
	if s.MaxContextAge <= 0 {
		return 15 * time.Hour
	}
	return s.MaxContextAge
}

func (s *Service) now() time.Time {
	if s.Clock == nil {
		return clock.Real{}.Now()
	}
	return s.Clock.Now().In(clock.Zone)
}

// mark 记状态。用不随调用方取消的 ctx：停机时 ctx 已取消，消息却可能已经发出，状态必须记下。
func (s *Service) mark(ctx context.Context, ids []int64, state, lastErr string, at time.Time) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()
	return s.Store.MarkReminders(ctx, ids, state, lastErr, at)
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

// promoteStale 把遗留的 pending 行（上次在建行与记状态之间退出）转为 deferred：
// 宁可偶尔重复一次，也不静默丢失。
func (s *Service) promoteStale(ctx context.Context, now time.Time) error {
	rows, err := s.Store.RemindersByState(ctx, "pending")
	if err != nil {
		return err
	}
	var ids []int64
	for _, r := range rows {
		if now.Sub(r.CreatedAt) > stalePending {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) > 0 {
		s.log().Warn("发现遗留的未发送提醒，转为补发", "n", len(ids))
	}
	return s.mark(ctx, ids, "deferred", "上次发送中断", now)
}

// Tick 实现 app.Ticker。每次最多发一条消息：到点的摘要和所有到期窗口合在一起，
// 停机恢复后也不会连发多条触发限流；发送失败全部标 deferred，不再继续发。
func (s *Service) Tick(ctx context.Context, now time.Time) {
	now = now.In(clock.Zone)
	if err := s.promoteStale(ctx, now); err != nil {
		s.log().Error("处理遗留提醒失败", "err", err)
	}
	var ids []int64
	digest, digestID, err := s.tickDigest(ctx, now)
	if err != nil {
		s.log().Error("每日摘要失败", "err", err)
	}
	if digest != nil {
		ids = append(ids, digestID)
	}
	due, dueIDs, err := s.tickDue(ctx, now, listed(digest))
	if err != nil {
		s.log().Error("到期提醒失败", "err", err)
	}
	ids = append(ids, dueIDs...)
	if len(ids) == 0 {
		return
	}
	if err := s.send(ctx, ids, buildMessage(now, digest, due, HeaderDue), now); err != nil {
		s.log().Error("记录提醒状态失败", "err", err)
	}
}

// listed 摘要里已经列出的条目编号。
func listed(d *store.DigestData) map[int64]bool {
	out := map[int64]bool{}
	if d == nil {
		return out
	}
	for _, sec := range [][]model.Item{d.Overdue, d.DueToday, d.DueTomorrow} {
		for _, it := range sec {
			out[it.ID] = true
		}
	}
	return out
}

// tickDigest 到点且今天还没建过摘要时，建 pending 行并返回摘要数据。
func (s *Service) tickDigest(ctx context.Context, now time.Time) (*store.DigestData, int64, error) {
	set, err := s.Store.LoadSettings(ctx)
	if err != nil {
		return nil, 0, err
	}
	at := digestAt(now, set.Schedule.DigestTime)
	if now.Before(at) {
		return nil, 0, nil
	}
	if has, err := s.Store.HasDigest(ctx, at); err != nil || has {
		return nil, 0, err
	}
	data, err := s.Store.DigestData(ctx, now)
	if err != nil {
		return nil, 0, err
	}
	r := &store.Reminder{Kind: "digest", ScheduledAt: at, Body: BuildDigest(now, data)}
	if err := s.Store.InsertReminder(ctx, r); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	return &data, r.ID, nil
}

// tickDue 建到期提醒行，返回要放进消息的条目（摘要里已列出的不重复）和全部行 id。
func (s *Service) tickDue(ctx context.Context, now time.Time, skip map[int64]bool) ([]model.Item, []int64, error) {
	until := time.Unix((now.Unix()/600+1)*600, 0).In(clock.Zone)
	cands, err := s.Store.DueCandidates(ctx, until)
	if err != nil {
		return nil, nil, err
	}
	groups := map[int64][]model.Item{}
	var keys []int64
	for _, it := range cands {
		due := *it.DueAt
		if now.Sub(due) > s.late() {
			err := s.Store.InsertReminder(ctx, &store.Reminder{Kind: "due", ItemID: it.ID, ScheduledAt: due, State: "dropped", LastError: "到期太久才发现，不单独推送"})
			if err != nil && !errors.Is(err, store.ErrDuplicate) {
				return nil, nil, err
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
	var items []model.Item
	var ids []int64
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
		for _, it := range g {
			r := &store.Reminder{Kind: "due", ItemID: it.ID, ScheduledAt: *it.DueAt, Body: body}
			if err := s.Store.InsertReminder(ctx, r); err != nil {
				if errors.Is(err, store.ErrDuplicate) {
					continue
				}
				return items, ids, err
			}
			ids = append(ids, r.ID)
			if !skip[it.ID] {
				items = append(items, it)
			}
		}
	}
	return items, ids, nil
}

// send 发一次；失败标 deferred，等下一次入站消息时随回执补发。
func (s *Service) send(ctx context.Context, ids []int64, body string, now time.Time) error {
	if reason := s.contextStale(ctx, now); reason != "" {
		s.log().Warn("context_token 不可用，提醒等下次收到消息时补发", "reason", reason)
		return s.mark(ctx, ids, "deferred", reason, now)
	}
	if err := s.Notifier.Send(ctx, body); err != nil {
		s.log().Warn("提醒发送失败，等下次收到消息时补发", "err", err)
		return s.mark(ctx, ids, "deferred", err.Error(), now)
	}
	return s.mark(ctx, ids, "sent", "", now)
}

// contextStale 返回非空原因表示 context_token 已知不可用、发送注定失败。
func (s *Service) contextStale(ctx context.Context, now time.Time) string {
	if s.Session == nil {
		return ""
	}
	_, at, ok, err := s.Session.Context(ctx)
	switch {
	case err != nil:
		return "" // 读不到就照常发，由 Send 判断
	case !ok:
		return notify.ErrNoContext.Error()
	case !at.IsZero() && now.Sub(at) > s.maxContextAge():
		return "context_token 超过 " + s.maxContextAge().String() + " 未刷新"
	}
	return ""
}

// TakeDeferred 实现 ingest.Deferred：取出待补发的提醒，合成一段文字。
// 摘要只补最新一份，并记到今天的摘要时刻，避免一天两份；还没到今天的摘要时刻则只补到期条目。
// 补发的到期条目若已在重新生成的摘要里列出，不再重复列，随摘要一起记为已发。
func (s *Service) TakeDeferred(ctx context.Context) (string, func(context.Context, bool) error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight == nil {
		s.inflight = map[int64]bool{}
	}
	now := s.now()
	if err := s.promoteStale(ctx, now); err != nil {
		return "", nil, err
	}
	rows, err := s.Store.RemindersByState(ctx, "deferred")
	if err != nil {
		return "", nil, err
	}
	var digests []store.Reminder
	var drop, keep []int64
	type dueRow struct {
		id   int64
		item model.Item
	}
	var dues []dueRow
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
		dues = append(dues, dueRow{r.ID, *it})
	}

	var digest *store.DigestData
	if len(digests) > 0 {
		for _, r := range digests[:len(digests)-1] {
			drop = append(drop, r.ID)
		}
		latest := digests[len(digests)-1]
		id, ok, err := s.digestSlot(ctx, now, latest)
		if err != nil {
			return "", nil, err
		}
		if ok {
			data, err := s.Store.DigestData(ctx, now)
			if err != nil {
				return "", nil, err
			}
			digest = &data
			keep = append(keep, id)
		}
		if !ok || id != latest.ID {
			drop = append(drop, latest.ID)
		}
	}
	skip := listed(digest)
	var dueItems []model.Item
	for _, d := range dues {
		keep = append(keep, d.id)
		if !skip[d.item.ID] {
			dueItems = append(dueItems, d.item)
		}
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
	text := buildMessage(now, digest, dueItems, HeaderResend)
	commit := func(ctx context.Context, sent bool) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, id := range keep {
			delete(s.inflight, id)
		}
		if sent {
			return s.mark(ctx, keep, "sent", "", s.now())
		}
		return s.mark(ctx, keep, "deferred", "补发失败", s.now())
	}
	return text, commit, nil
}

// digestSlot 决定待补发的最新摘要记在哪一行：
// 已到今天的摘要时刻——就是今天这份则用它；否则今天还没有摘要行时新建一行占住今天的时刻
// （Tick 不会再发第二份），今天已有摘要则不补。还没到今天的摘要时刻——不补（ok=false）。
func (s *Service) digestSlot(ctx context.Context, now time.Time, latest store.Reminder) (int64, bool, error) {
	set, err := s.Store.LoadSettings(ctx)
	if err != nil {
		return 0, false, err
	}
	at := digestAt(now, set.Schedule.DigestTime)
	if now.Before(at) {
		return 0, false, nil
	}
	if latest.ScheduledAt.Equal(at) {
		return latest.ID, true, nil
	}
	if has, err := s.Store.HasDigest(ctx, at); err != nil || has {
		return 0, false, err
	}
	r := &store.Reminder{Kind: "digest", ScheduledAt: at, Body: "（补发 " + clock.DayString(latest.ScheduledAt) + " 的摘要）"}
	if err := s.Store.InsertReminder(ctx, r); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return r.ID, true, nil
}
