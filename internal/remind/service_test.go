package remind

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

type fakeNotifier struct {
	mu   sync.Mutex
	sent []string
	fail error
}

func (f *fakeNotifier) Send(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, text)
	return nil
}

func (f *fakeNotifier) texts(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sent {
		if strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}
	return out
}

func newSvc(t *testing.T, start time.Time) (*Service, *store.Store, *clock.Fake, *fakeNotifier) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := clock.NewFake(start)
	st.SetClock(clk)
	n := &fakeNotifier{}
	return &Service{Store: st, Notifier: n, Clock: clk}, st, clk, n
}

func addTodo(t *testing.T, st *store.Store, text string, due time.Time, hasTime bool) int64 {
	t.Helper()
	id, err := st.InsertItem(context.Background(), &model.Item{MsgID: text, RawText: text, Category: model.CatTodo, CategoryBy: model.ByPrefix, DueAt: &due, DueHasTime: hasTime})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (s *Service) tickAt(clk *clock.Fake, t time.Time) {
	clk.Set(t)
	s.Tick(context.Background(), t)
}

func TestDigestOncePerDay(t *testing.T) {
	s, _, clk, n := newSvc(t, clock.At(2026, 10, 1, 8, 0))
	s.tickAt(clk, clock.At(2026, 10, 1, 8, 59))
	if len(n.texts("📋")) != 0 {
		t.Fatal("digest before 09:00")
	}
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 0))
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 30))
	s.tickAt(clk, clock.At(2026, 10, 1, 23, 0))
	if got := n.texts("📋"); len(got) != 1 || !strings.HasPrefix(got[0], "📋 今日摘要 10-01 周四") {
		t.Fatalf("digests = %v", got)
	}
	s.tickAt(clk, clock.At(2026, 10, 2, 9, 0))
	if got := n.texts("📋"); len(got) != 2 {
		t.Fatalf("second day digests = %d", len(got))
	}
}

func TestDigestTimeFromSettings(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 8, 0))
	set, _ := st.LoadSettings(context.Background())
	set.Schedule.DigestTime = "07:30"
	st.SaveSettings(context.Background(), set)
	s.tickAt(clk, clock.At(2026, 10, 1, 7, 31))
	if len(n.texts("📋")) != 1 {
		t.Fatal("digest time setting ignored")
	}
}

func TestDueMergedInWindow(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 9, 30)) // 摘要已过，先发掉
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 30))
	a := addTodo(t, st, "交材料", clock.At(2026, 10, 1, 15, 0), true)
	b := addTodo(t, st, "打电话", clock.At(2026, 10, 1, 15, 5), true)
	addTodo(t, st, "只有日期", clock.At(2026, 10, 1, 0, 0), false)
	s.tickAt(clk, clock.At(2026, 10, 1, 14, 59))
	if len(n.texts(HeaderDue)) != 0 {
		t.Fatal("reminded early")
	}
	s.tickAt(clk, clock.At(2026, 10, 1, 15, 0))
	got := n.texts(HeaderDue)
	if len(got) != 1 || !strings.Contains(got[0], "#1 交材料") || !strings.Contains(got[0], "#2 打电话") {
		t.Fatalf("due = %v", got)
	}
	s.tickAt(clk, clock.At(2026, 10, 1, 15, 5))
	s.tickAt(clk, clock.At(2026, 10, 1, 16, 0))
	if len(n.texts(HeaderDue)) != 1 {
		t.Fatal("reminded twice")
	}
	_, _ = a, b
	sent, _ := st.RemindersByState(context.Background(), "sent")
	if len(sent) != 3 { // 1 摘要 + 2 条到期
		t.Fatalf("sent rows = %d", len(sent))
	}
}

func TestRestartAfterDowntime(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 9, 30, 8, 0))
	addTodo(t, st, "昨天的", clock.At(2026, 9, 30, 15, 0), true)
	addTodo(t, st, "今早的", clock.At(2026, 10, 1, 8, 30), true)
	// 停机一整天，10 月 1 日 10:00 才启动
	s.tickAt(clk, clock.At(2026, 10, 1, 10, 0))
	if got := n.texts(HeaderDue); len(got) != 0 {
		t.Fatalf("stale reminders sent: %v", got)
	}
	digests := n.texts("📋")
	if len(digests) != 1 || !strings.Contains(digests[0], "逾期（2）") {
		t.Fatalf("digest = %v", digests)
	}
	dropped, _ := st.RemindersByState(context.Background(), "dropped")
	if len(dropped) != 2 {
		t.Fatalf("dropped = %d", len(dropped))
	}
	s.tickAt(clk, clock.At(2026, 10, 1, 10, 1))
	if len(n.sent) != 1 {
		t.Fatalf("extra sends: %v", n.sent)
	}
}

func TestSendFailureDeferredThenTaken(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 9, 30))
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 30)) // 摘要
	addTodo(t, st, "交材料", clock.At(2026, 10, 1, 15, 0), true)
	n.fail = errors.New("ilink sendmessage: ret=-2")
	s.tickAt(clk, clock.At(2026, 10, 1, 15, 0))
	def, _ := st.RemindersByState(context.Background(), "deferred")
	if len(def) != 1 || def[0].LastError == "" {
		t.Fatalf("deferred = %+v", def)
	}
	n.fail = nil
	clk.Set(clock.At(2026, 10, 1, 16, 0))
	text, commit, err := s.TakeDeferred(context.Background())
	if err != nil || !strings.HasPrefix(text, HeaderResend) || !strings.Contains(text, "#1 交材料") {
		t.Fatalf("text=%q err=%v", text, err)
	}
	// 还没 commit 时，并发的第二次取不到同一批
	if again, _, _ := s.TakeDeferred(context.Background()); again != "" {
		t.Fatalf("inflight taken twice: %q", again)
	}
	commit(context.Background(), true)
	if left, _ := st.RemindersByState(context.Background(), "deferred"); len(left) != 0 {
		t.Fatalf("left = %+v", left)
	}
	if again, c, _ := s.TakeDeferred(context.Background()); again != "" || c != nil {
		t.Fatal("nothing should remain")
	}
}

