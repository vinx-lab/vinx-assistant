package command

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/ingest"
	"github.com/vinx-lab/vinx-assistant/internal/llm"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

type fakeChat struct {
	reply        string
	err          error
	system, user string
}

func (f *fakeChat) Chat(ctx context.Context, system, user string) (string, int, int, error) {
	f.system, f.user = system, user
	return f.reply, 120, 30, f.err
}

func TestAITranslatorPromptAndRecord(t *testing.T) {
	due := clock.At(2026, 10, 8, 15, 0)
	todos := []model.Item{{ID: 12, RawText: "交发票", DueAt: &due, DueHasTime: true}, {ID: 15, RawText: "交材料"}}
	fc := &fakeChat{reply: "```json\n{\"op\":\"done\",\"id\":12}\n```"}
	var recorded [2]int
	tr := &AITranslator{Chat: fc, Record: func(_ context.Context, p, c int) { recorded = [2]int{p, c} }}
	got, err := tr.Translate(context.Background(), "发票那个搞定了", todos, clock.At(2026, 10, 1, 9, 0))
	if err != nil || got.Op != OpDone || got.ID != 12 {
		t.Fatalf("got %+v err=%v", got, err)
	}
	for _, want := range []string{"当前时间：2026-10-01 09:00 周四", "#12 交发票（截止 2026-10-08 15:00）", "#15 交材料", "用户说：发票那个搞定了"} {
		if !strings.Contains(fc.user, want) {
			t.Errorf("prompt missing %q:\n%s", want, fc.user)
		}
	}
	if !strings.Contains(fc.system, "reschedule") || recorded != [2]int{120, 30} {
		t.Fatalf("system/record wrong: %v", recorded)
	}
}

func TestParseTranslation(t *testing.T) {
	cases := map[string]Translation{
		`{"op":"reschedule","id":3,"when":"2026-10-08 15:00"}`: {Op: OpReschedule, ID: 3, When: "2026-10-08 15:00"},
		`好的：{"op":"ASK","candidates":[1,2]}`:                   {Op: OpAsk, Candidates: []int64{1, 2}},
		`{"op":" none "}`: {Op: OpNone},
	}
	for in, want := range cases {
		got, err := parseTranslation(in)
		if err != nil || got.Op != want.Op || got.ID != want.ID || got.When != want.When || len(got.Candidates) != len(want.Candidates) {
			t.Errorf("parseTranslation(%q) = %+v err=%v", in, got, err)
		}
	}
	if _, err := parseTranslation("不是 JSON"); err == nil {
		t.Fatal("want error")
	}
}

type fakeLLM struct {
	req llm.Request
}

func (f *fakeLLM) Chat(ctx context.Context, req llm.Request) (llm.Response, error) {
	f.req = req
	return llm.Response{Content: `{"op":"list"}`, Usage: llm.Usage{PromptTokens: 7, CompletionTokens: 3}}, nil
}

func TestLLMChatterAndUsageRecorder(t *testing.T) {
	_, st, clk := newHandler(t, nil)
	fl := &fakeLLM{}
	c := &LLMChatter{Client: fl, Model: "light-1"}
	content, p, cp, err := c.Chat(context.Background(), "sys", "usr")
	if err != nil || content != `{"op":"list"}` || p != 7 || cp != 3 {
		t.Fatalf("content=%q p=%d c=%d err=%v", content, p, cp, err)
	}
	if fl.req.Model != "light-1" || len(fl.req.Messages) != 2 || fl.req.MaxTokens <= 0 {
		t.Fatalf("req = %+v", fl.req)
	}
	UsageRecorder(st, clk, "prov", "light-1")(context.Background(), 7, 3)
	var level, day string
	var total int
	if err := st.DB().QueryRow(`SELECT level, day, prompt_tokens + completion_tokens FROM llm_usage`).Scan(&level, &day, &total); err != nil {
		t.Fatal(err)
	}
	if level != "command" || day != "2026-10-01" || total != 10 {
		t.Fatalf("usage level=%s day=%s total=%d", level, day, total)
	}
}

// ---- 异步执行 ----

type asyncEnv struct {
	h  *Handler
	st *store.Store

	mu      sync.Mutex
	replies []string
	saved   map[string]string
}

func (e *asyncEnv) Replies() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.replies...)
}

func (e *asyncEnv) Saved() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]string{}
	for k, v := range e.saved {
		out[k] = v
	}
	return out
}

