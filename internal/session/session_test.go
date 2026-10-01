package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

func newTest(t *testing.T) (*Session, *clock.Fake) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := clock.NewFake(clock.At(2026, 10, 1, 9, 0))
	return New(st, nil, clk), clk
}

func TestNoCredThenSave(t *testing.T) {
	s, _ := newTest(t)
	ctx := context.Background()
	if s.Status(ctx) != StatusNoCred {
		t.Fatalf("status = %s", s.Status(ctx))
	}
	if _, err := s.Client(ctx); !errors.Is(err, ErrNoCred) {
		t.Fatalf("err = %v", err)
	}
	if err := s.SaveCred(ctx, ilink.Cred{BotToken: "tok-1", UserID: "u", BaseURL: "http://x"}); err != nil {
		t.Fatal(err)
	}
	c1, err := s.Client(ctx)
	if err != nil || c1.BaseURL != "http://x" {
		t.Fatalf("client=%v err=%v", c1, err)
	}
	c2, _ := s.Client(ctx)
	if c1 != c2 {
		t.Fatal("client must be cached while token unchanged")
	}
	cred, ok, _ := s.Cred(ctx)
	if !ok || cred.UserID != "u" {
		t.Fatalf("cred = %+v", cred)
	}
}

func TestPauseOneHourThenResume(t *testing.T) {
	s, clk := newTest(t)
	ctx := context.Background()
	s.SaveCred(ctx, ilink.Cred{BotToken: "tok-1", UserID: "u"})
	s.Pause(ctx)
	if s.Status(ctx) != StatusPaused || s.StaleCount(ctx) != 1 {
		t.Fatalf("status=%s stale=%d", s.Status(ctx), s.StaleCount(ctx))
	}
	if _, err := s.Client(ctx); !errors.Is(err, ErrPaused) {
		t.Fatalf("err = %v", err)
	}
	clk.Advance(59 * time.Minute)
	if s.Status(ctx) != StatusPaused {
		t.Fatal("resumed too early")
	}
	clk.Advance(time.Minute)
	if _, err := s.Client(ctx); err != nil {
		t.Fatalf("must resume after 1h: %v", err)
	}
	if s.StaleCount(ctx) != 1 {
		t.Fatal("stale count must survive until a successful poll")
	}
	s.MarkHealthy(ctx)
	if s.StaleCount(ctx) != 0 {
		t.Fatal("MarkHealthy must reset")
	}
}

func TestNewCredClearsPauseAndKeepsHistory(t *testing.T) {
	s, _ := newTest(t)
	ctx := context.Background()
	for i := 1; i <= 12; i++ {
		s.SaveCred(ctx, ilink.Cred{BotToken: "tok-" + string(rune('a'+i)), UserID: "u"})
		if i == 5 {
			s.Pause(ctx)
		}
	}
	if s.Status(ctx) != StatusOK {
		t.Fatalf("status = %s", s.Status(ctx))
	}
	h := s.TokenHistory(ctx)
	if len(h) != 10 || h[0] != "tok-"+string(rune('a'+12)) {
		t.Fatalf("history = %v", h)
	}
}

func TestRememberContext(t *testing.T) {
	s, _ := newTest(t)
	ctx := context.Background()
	if _, _, ok, _ := s.Context(ctx); ok {
		t.Fatal("unexpected context")
	}
	at := clock.At(2026, 10, 1, 9, 30)
	s.RememberContext(ctx, "ctx-9", at)
	tok, got, ok, err := s.Context(ctx)
	if err != nil || !ok || tok != "ctx-9" || !got.Equal(at) {
		t.Fatalf("tok=%s at=%v ok=%v err=%v", tok, got, ok, err)
	}
}

func TestSaveCredClearsStateOnAccountChange(t *testing.T) {
	s, _ := newTest(t)
	ctx := context.Background()
	st := s.st
	seed := func() {
		st.SetKV(ctx, keyBuf, "cursor-1")
		s.RememberContext(ctx, "ctx-1", clock.At(2026, 10, 1, 9, 0))
	}
	has := func(k string) bool { _, ok, _ := st.GetKV(ctx, k); return ok }

	s.SaveCred(ctx, ilink.Cred{BotToken: "t1", BotID: "bot1", UserID: "u1"})
	seed()
	// 同一 Bot 同一用户重新登录：保留游标和 context
	s.SaveCred(ctx, ilink.Cred{BotToken: "t2", BotID: "bot1", UserID: "u1"})
	if !has(keyBuf) || !has(keyContext) {
		t.Fatal("same bot re-login must keep buf and context")
	}
	// 换 Bot：清游标，用户没变保留 context
	s.SaveCred(ctx, ilink.Cred{BotToken: "t3", BotID: "bot2", UserID: "u1"})
	if has(keyBuf) || !has(keyContext) {
		t.Fatalf("bot changed: buf=%v context=%v", has(keyBuf), has(keyContext))
	}
	seed()
	// 换用户：清 context 和时间，游标保留
	s.SaveCred(ctx, ilink.Cred{BotToken: "t4", BotID: "bot2", UserID: "u2"})
	if !has(keyBuf) || has(keyContext) || has(keyContextAt) {
		t.Fatalf("user changed: buf=%v context=%v at=%v", has(keyBuf), has(keyContext), has(keyContextAt))
	}
}

func TestSaveCredRejectsEmptyTokenAndBadHistory(t *testing.T) {
	s, _ := newTest(t)
	ctx := context.Background()
	if err := s.SaveCred(ctx, ilink.Cred{UserID: "u"}); err == nil {
		t.Fatal("empty token accepted")
	}
	if _, ok, _ := s.Cred(ctx); ok {
		t.Fatal("cred written despite error")
	}
	s.st.SetKV(ctx, keyTokens, "not json")
	if err := s.SaveCred(ctx, ilink.Cred{BotToken: "t", UserID: "u"}); err == nil {
		t.Fatal("corrupt history must be an error, not overwritten")
	}
	if v, _, _ := s.st.GetKV(ctx, keyTokens); v != "not json" {
		t.Fatalf("history overwritten: %q", v)
	}
}

func TestClientRebuiltWhenBaseURLChanges(t *testing.T) {
	s, _ := newTest(t)
	ctx := context.Background()
	s.SaveCred(ctx, ilink.Cred{BotToken: "t", UserID: "u", BaseURL: "http://a"})
	c1, _ := s.Client(ctx)
	s.SaveCred(ctx, ilink.Cred{BotToken: "t", UserID: "u", BaseURL: "http://b"})
	c2, _ := s.Client(ctx)
	if c1 == c2 || c2.BaseURL != "http://b" {
		t.Fatalf("client not rebuilt: %v", c2)
	}
}
