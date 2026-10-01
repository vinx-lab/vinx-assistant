package command

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/ingest"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

type fakeTr struct {
	out   Translation
	err   error
	calls int
	text  string
	todos []model.Item
}

func (f *fakeTr) Translate(ctx context.Context, text string, todos []model.Item, now time.Time) (Translation, error) {
	f.calls++
	f.text, f.todos = text, todos
	return f.out, f.err
}

func newHandler(t *testing.T, tr Translator) (*Handler, *store.Store, *clock.Fake) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := clock.NewFake(clock.At(2026, 10, 1, 9, 0))
	st.SetClock(clk)
	h := &Handler{Store: st, Clock: clk}
	if tr != nil {
		h.Translator = func(context.Context) (Translator, error) { return tr, nil }
	}
	return h, st, clk
}

func todo(t *testing.T, st *store.Store, text string, due *time.Time, hasTime bool) int64 {
	t.Helper()
	id, err := st.InsertItem(context.Background(), &model.Item{MsgID: text, RawText: text, Category: model.CatTodo, CategoryBy: model.ByPrefix, DueAt: due, DueHasTime: hasTime})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

var msgSeq int

func nextMsg() string { msgSeq++; return fmt.Sprintf("m%d", msgSeq) }

func handle(t *testing.T, h *Handler, text, ref string) (string, bool) {
	t.Helper()
	reply, handled, err := h.Handle(context.Background(), ingest.CommandInput{Text: text, RefText: ref, HasRef: ref != "", MsgID: nextMsg()})
	if err != nil {
		t.Fatal(err)
	}
	return reply, handled
}

func TestDoneAndUndo(t *testing.T) {
	h, st, _ := newHandler(t, nil)
	id := todo(t, st, "交发票", nil, false)
	reply, handled := handle(t, h, "完成 1", "")
	if !handled || reply != "✓ 已完成 #1 交发票" {
		t.Fatalf("reply=%q handled=%v", reply, handled)
	}
	it, _ := st.GetItem(context.Background(), id)
	if it.Status != model.StatusDone {
		t.Fatalf("status = %s", it.Status)
	}
	reply, _ = handle(t, h, "撤销", "")
	if !strings.Contains(reply, "已撤销") || !strings.Contains(reply, "#1") {
		t.Fatalf("undo reply = %q", reply)
	}
	it, _ = st.GetItem(context.Background(), id)
	if it.Status != model.StatusOpen {
		t.Fatalf("after undo status = %s", it.Status)
	}
	reply, _ = handle(t, h, "撤销", "")
	if reply != "没有可以撤销的操作。" {
		t.Fatalf("second undo = %q", reply)
	}
}

func TestPostponeAndUndoRestoresDue(t *testing.T) {
	h, st, _ := newHandler(t, nil)
	due := clock.At(2026, 10, 1, 15, 0)
	id := todo(t, st, "交材料", &due, true)
	reply, _ := handle(t, h, "推迟 1 明天", "")
	if !strings.Contains(reply, "10-02 周五 15:00") {
		t.Fatalf("reply = %q", reply)
	}
	handle(t, h, "撤销", "")
	it, _ := st.GetItem(context.Background(), id)
	if it.DueAt == nil || !it.DueAt.Equal(due) || !it.DueHasTime {
		t.Fatalf("due not restored: %+v", it)
	}
}

func TestNoChangeIsNotRecorded(t *testing.T) {
	h, st, _ := newHandler(t, nil)
	todo(t, st, "a", nil, false)
	handle(t, h, "完成 1", "")
	reply, _ := handle(t, h, "完成 1", "")
	if !strings.Contains(reply, "已经是") {
		t.Fatalf("reply = %q", reply)
	}
	handle(t, h, "撤销", "") // 应撤销第一次完成
	it, _ := st.GetItem(context.Background(), 1)
	if it.Status != model.StatusOpen {
		t.Fatalf("status = %s", it.Status)
	}
}

func TestMissingItemAndDateError(t *testing.T) {
	h, _, _ := newHandler(t, nil)
	if reply, handled := handle(t, h, "完成 99", ""); !handled || reply != "没有 #99。" {
		t.Fatalf("reply = %q", reply)
	}
	if reply, _ := handle(t, h, "改到 1 02-30", ""); !strings.HasPrefix(reply, "日期不对：02-30") {
		t.Fatalf("reply = %q", reply)
	}
}

func TestList(t *testing.T) {
	h, st, _ := newHandler(t, nil)
	if reply, _ := handle(t, h, "列表", ""); reply != "没有未完成的待办。" {
		t.Fatalf("empty list = %q", reply)
	}
	todo(t, st, "无截止", nil, false)
	d := clock.At(2026, 10, 8, 15, 0)
	todo(t, st, "有截止", &d, true)
	reply, _ := handle(t, h, "列表", "")
	want := "未完成的待办（2）：\n#2 有截止 · 10-08 周四 15:00\n#1 无截止"
	if reply != want {
		t.Fatalf("list = %q, want %q", reply, want)
	}
	for i := 0; i < 25; i++ {
		todo(t, st, "批量"+string(rune('a'+i)), nil, false)
	}
	reply, _ = handle(t, h, "列表", "")
	if !strings.HasPrefix(reply, "未完成的待办（27）：") || !strings.HasSuffix(reply, "…还有 7 条，见网页") {
		t.Fatalf("long list = %q", reply)
	}
}

func TestRefCandidatesAsk(t *testing.T) {
	h, st, _ := newHandler(t, nil)
	todo(t, st, "交发票", nil, false)
	todo(t, st, "交材料", nil, false)
	reply, handled := handle(t, h, "完成", "⏰ 到期提醒 #1 交发票 #2 交材料")
	if !handled || !strings.Contains(reply, "是指哪一条") || !strings.Contains(reply, "#1 交发票") || !strings.Contains(reply, "「完成 1」") {
		t.Fatalf("reply = %q", reply)
	}
	it, _ := st.GetItem(context.Background(), 1)
	if it.Status != model.StatusOpen {
		t.Fatal("must not act on candidates")
	}
}

func TestUnrecognizedWithoutAI(t *testing.T) {
	h, _, _ := newHandler(t, nil)
	reply, handled := handle(t, h, "完成发票那个", "")
	if !handled || !strings.Contains(reply, Usage) {
		t.Fatalf("reply=%q handled=%v", reply, handled)
	}
	// 只是引用、没有操作词、AI 未配置 → 不当指令
	if _, handled := handle(t, h, "这个也看看", "#1"); handled {
		t.Fatal("ref-only message must fall through when AI is off")
	}
}

func TestRefPresentButUnresolved(t *testing.T) {
	tr := &fakeTr{out: Translation{Op: OpDone, ID: 1}}
	h, st, _ := newHandler(t, tr)
	todo(t, st, "交发票", nil, false)
	call := func(text string) (string, bool) {
		reply, handled, err := h.Handle(context.Background(), ingest.CommandInput{Text: text, HasRef: true, MsgID: nextMsg()})
		if err != nil {
			t.Fatal(err)
		}
		return reply, handled
	}
	// 引用了消息但 ingest 没解析出原文：不调 AI，请用户带编号
	if reply, handled := call("完成"); !handled || reply != RefUnknown {
		t.Fatalf("reply=%q handled=%v", reply, handled)
	}
	if reply, _ := call("完成那个"); reply != RefUnknown {
		t.Fatalf("reply=%q", reply)
	}
	if tr.calls != 0 {
		t.Fatalf("AI called %d times", tr.calls)
	}
	// 没有操作词：当普通条目保存
	if _, handled := call("这个也看看"); handled {
		t.Fatal("must fall through")
	}
	// 带了编号照常执行
	if reply, _ := call("完成 1"); reply != "✓ 已完成 #1 交发票" {
		t.Fatalf("reply=%q", reply)
	}
}

func TestReplayedMsgIDNotReExecuted(t *testing.T) {
	h, st, _ := newHandler(t, nil)
	due := clock.At(2026, 10, 1, 15, 0)
	id := todo(t, st, "交材料", &due, true)
	in := ingest.CommandInput{Text: "推迟 1 1天", MsgID: "dup1"}
	r1, _, err := h.Handle(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	it1, _ := st.GetItem(context.Background(), id)
	r2, handled, err := h.Handle(context.Background(), in)
	if err != nil || !handled || r2 == r1 || !strings.Contains(r2, "已处理过") {
		t.Fatalf("r2=%q handled=%v err=%v", r2, handled, err)
	}
	it2, _ := st.GetItem(context.Background(), id)
	if !it2.DueAt.Equal(*it1.DueAt) {
		t.Fatalf("executed twice: %v -> %v", it1.DueAt, it2.DueAt)
	}
	a, err := st.ActionByMsgID(context.Background(), "dup1")
	if err != nil || a.ItemID != id {
		t.Fatalf("action = %+v err=%v", a, err)
	}
}