// wire 给 h 接上回复与「存为普通条目」的钩子并启动后台。
func wire(t *testing.T, h *Handler, st *store.Store) *asyncEnv {
	t.Helper()
	e := &asyncEnv{h: h, st: st, saved: map[string]string{}}
	h.Reply = func(_ context.Context, text string) {
		e.mu.Lock()
		e.replies = append(e.replies, text)
		e.mu.Unlock()
	}
	h.SaveAsItem = func(_ context.Context, msgID, text string) error {
		e.mu.Lock()
		e.saved[msgID] = text
		e.mu.Unlock()
		return nil
	}
	h.Start(context.Background())
	t.Cleanup(h.Close)
	return e
}

func newAsync(t *testing.T, tr Translator) (*asyncEnv, *store.Store) {
	t.Helper()
	h, st, _ := newHandler(t, tr)
	return wire(t, h, st), st
}

func settle(t *testing.T, h *Handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Wait(ctx); err != nil {
		t.Fatalf("worker not idle: %v", err)
	}
}

// lastReply 等后台处理完，返回唯一一条异步回复。
func lastReply(t *testing.T, e *asyncEnv) string {
	t.Helper()
	settle(t, e.h)
	r := e.Replies()
	if len(r) != 1 {
		t.Fatalf("replies = %q", r)
	}
	return r[0]
}

type safeTr struct {
	mu    sync.Mutex
	out   Translation
	err   error
	calls int
	text  string
	todos []model.Item
	gate  chan struct{} // 非 nil 时等它关闭（或 ctx 取消）再返回
}

