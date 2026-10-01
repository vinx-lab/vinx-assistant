package remind

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

func countState(t *testing.T, st *store.Store, state string) int {
	t.Helper()
	rows, err := st.RemindersByState(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// 停机 50 分钟后启动：多个到期窗口合成一条消息，只调用一次 Send。
func TestOutageSendsOneMessage(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 9, 30))
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 30)) // 摘要
	for i, m := range []int{5, 15, 25, 35} {
		addTodo(t, st, "事项"+string(rune('A'+i)), clock.At(2026, 10, 1, 14, m), true)
	}
	s.tickAt(clk, clock.At(2026, 10, 1, 14, 0))
	before := n.calls
	s.tickAt(clk, clock.At(2026, 10, 1, 14, 50))
	if n.calls-before != 1 {
		t.Fatalf("sends = %d, want 1", n.calls-before)
	}
	got := n.sent[len(n.sent)-1]
	for _, want := range []string{"#1 事项A", "#2 事项B", "#3 事项C", "#4 事项D"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %q", want, got)
		}
	}
	if strings.Count(got, HeaderDue) != 1 {
		t.Fatalf("header count: %q", got)
	}
	if c := countState(t, st, "sent"); c != 5 {
		t.Fatalf("sent rows = %d", c)
	}
}

// 失败即停：一次 Tick 只尝试一次，全部标 deferred。
func TestOutageFailureStopsAfterOneAttempt(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 8, 0))
	addTodo(t, st, "甲", clock.At(2026, 10, 1, 8, 20), true)
	addTodo(t, st, "乙", clock.At(2026, 10, 1, 8, 45), true)
	n.fail = errors.New("ret=-2")
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 5)) // 摘要 + 两个窗口
	if n.calls != 1 {
		t.Fatalf("calls = %d", n.calls)
	}
	if c := countState(t, st, "deferred"); c != 3 {
		t.Fatalf("deferred = %d", c)
	}
}

// 摘要和到期同一次 Tick：合成一条，摘要在前；摘要里已列出的条目不在到期段重复。
func TestDigestAndDueCombinedDedup(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 8, 0))
	addTodo(t, st, "早上的", clock.At(2026, 10, 1, 8, 50), true)
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 0))
	if n.calls != 1 {
		t.Fatalf("calls = %d", n.calls)
	}
	got := n.sent[0]
	if !strings.HasPrefix(got, "📋 今日摘要") || strings.Count(got, "#1 早上的") != 1 || strings.Contains(got, HeaderDue) {
		t.Fatalf("text = %q", got)
	}
	if c := countState(t, st, "sent"); c != 2 {
		t.Fatalf("sent rows = %d", c)
	}
}

// 发送后 ctx 被取消（停机）：状态仍要记下。
func TestMarkSurvivesCancel(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 9, 0))
	ctx, cancel := context.WithCancel(context.Background())
	n.onSend = cancel
	clk.Set(clock.At(2026, 10, 1, 9, 0))
	s.Tick(ctx, clock.At(2026, 10, 1, 9, 0))
	if c := countState(t, st, "sent"); c != 1 {
		t.Fatalf("sent = %d", c)
	}
}

// 遗留的 pending 行（上次在发送与记状态之间退出）超过 5 分钟转为 deferred，不会再新建一份。
func TestStalePendingBecomesDeferred(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 9, 0))
	ctx := context.Background()
	old := &store.Reminder{Kind: "digest", ScheduledAt: clock.At(2026, 10, 1, 9, 0), Body: "x"}
	if err := st.InsertReminder(ctx, old); err != nil {
		t.Fatal(err)
	}
	clk.Set(clock.At(2026, 10, 1, 9, 3))
	id := addTodo(t, st, "新", clock.At(2026, 10, 1, 12, 0), true)
	fresh := &store.Reminder{Kind: "due", ItemID: id, ScheduledAt: clock.At(2026, 10, 1, 12, 0), Body: "y"}
	if err := st.InsertReminder(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 6))
	if n.calls != 0 {
		t.Fatalf("digest re-sent by Tick: %v", n.sent)
	}
	if c := countState(t, st, "deferred"); c != 1 {
		t.Fatalf("deferred = %d", c)
	}
	if c := countState(t, st, "pending"); c != 1 {
		t.Fatalf("fresh pending should stay: %d", c)
	}
	text, commit, err := s.TakeDeferred(ctx)
	if err != nil || !strings.HasPrefix(text, "📋 今日摘要") {
		t.Fatalf("text=%q err=%v", text, err)
	}
	commit(ctx, true)

	// TakeDeferred 开头也处理遗留 pending
	clk.Set(clock.At(2026, 10, 1, 9, 20))
	text, commit, _ = s.TakeDeferred(ctx)
	if !strings.HasPrefix(text, HeaderResend) || !strings.Contains(text, "#1 新") {
		t.Fatalf("text = %q", text)
	}
	commit(ctx, true)
}

