package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/command"
	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/ilink/ilinktest"
	"github.com/vinx-lab/vinx-assistant/internal/ingest"
	"github.com/vinx-lab/vinx-assistant/internal/llm/llmtest"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/notify"
	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

// flow 用假微信（ilinktest）、假 AI（llmtest）和假时钟，走与 New 相同的指令/提醒装配。
type flow struct {
	t   *testing.T
	a   *App
	srv *ilinktest.Server
	ai  *llmtest.Server
	clk *clock.Fake
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	srv := ilinktest.New()
	t.Cleanup(srv.Close)
	ai := llmtest.New()
	t.Cleanup(ai.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(clock.At(2026, 10, 1, 8, 0))
	st.SetClock(clk)
	sess := session.New(st, nil, clk)
	sess.NewClient = func(c ilink.Cred) *ilink.Client { cl := ilink.New(c, nil); cl.CDNBaseURL = srv.URL; return cl }
	if err := sess.SaveCred(context.Background(), srv.Cred()); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &App{Store: st, Clock: clk, Session: sess, Log: log, baseCtx: context.Background()}
	a.Notifier = notify.NewWeChat(sess, st)
	a.Ingest = ingest.New(ingest.Deps{Store: st, Session: sess, Notifier: a.Notifier, Clock: clk, MediaDir: t.TempDir(), Log: log})
	a.wireCommandsAndReminders(nil)
	a.Commands.Start(context.Background())
	t.Cleanup(func() {
		a.Commands.Close()
		a.Ingest.Close()
		st.Close()
	})
	return &flow{t: t, a: a, srv: srv, ai: ai, clk: clk}
}

// useAI 在设置里配好轻量档服务商，指向假 AI。
func (f *flow) useAI() {
	f.t.Helper()
	ctx := context.Background()
	set, err := f.a.Store.LoadSettings(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	set.AI.Providers = []model.Provider{{ID: "p", Name: "测试服务商", BaseURL: f.ai.URL, APIKey: "sk-test"}}
	set.AI.Light = model.ModelRef{ProviderID: "p", Model: "light-model"}
	if err := f.a.Store.SaveSettings(ctx, set); err != nil {
		f.t.Fatal(err)
	}
}

func (f *flow) recv(raw string) {
	f.t.Helper()
	var m ilink.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		f.t.Fatal(err)
	}
	if err := f.a.Ingest.Handle(context.Background(), m); err != nil {
		f.t.Fatal(err)
	}
	f.a.Ingest.Wait()
}

// waitAI 等后台 AI 指令处理完，再等它可能触发的后处理。
func (f *flow) waitAI() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.a.Commands.Wait(ctx); err != nil {
		f.t.Fatal(err)
	}
	f.a.Ingest.Wait()
}

func (f *flow) flushAcks() {
	f.clk.Advance(6 * time.Second)
	f.a.Ingest.FlushAcks(context.Background())
}

func (f *flow) lastSent() string {
	s := f.srv.Sent()
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1].Text
}