func (f *safeTr) Translate(ctx context.Context, text string, todos []model.Item, now time.Time) (Translation, error) {
	f.mu.Lock()
	f.calls++
	f.text, f.todos = text, todos
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return Translation{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out, f.err
}

func (f *safeTr) Calls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func TestHandlerUsesAI(t *testing.T) {
	tr := &safeTr{out: Translation{Op: OpDone, ID: 1}}
	e, st := newAsync(t, tr)
	todo(t, st, "交发票", nil, false)
	reply, handled := handle(t, e.h, "发票那个搞定了", "#1")
	if !handled || reply != "" {
		t.Fatalf("immediate reply=%q handled=%v", reply, handled)
	}
	if got := lastReply(t, e); got != "✓ 已完成 #1 交发票" || tr.Calls() != 1 || len(tr.todos) != 1 {
		t.Fatalf("reply=%q calls=%d", got, tr.Calls())
	}
	if !strings.Contains(tr.text, "引用的消息：#1") {
		t.Fatalf("ref not passed: %q", tr.text)
	}
}

func TestHandlerAIThinkingReplyWithActionWord(t *testing.T) {
	tr := &safeTr{out: Translation{Op: OpDone, ID: 1}}
	e, st := newAsync(t, tr)
	todo(t, st, "交发票", nil, false)
	if reply, handled := handle(t, e.h, "完成发票那个", ""); !handled || reply != Thinking {
		t.Fatalf("immediate reply=%q handled=%v", reply, handled)
	}
	if got := lastReply(t, e); got != "✓ 已完成 #1 交发票" {
		t.Fatalf("reply = %q", got)
	}
}

func TestHandlerAIRejectsUnknownID(t *testing.T) {
	tr := &safeTr{out: Translation{Op: OpDone, ID: 99}}
	e, st := newAsync(t, tr)
	todo(t, st, "交发票", nil, false)
	handle(t, e.h, "完成那个", "")
	if got := lastReply(t, e); got != "没找到未完成的待办 #99。" {
		t.Fatalf("reply = %q", got)
	}
	if it, _ := st.GetItem(context.Background(), 1); it.Status != model.StatusOpen {
		t.Fatal("must not act on unknown id")
	}
}

func TestHandlerAINoneSavesAsItem(t *testing.T) {
	tr := &safeTr{out: Translation{Op: OpNone}}
	e, _ := newAsync(t, tr)
	in := ingest.CommandInput{Text: "完成了一个新想法：做收集箱", MsgID: nextMsg()}
	reply, handled, err := e.h.Handle(context.Background(), in)
	if err != nil || !handled || reply != Thinking {
		t.Fatalf("reply=%q handled=%v err=%v", reply, handled, err)
	}
	settle(t, e.h)
	if r := e.Replies(); len(r) != 0 {
		t.Fatalf("op=none must not reply: %q", r)
	}
	if got := e.Saved()[in.MsgID]; got != in.Text {
		t.Fatalf("saved = %q", e.Saved())
	}
}

func TestHandlerAINoneWithoutSaveHook(t *testing.T) {
	tr := &safeTr{out: Translation{Op: OpNone}}
	h, st, _ := newHandler(t, tr)
	e := wire(t, h, st)
	h.SaveAsItem = nil
	// 没有操作词又没有「存为条目」的钩子：同步落回普通条目，不走 AI
	if _, handled := handle(t, h, "这个也看看", "#1"); handled {
		t.Fatal("must fall through when SaveAsItem is nil")
	}
	// 有操作词：AI 说不是指令，只能回用法提示
	handle(t, h, "完成了一个新想法", "")
	if got := lastReply(t, e); !strings.Contains(got, Usage) {
		t.Fatalf("reply = %q", got)
	}
}

func TestHandlerAIAsk(t *testing.T) {
	tr := &safeTr{out: Translation{Op: OpAsk, Candidates: []int64{1, 2, 99}}}
	e, st := newAsync(t, tr)
	todo(t, st, "交发票", nil, false)
	todo(t, st, "交材料", nil, false)
	handle(t, e.h, "完成交的那个", "")
	got := lastReply(t, e)
	if !strings.Contains(got, "#1 交发票；#2 交材料") || strings.Contains(got, "#99") || !strings.Contains(got, "「完成 1」") {
		t.Fatalf("reply = %q", got)
	}
}

func TestHandlerAIReschedule(t *testing.T) {
	tr := &safeTr{out: Translation{Op: OpReschedule, ID: 1, When: "2026-10-31"}}
	e, st := newAsync(t, tr)
	todo(t, st, "交发票", nil, false)
	handle(t, e.h, "推迟发票到月底", "")
	if got := lastReply(t, e); !strings.Contains(got, "10-31 周六") {
		t.Fatalf("reply = %q", got)
	}
	it, _ := st.GetItem(context.Background(), 1)
	if it.DueAt == nil || it.DueHasTime {
		t.Fatalf("due = %+v", it)
	}
}

func TestHandlerAIError(t *testing.T) {
	tr := &safeTr{err: errors.New("timeout")}
	e, _ := newAsync(t, tr)
	reply, handled := handle(t, e.h, "完成那个", "")
	if !handled || reply != Thinking {
		t.Fatalf("reply=%q handled=%v", reply, handled)
	}
	if got := lastReply(t, e); !strings.Contains(got, "AI 暂时不可用") {
		t.Fatalf("reply = %q", got)
	}
	// 没有操作词的消息：AI 失败时当普通条目保存，不丢
	in := ingest.CommandInput{Text: "这个也看看", RefText: "#1", HasRef: true, MsgID: nextMsg()}
	if _, handled, _ := e.h.Handle(context.Background(), in); !handled {
		t.Fatal("want handled")
	}
	settle(t, e.h)
	if e.Saved()[in.MsgID] != in.Text {
		t.Fatalf("saved = %q", e.Saved())
	}
}

func TestSlowAIDoesNotBlockHandle(t *testing.T) {
	tr := &safeTr{out: Translation{Op: OpDone, ID: 1}, gate: make(chan struct{})}
	e, st := newAsync(t, tr)
	todo(t, st, "交发票", nil, false)
	todo(t, st, "交材料", nil, false)
	start := time.Now()
	handle(t, e.h, "完成发票那个", "")
	// 第一条还卡在 AI 时，后面的固定格式指令照常同步执行
	if reply, _ := handle(t, e.h, "完成 2", ""); reply != "✓ 已完成 #2 交材料" {
		t.Fatalf("sync reply = %q", reply)
	}
	handle(t, e.h, "取消发票那个", "")
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("Handle blocked for %v", d)
	}
	if r := e.Replies(); len(r) != 0 {
		t.Fatalf("premature replies: %q", r)
	}
	close(tr.gate)
	settle(t, e.h)
	r := e.Replies()
	if len(r) != 2 || r[0] != "✓ 已完成 #1 交发票" || !strings.Contains(r[1], "没找到未完成的待办 #1") {
		t.Fatalf("replies = %q", r)
	}
}

