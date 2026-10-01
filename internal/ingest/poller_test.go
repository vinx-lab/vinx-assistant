package ingest

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/ilink/ilinktest"
	"github.com/vinx-lab/vinx-assistant/internal/session"
)

func runPoller(t *testing.T, e *env) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &Poller{Session: e.sess, Service: e.svc, Store: e.st, RetryDelay: 10 * time.Millisecond, BackoffDelay: 10 * time.Millisecond, IdleDelay: 10 * time.Millisecond}
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in 3s")
}

func TestPollerStoresMessagesAndSavesCursor(t *testing.T) {
	e := newEnv(t, nil)
	stop := runPoller(t, e)
	defer stop()
	e.srv.Push(ilinktest.TextMsg(1, ilinktest.OwnerID, "hello"))
	eventually(t, func() bool { _, err := e.st.GetItem(context.Background(), 1); return err == nil })
	eventually(t, func() bool {
		v, ok, _ := e.st.GetKV(context.Background(), keyBuf)
		return ok && v != ""
	})
	stop()
	bufs := e.srv.Bufs()
	if bufs[0] != "" {
		t.Fatalf("first poll must use empty cursor, got %q", bufs[0])
	}
}

func TestPollerPausesOnStaleTokenThenResumes(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.SetUpdatesRet(-14)
	stop := runPoller(t, e)
	defer stop()
	eventually(t, func() bool { return e.sess.Status(context.Background()) == session.StatusPaused })
	e.srv.SetUpdatesRet(0)
	e.srv.Push(ilinktest.TextMsg(1, ilinktest.OwnerID, "after pause"))
	time.Sleep(50 * time.Millisecond)
	if _, err := e.st.GetItem(context.Background(), 1); err == nil {
		t.Fatal("polled while paused")
	}
	e.clk.Advance(session.PauseDuration)
	eventually(t, func() bool { _, err := e.st.GetItem(context.Background(), 1); return err == nil })
	eventually(t, func() bool { return e.sess.StaleCount(context.Background()) == 0 })
}

func TestPollerSkipsUndecodableAndKeepsGoing(t *testing.T) {
	e := newEnv(t, nil)
	var logs syncBuf
	ctx, cancel := context.WithCancel(context.Background())
	p := &Poller{Session: e.sess, Service: e.svc, Store: e.st, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		RetryDelay: 10 * time.Millisecond, BackoffDelay: 10 * time.Millisecond, IdleDelay: 10 * time.Millisecond}
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	e.srv.Push(`{"seq":1,"message_id":1,"from_user_id":"` + ilinktest.OwnerID + `","message_type":1,"context_token":"SECRET-TOKEN","item_list":"oops"}`)
	e.srv.Push(ilinktest.TextMsg(2, ilinktest.OwnerID, "good"))
	eventually(t, func() bool { _, err := e.st.GetItem(context.Background(), 1); return err == nil })
	eventually(t, func() bool {
		v, ok, _ := e.st.GetKV(context.Background(), keyBuf)
		return ok && v != ""
	})
	it, _ := e.st.GetItem(context.Background(), 1)
	if it.RawText != "good" {
		t.Fatalf("stored item = %q, want the good message", it.RawText)
	}
	out := logs.String()
	if !strings.Contains(out, "有消息无法解析") || !strings.Contains(out, "count=1") {
		t.Fatalf("missing warn: %q", out)
	}
	if strings.Contains(out, "SECRET-TOKEN") || strings.Contains(out, "oops") {
		t.Fatalf("raw message leaked into log: %s", out)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