// 补发时摘要和到期一起：摘要里已列出的到期条目不重复，随摘要记为已发。
func TestResendDedupAgainstDigest(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 8, 0))
	addTodo(t, st, "交材料", clock.At(2026, 10, 1, 8, 30), true)
	n.fail = errors.New("ret=-2")
	s.tickAt(clk, clock.At(2026, 10, 1, 8, 30)) // 到期失败
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 0))  // 摘要失败
	n.fail = nil
	clk.Set(clock.At(2026, 10, 1, 9, 10))
	text, commit, _ := s.TakeDeferred(context.Background())
	if strings.Count(text, "#1 交材料") != 1 || strings.Contains(text, HeaderResend) {
		t.Fatalf("text = %q", text)
	}
	commit(context.Background(), true)
	if c := countState(t, st, "sent"); c != 2 {
		t.Fatalf("sent = %d", c)
	}
}

// 合并消息整体按字数上限截断，页脚保留。
func TestCombinedFitsCap(t *testing.T) {
	now := clock.At(2026, 10, 1, 9, 0)
	long := strings.Repeat("很长的待办内容", 5)
	var over, due []model.Item
	for i := 0; i < 60; i++ {
		d := clock.At(2026, 9, 28, 0, 0)
		over = append(over, model.Item{ID: int64(i + 1), RawText: long, DueAt: &d})
		d2 := clock.At(2026, 10, 1, 8, 0)
		due = append(due, model.Item{ID: int64(i + 100), RawText: long, DueAt: &d2, DueHasTime: true})
	}
	text := buildMessage(now, &store.DigestData{Overdue: over}, due, HeaderResend)
	if n := utf8.RuneCountInString(text); n > MaxRunes {
		t.Fatalf("runes = %d", n)
	}
	if !strings.HasSuffix(text, digestFooter) || !strings.Contains(text, "…还有") {
		t.Fatalf("text = %q", text)
	}
}

// 昨天的摘要在今天摘要时刻之后补发：记到今天的时刻，Tick 不再发第二份。
func TestOldDigestResentCountsForToday(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 9, 30, 9, 0))
	n.fail = errors.New("ret=-2")
	s.tickAt(clk, clock.At(2026, 9, 30, 9, 0))
	n.fail = nil
	// 10-01 09:00 这一刻 Tick 还没轮到，先来了一条消息
	clk.Set(clock.At(2026, 10, 1, 9, 0))
	text, commit, _ := s.TakeDeferred(context.Background())
	if !strings.HasPrefix(text, "📋 今日摘要 10-01") {
		t.Fatalf("text = %q", text)
	}
	commit(context.Background(), true)
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 1))
	if n.calls != 1 {
		t.Fatalf("second digest sent: %v", n.sent)
	}
	if has, _ := st.HasDigest(context.Background(), clock.At(2026, 10, 1, 9, 0)); !has {
		t.Fatal("today's slot not recorded")
	}
	if c := countState(t, st, "dropped"); c != 1 {
		t.Fatalf("dropped = %d", c)
	}
}

// 还没到今天摘要时刻：旧摘要直接丢弃，只补到期条目。
func TestOldDigestBeforeDigestTimeDropped(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 9, 30, 9, 0))
	n.fail = errors.New("ret=-2")
	s.tickAt(clk, clock.At(2026, 9, 30, 9, 0))
	addTodo(t, st, "交材料", clock.At(2026, 10, 1, 8, 0), true)
	s.tickAt(clk, clock.At(2026, 10, 1, 8, 0))
	n.fail = nil
	clk.Set(clock.At(2026, 10, 1, 8, 30))
	text, commit, _ := s.TakeDeferred(context.Background())
	if strings.Contains(text, "📋") || !strings.HasPrefix(text, HeaderResend) {
		t.Fatalf("text = %q", text)
	}
	commit(context.Background(), true)
	if c := countState(t, st, "dropped"); c != 1 {
		t.Fatalf("dropped = %d", c)
	}
}

type fakeCtx struct{ at time.Time }

func (f fakeCtx) Context(ctx context.Context) (string, time.Time, bool, error) {
	return "tok", f.at, true, nil
}

// context_token 太旧：不调用 Send，直接 deferred；阈值可配。
func TestStaleContextSkipsSend(t *testing.T) {
	s, st, clk, n := newSvc(t, clock.At(2026, 10, 1, 9, 0))
	s.Session = fakeCtx{at: clock.At(2026, 9, 30, 17, 0)} // 16 小时前
	s.tickAt(clk, clock.At(2026, 10, 1, 9, 0))
	if n.calls != 0 {
		t.Fatal("Send called with stale token")
	}
	if c := countState(t, st, "deferred"); c != 1 {
		t.Fatalf("deferred = %d", c)
	}

	s2, st2, clk2, n2 := newSvc(t, clock.At(2026, 10, 1, 9, 0))
	s2.Session = fakeCtx{at: clock.At(2026, 9, 30, 17, 0)}
	s2.MaxContextAge = 20 * time.Hour
	s2.tickAt(clk2, clock.At(2026, 10, 1, 9, 0))
	if n2.calls != 1 || countState(t, st2, "sent") != 1 {
		t.Fatal("threshold field ignored")
	}
}