func TestPendingResumesAfterRestart(t *testing.T) {
	tr := &safeTr{out: Translation{Op: OpDone, ID: 1}, gate: make(chan struct{})}
	h1, st, clk := newHandler(t, tr)
	e1 := wire(t, h1, st)
	todo(t, st, "交发票", nil, false)
	in := ingest.CommandInput{Text: "完成发票那个", MsgID: nextMsg()}
	if _, handled, err := h1.Handle(context.Background(), in); err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	for tr.Calls() == 0 { // 等后台开始调用 AI
		time.Sleep(time.Millisecond)
	}
	h1.Close() // 模拟进程退出：进行中的 AI 调用被取消，指令留在待处理表
	if len(e1.Replies()) != 0 {
		t.Fatalf("replies = %q", e1.Replies())
	}
	pending, err := st.PendingAICommands(context.Background(), 10)
	if err != nil || len(pending) != 1 || pending[0].MsgID != in.MsgID {
		t.Fatalf("pending = %+v err=%v", pending, err)
	}

	tr2 := &safeTr{out: Translation{Op: OpDone, ID: 1}}
	h2 := &Handler{Store: st, Clock: clk, Translator: func(context.Context) (Translator, error) { return tr2, nil }}
	e2 := wire(t, h2, st)
	if got := lastReply(t, e2); got != "✓ 已完成 #1 交发票" {
		t.Fatalf("resumed reply = %q", got)
	}
	if pending, _ := st.PendingAICommands(context.Background(), 10); len(pending) != 0 {
		t.Fatalf("still pending: %+v", pending)
	}
	// 重放同一条消息：只回显，不再调 AI
	reply, handled, err := h2.Handle(context.Background(), in)
	if err != nil || !handled || !strings.Contains(reply, "已处理过") || tr2.Calls() != 1 {
		t.Fatalf("replay reply=%q handled=%v calls=%d", reply, handled, tr2.Calls())
	}
}

func TestResumeAfterExecutedSkipsAI(t *testing.T) {
	// 崩溃发生在「已执行、未标完成」之间：重启后按 msg_id 查到指令记录，只回显不再调 AI
	h, st, clk := newHandler(t, nil)
	todo(t, st, "交发票", nil, false)
	ctx := context.Background()
	if err := st.EnqueueAICommand(ctx, &store.AICommand{MsgID: "mx", Text: "完成发票那个", HasWord: true}); err != nil {
		t.Fatal(err)
	}
	_, err := st.ModifyItemWithAction(ctx, 1, func(it *model.Item) (*store.Action, error) {
		before := SnapshotOf(it)
		Apply(it, Cmd{Op: OpDone, ID: 1}, clk.Now())
		b, _ := json.Marshal(before)
		a, _ := json.Marshal(SnapshotOf(it))
		return &store.Action{MsgID: "mx", Command: "完成发票那个", Before: string(b), After: string(a)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := &safeTr{out: Translation{Op: OpDone, ID: 1}}
	h.Translator = func(context.Context) (Translator, error) { return tr, nil }
	e := wire(t, h, st)
	if got := lastReply(t, e); !strings.Contains(got, "已处理过") || tr.Calls() != 0 {
		t.Fatalf("reply=%q calls=%d", got, tr.Calls())
	}
}

func TestSameMsgIDQueuedOnce(t *testing.T) {
	tr := &safeTr{out: Translation{Op: OpList}, gate: make(chan struct{})}
	e, st := newAsync(t, tr)
	todo(t, st, "交发票", nil, false)
	in := ingest.CommandInput{Text: "列一下还有啥", RefText: "#1", HasRef: true, MsgID: nextMsg()}
	for i := 0; i < 3; i++ {
		reply, handled, err := e.h.Handle(context.Background(), in)
		if err != nil || !handled || reply != "" {
			t.Fatalf("#%d reply=%q handled=%v err=%v", i, reply, handled, err)
		}
	}
	close(tr.gate)
	if got := lastReply(t, e); !strings.Contains(got, "#1 交发票") || tr.Calls() != 1 {
		t.Fatalf("reply=%q calls=%d", got, tr.Calls())
	}
	// 处理完之后再重放：不再排队、不再回复
	if reply, handled, _ := e.h.Handle(context.Background(), in); !handled || reply != "" {
		t.Fatalf("after done reply=%q handled=%v", reply, handled)
	}
	settle(t, e.h)
	if tr.Calls() != 1 || len(e.Replies()) != 1 {
		t.Fatalf("calls=%d replies=%q", tr.Calls(), e.Replies())
	}
}

func TestPendingSurvivesWithoutWorker(t *testing.T) {
	// Handle 在后台启动前也能收：先落库，Start 时补做
	tr := &safeTr{out: Translation{Op: OpDone, ID: 1}}
	h, st, _ := newHandler(t, tr)
	todo(t, st, "交发票", nil, false)
	h.Reply = func(context.Context, string) {}
	h.SaveAsItem = func(context.Context, string, string) error { return nil }
	handle(t, h, "完成发票那个", "")
	if tr.Calls() != 0 {
		t.Fatal("must not translate before Start")
	}
	e := wire(t, h, st)
	if got := lastReply(t, e); got != "✓ 已完成 #1 交发票" {
		t.Fatalf("reply = %q", got)
	}
}