func (f *flow) item(id int64) *model.Item {
	f.t.Helper()
	it, err := f.a.Store.GetItem(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return it
}

func (f *flow) setDue(id int64, due time.Time) {
	f.t.Helper()
	it := f.item(id)
	it.DueAt, it.DueHasTime = &due, true
	if err := f.a.Store.UpdateItem(context.Background(), it); err != nil {
		f.t.Fatal(err)
	}
}

func TestFlowCommandDoneAndEcho(t *testing.T) {
	f := newFlow(t)
	f.recv(ilinktest.TextMsg(1, ilinktest.OwnerID, "待办：交发票")) // #1
	f.flushAcks()
	if !strings.HasPrefix(f.lastSent(), "✓ 已收：待办｜交发票") {
		t.Fatalf("ack = %q", f.lastSent())
	}
	f.recv(ilinktest.TextMsg(2, ilinktest.OwnerID, "完成 1"))
	if got := f.lastSent(); !strings.Contains(got, "#1") || !strings.Contains(got, "交发票") || f.item(1).Status != model.StatusDone {
		t.Fatalf("reply=%q status=%s", got, f.item(1).Status)
	}
	// 指令不建条目
	if _, err := f.a.Store.GetItem(context.Background(), 2); err == nil {
		t.Fatal("command stored as item")
	}
	// 重放同一条消息：不再执行、不再回复
	n := len(f.srv.Sent())
	f.recv(ilinktest.TextMsg(2, ilinktest.OwnerID, "完成 1"))
	if len(f.srv.Sent()) != n {
		t.Fatal("replayed command replied again")
	}
}

// 新版微信：引用 Bot 发的到期提醒，引用里只有 msg_id（= sendmessage 返回的 message_id），只回「完成」。
func TestFlowQuoteByMsgIDOnly(t *testing.T) {
	f := newFlow(t)
	f.recv(ilinktest.TextMsg(1, ilinktest.OwnerID, "待办：交发票")) // #1
	f.flushAcks()
	f.setDue(1, clock.At(2026, 10, 1, 15, 0))
	f.clk.Set(clock.At(2026, 10, 1, 15, 0))
	f.a.Remind.Tick(context.Background(), f.clk.Now())
	sent := f.srv.Sent()
	last := sent[len(sent)-1]
	if !strings.Contains(last.Text, "#1 交发票") || last.MsgID == "" {
		t.Fatalf("reminder = %+v", last)
	}
	f.recv(ilinktest.RefMsgIDMsg(2, ilinktest.OwnerID, "完成", last.MsgID))
	if f.item(1).Status != model.StatusDone || !strings.Contains(f.lastSent(), "#1") {
		t.Fatalf("reply=%q status=%s", f.lastSent(), f.item(1).Status)
	}
}

func TestFlowAIFallbackRepliesAsync(t *testing.T) {
	f := newFlow(t)
	f.useAI()
	f.recv(ilinktest.TextMsg(1, ilinktest.OwnerID, "待办：交发票")) // #1
	f.recv(ilinktest.TextMsg(2, ilinktest.OwnerID, "待办：交材料")) // #2
	f.flushAcks()

	f.ai.Enqueue(llmtest.JSON(map[string]any{"op": "done", "id": 2}))
	f.recv(ilinktest.TextMsg(3, ilinktest.OwnerID, "完成材料那个"))
	f.waitAI()
	sent := f.srv.Sent()
	if len(sent) < 2 || sent[len(sent)-2].Text != command.Thinking {
		t.Fatalf("want thinking reply first, sent = %+v", sent)
	}
	if got := f.lastSent(); !strings.Contains(got, "#2") || f.item(2).Status != model.StatusDone || f.item(1).Status != model.StatusOpen {
		t.Fatalf("reply=%q #1=%s #2=%s", got, f.item(1).Status, f.item(2).Status)
	}
	reqs := f.ai.Requests()
	if len(reqs) != 1 || reqs[0].Model != "light-model" || !strings.Contains(reqs[0].Text, "#2 交材料") {
		t.Fatalf("ai requests = %+v", reqs)
	}
	var level, prov, mdl string
	var pt int64
	if err := f.a.Store.DB().QueryRow(`SELECT level, provider, model, prompt_tokens FROM llm_usage`).Scan(&level, &prov, &mdl, &pt); err != nil {
		t.Fatal(err)
	}
	if level != store.UsageLevelCommand || prov != "测试服务商" || mdl != "light-model" || pt != 100 {
		t.Fatalf("usage = %s %s %s %d", level, prov, mdl, pt)
	}

	// 引用一段普通文字、不以操作词开头：AI 判为「不是指令」→ 按普通收件存成条目并回执
	f.ai.Enqueue(llmtest.JSON(map[string]any{"op": "none"}))
	n := len(f.srv.Sent())
	f.recv(ilinktest.RefTextMsg(4, ilinktest.OwnerID, "这个想法不错，周末试试", "一段别人的话"))
	if len(f.srv.Sent()) != n {
		t.Fatalf("no-word message must stay silent while AI thinks: %q", f.lastSent())
	}
	f.waitAI()
	it, err := f.a.Store.GetItemByMsgID(context.Background(), "4")
	if err != nil {
		t.Fatalf("not saved as item: %v", err)
	}
	if it.Category != model.CatIdea || it.RawText != "这个想法不错，周末试试" {
		t.Fatalf("item = %+v", it)
	}
	f.flushAcks()
	if !strings.HasPrefix(f.lastSent(), "✓ 已收：点子｜这个想法不错") {
		t.Fatalf("ack = %q", f.lastSent())
	}
}

func TestFlowDigestOncePerDay(t *testing.T) {
	f := newFlow(t)
	f.recv(ilinktest.TextMsg(1, ilinktest.OwnerID, "待办：交材料")) // #1
	f.flushAcks()
	f.setDue(1, clock.At(2026, 10, 1, 18, 0))
	n := len(f.srv.Sent())
	f.clk.Set(clock.At(2026, 10, 1, 8, 59))
	f.a.Remind.Tick(context.Background(), f.clk.Now())
	if len(f.srv.Sent()) != n {
		t.Fatalf("digest sent early: %q", f.lastSent())
	}
	for _, at := range []time.Time{clock.At(2026, 10, 1, 9, 0), clock.At(2026, 10, 1, 9, 1), clock.At(2026, 10, 1, 9, 30)} {
		f.clk.Set(at)
		f.a.Remind.Tick(context.Background(), f.clk.Now())
	}
	sent := f.srv.Sent()[n:]
	if len(sent) != 1 || !strings.Contains(sent[0].Text, "📋 今日摘要") || !strings.Contains(sent[0].Text, "#1 交材料") {
		t.Fatalf("digests = %+v", sent)
	}
}

func TestFlowDeferredRidesOnNextAck(t *testing.T) {
	f := newFlow(t)
	f.recv(ilinktest.TextMsg(1, ilinktest.OwnerID, "待办：交材料")) // #1
	f.flushAcks()
	f.setDue(1, clock.At(2026, 10, 1, 15, 0))

	f.srv.SetSendRet(-2) // context_token 失效
	f.clk.Set(clock.At(2026, 10, 1, 9, 0))
	f.a.Remind.Tick(context.Background(), f.clk.Now())
	f.clk.Set(clock.At(2026, 10, 1, 15, 0))
	f.a.Remind.Tick(context.Background(), f.clk.Now())
	if def, _ := f.a.Store.RemindersByState(context.Background(), "deferred"); len(def) != 2 {
		t.Fatalf("deferred = %d", len(def))
	}

	f.srv.SetSendRet(0)
	f.recv(ilinktest.TextMsg(2, ilinktest.OwnerID, "随手记一条")) // #2
	f.flushAcks()
	got := f.lastSent()
	for _, want := range []string{"✓ 已收：未整理｜随手记一条", "📋 今日摘要", "#1 交材料"} {
		if !strings.Contains(got, want) {
			t.Errorf("ack missing %q:\n%s", want, got)
		}
	}
	if def, _ := f.a.Store.RemindersByState(context.Background(), "deferred"); len(def) != 0 {
		t.Fatalf("still deferred: %d", len(def))
	}
}
