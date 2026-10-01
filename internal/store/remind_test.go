package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func addTodo(t *testing.T, st *Store, msg string, due *time.Time, hasTime bool) int64 {
	t.Helper()
	id, err := st.InsertItem(context.Background(), &model.Item{MsgID: msg, RawText: msg, Category: model.CatTodo, CategoryBy: model.ByPrefix, DueAt: due, DueHasTime: hasTime})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func tp(t time.Time) *time.Time { return &t }

func TestReminderInsertDedupAndMark(t *testing.T) {
	st, fc := openTest(t)
	ctx := context.Background()
	due := clock.At(2026, 10, 1, 15, 0)
	id := addTodo(t, st, "a", &due, true)

	r := &Reminder{Kind: "due", ItemID: id, ScheduledAt: due, Body: "x"}
	if err := st.InsertReminder(ctx, r); err != nil || r.ID == 0 {
		t.Fatalf("insert: %v id=%d", err, r.ID)
	}
	if err := st.InsertReminder(ctx, &Reminder{Kind: "due", ItemID: id, ScheduledAt: due}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("dup err = %v", err)
	}
	digestAt := clock.At(2026, 10, 1, 9, 0)
	if has, _ := st.HasDigest(ctx, digestAt); has {
		t.Fatal("digest before insert")
	}
	if err := st.InsertReminder(ctx, &Reminder{Kind: "digest", ScheduledAt: digestAt}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertReminder(ctx, &Reminder{Kind: "digest", ScheduledAt: digestAt}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("digest dup err = %v", err)
	}
	if has, _ := st.HasDigest(ctx, digestAt); !has {
		t.Fatal("digest not found")
	}

	if err := st.MarkReminders(ctx, []int64{r.ID}, "deferred", "ret=-2", fc.Now()); err != nil {
		t.Fatal(err)
	}
	def, _ := st.RemindersByState(ctx, "deferred")
	if len(def) != 1 || def[0].Attempts != 1 || def[0].LastError != "ret=-2" || def[0].ItemID != id || def[0].SentAt != nil {
		t.Fatalf("deferred = %+v", def)
	}
	st.MarkReminders(ctx, []int64{r.ID}, "sent", "", fc.Now())
	sent, _ := st.RemindersByState(ctx, "sent")
	if len(sent) != 1 || sent[0].SentAt == nil || sent[0].Attempts != 2 {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestDueCandidates(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	d1 := clock.At(2026, 10, 1, 15, 0)
	d2 := clock.At(2026, 10, 1, 15, 5)
	a := addTodo(t, st, "a", &d1, true)
	b := addTodo(t, st, "b", &d2, true)
	addTodo(t, st, "c", tp(clock.At(2026, 10, 1, 0, 0)), false) // 只有日期：不进候选
	addTodo(t, st, "d", nil, false)
	done := addTodo(t, st, "e", &d1, true)
	it, _ := st.GetItem(ctx, done)
	it.Status = model.StatusDone
	st.UpdateItem(ctx, it)

	got, err := st.DueCandidates(ctx, clock.At(2026, 10, 1, 15, 10))
	if err != nil || len(got) != 2 || got[0].ID != a || got[1].ID != b {
		t.Fatalf("candidates = %+v err=%v", got, err)
	}
	st.InsertReminder(ctx, &Reminder{Kind: "due", ItemID: a, ScheduledAt: d1})
	got, _ = st.DueCandidates(ctx, clock.At(2026, 10, 1, 15, 10))
	if len(got) != 1 || got[0].ID != b {
		t.Fatalf("after remind = %+v", got)
	}
	// 改期后旧提醒不再挡住新的截止时间
	ia, _ := st.GetItem(ctx, a)
	nd := clock.At(2026, 10, 1, 15, 8)
	ia.DueAt = &nd
	st.UpdateItem(ctx, ia)
	got, _ = st.DueCandidates(ctx, clock.At(2026, 10, 1, 15, 10))
	if len(got) != 2 {
		t.Fatalf("rescheduled item not candidate again: %+v", got)
	}
}

func TestOpenTodosOrder(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	n := addTodo(t, st, "nodue", nil, false)
	late := addTodo(t, st, "late", tp(clock.At(2026, 10, 9, 0, 0)), false)
	early := addTodo(t, st, "early", tp(clock.At(2026, 10, 2, 10, 0)), true)
	got, _ := st.OpenTodos(ctx, 10)
	if len(got) != 3 || got[0].ID != early || got[1].ID != late || got[2].ID != n {
		t.Fatalf("order = %v", ids(got))
	}
	if got, _ := st.OpenTodos(ctx, 2); len(got) != 2 {
		t.Fatalf("limit ignored: %d", len(got))
	}
}

func ids(items []model.Item) []int64 {
	var out []int64
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func TestDigestData(t *testing.T) {
	st, fc := openTest(t) // 2026-10-01 09:00
	ctx := context.Background()
	overdueDate := addTodo(t, st, "od1", tp(clock.At(2026, 9, 30, 0, 0)), false)
	overdueTime := addTodo(t, st, "od2", tp(clock.At(2026, 10, 1, 8, 0)), true)
	todayDate := addTodo(t, st, "td1", tp(clock.At(2026, 10, 1, 0, 0)), false)
	todayTime := addTodo(t, st, "td2", tp(clock.At(2026, 10, 1, 15, 0)), true)
	tomorrow := addTodo(t, st, "tm", tp(clock.At(2026, 10, 2, 10, 0)), true)
	addTodo(t, st, "later", tp(clock.At(2026, 10, 5, 0, 0)), false)

	for i, p := range []model.Priority{model.PriorityLow, model.PriorityHigh, "", model.PriorityMedium} {
		id, _ := st.InsertItem(ctx, &model.Item{MsgID: "r" + string(rune('a'+i)), Category: model.CatResearch, Priority: p})
		_ = id
	}
	// 昨天收的一条
	fc.Set(clock.At(2026, 9, 30, 12, 0))
	st.InsertItem(ctx, &model.Item{MsgID: "y"})
	fc.Set(clock.At(2026, 10, 1, 9, 0))

	d, err := st.DigestData(ctx, fc.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(d.Overdue); len(got) != 2 || got[0] != overdueDate || got[1] != overdueTime {
		t.Fatalf("overdue = %v", got)
	}
	if got := ids(d.DueToday); len(got) != 2 || got[0] != todayDate || got[1] != todayTime {
		t.Fatalf("today = %v", got)
	}
	if got := ids(d.DueTomorrow); len(got) != 1 || got[0] != tomorrow {
		t.Fatalf("tomorrow = %v", got)
	}
	if d.ResearchBacklog != 4 || len(d.TopResearch) != 3 || d.TopResearch[0].Priority != model.PriorityHigh || d.TopResearch[1].Priority != model.PriorityMedium {
		t.Fatalf("research = %d %+v", d.ResearchBacklog, d.TopResearch)
	}
	if d.NewYesterday != 1 {
		t.Fatalf("new yesterday = %d", d.NewYesterday)
	}
}

func TestActions(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	if _, err := st.LastAction(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	id := addTodo(t, st, "a", nil, false)
	a1 := &Action{Command: "完成 1", ItemID: id, Before: `{"status":"open"}`, After: `{"status":"done"}`}
	a2 := &Action{Command: "推迟 1 明天", ItemID: id, Before: `{}`, After: `{}`}
	st.InsertAction(ctx, a1)
	st.InsertAction(ctx, a2)
	last, _ := st.LastAction(ctx)
	if last.ID != a2.ID {
		t.Fatalf("last = %+v", last)
	}
	st.MarkUndone(ctx, a2.ID)
	last, _ = st.LastAction(ctx)
	if last.ID != a1.ID || last.Before != `{"status":"open"}` || last.ItemID != id {
		t.Fatalf("after undo = %+v", last)
	}
}

func TestActionMsgIDDedup(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	if _, err := st.ActionByMsgID(ctx, "m1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	a := &Action{MsgID: "m1", Command: "完成 1", Before: `{}`, After: `{}`}
	if err := st.InsertAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAction(ctx, &Action{MsgID: "m1", Command: "完成 1"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("dup err = %v", err)
	}
	// 无 msg_id 的记录不互相冲突
	if err := st.InsertAction(ctx, &Action{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAction(ctx, &Action{Command: "y"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ActionByMsgID(ctx, "m1")
	if err != nil || got.ID != a.ID || got.Command != "完成 1" {
		t.Fatalf("got %+v err=%v", got, err)
	}
	st.MarkUndone(ctx, a.ID)
	if got, err := st.ActionByMsgID(ctx, "m1"); err != nil || !got.Undone {
		t.Fatalf("undone lookup %+v %v", got, err)
	}
}