func TestCommitFailureKeepsDeferred(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 9, 30))
	n.fail = errors.New("ret=-2")
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 30))
	_, commit, _ := s.TakeDeferred(context.Background())
	commit(context.Background(), false)
	def, _ := st.RemindersByState(context.Background(), "deferred")
	if len(def) != 1 {
		t.Fatalf("deferred = %d", len(def))
	}
	if text, _, _ := s.TakeDeferred(context.Background()); text == "" {
		t.Fatal("must be retakeable after failed commit")
	}
}

func TestTakeDeferredKeepsLatestDigestAndDropsStaleDue(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 9, 30, 9, 0))
	n.fail = errors.New("ret=-2")
	s.tickAt(clk, clock.At(2026, 9, 30, 9, 0)) // 9-30 摘要失败
	id := addTodo(t, st, "交材料", clock.At(2026, 10, 1, 8, 0), true)
	s.tickAt(clk, clock.At(2026, 10, 1, 8, 0)) // 到期提醒失败
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 0)) // 10-01 摘要失败
	it, _ := st.GetItem(context.Background(), id)
	it.Status = model.StatusDone
	st.UpdateItem(context.Background(), it)

	n.fail = nil
	clk.Set(clock.At(2026, 10, 1, 12, 0))
	text, commit, _ := s.TakeDeferred(context.Background())
	if strings.Count(text, "📋 今日摘要") != 1 || strings.Contains(text, HeaderResend) {
		t.Fatalf("text = %q", text)
	}
	commit(context.Background(), true)
	dropped, _ := st.RemindersByState(context.Background(), "dropped")
	if len(dropped) != 2 { // 旧摘要 + 已完成条目的提醒
		t.Fatalf("dropped = %d", len(dropped))
	}
}

func TestDigestAt(t *testing.T) {
	now := clock.At(2026, 10, 1, 12, 0)
	if got := digestAt(now, "07:45"); !got.Equal(clock.At(2026, 10, 1, 7, 45)) {
		t.Fatalf("got %v", got)
	}
	if got := digestAt(now, "25:99"); !got.Equal(clock.At(2026, 10, 1, 9, 0)) {
		t.Fatalf("invalid fallback %v", got)
	}
	// 跨日：UTC 时间 2026-09-30 17:00 是上海 10-01 01:00，摘要时刻应是 10-01 09:00。
	if got := digestAt(clock.At(2026, 10, 1, 1, 0).UTC(), "09:00"); !got.Equal(clock.At(2026, 10, 1, 9, 0)) {
		t.Fatalf("zone %v", got)
	}
}

func TestTickNormalizesZone(t *testing.T) {
	// 调用方可能传入 UTC 时间：摘要时刻必须按上海时间算。
	s, _, clk, n := newSvc(t, clock.At(2026, 10, 1, 8, 0))
	utc := clock.At(2026, 10, 1, 8, 59).UTC()
	clk.Set(utc)
	s.Tick(context.Background(), utc)
	if len(n.texts("📋")) != 0 {
		t.Fatal("digest sent before 09:00 Shanghai")
	}
	utc = clock.At(2026, 10, 1, 9, 0).UTC()
	clk.Set(utc)
	s.Tick(context.Background(), utc)
	if got := n.texts("📋"); len(got) != 1 || !strings.HasPrefix(got[0], "📋 今日摘要 10-01 周四") {
		t.Fatalf("digests = %v", got)
	}
}

func TestLateLimitConfigurable(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 9, 30))
	s.LateLimit = 10 * time.Minute
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 30)) // 摘要
	addTodo(t, st, "交材料", clock.At(2026, 10, 1, 15, 0), true)
	s.tickAt(clk, clock.At(2026, 10, 1, 15, 20))
	if len(n.texts(HeaderDue)) != 0 {
		t.Fatal("late reminder should be dropped")
	}
	if dropped, _ := st.RemindersByState(context.Background(), "dropped"); len(dropped) != 1 {
		t.Fatalf("dropped = %d", len(dropped))
	}
}

func TestTakeDeferredConcurrent(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 9, 30))
	n.fail = errors.New("ret=-2")
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 30)) // 摘要失败
	addTodo(t, st, "交材料", clock.At(2026, 10, 1, 15, 0), true)
	s.tickAt(clk, clock.At(2026, 10, 1, 15, 0)) // 到期提醒失败
	n.fail = nil

	var wg sync.WaitGroup
	var mu sync.Mutex
	var commits []func(context.Context, bool) error
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, commit, err := s.TakeDeferred(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			if text != "" {
				mu.Lock()
				commits = append(commits, commit)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(commits) != 1 {
		t.Fatalf("taken %d times, want 1", len(commits))
	}
	if err := commits[0](context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if sent, _ := st.RemindersByState(context.Background(), "sent"); len(sent) != 2 {
		t.Fatalf("sent = %d", len(sent))
	}
}
