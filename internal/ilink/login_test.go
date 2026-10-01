package ilink_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/ilink/ilinktest"
)

func TestLoginWithVerifyCode(t *testing.T) {
	srv := ilinktest.New()
	defer srv.Close()
	srv.SetLoginStates("wait", "scaned", "need_verifycode", "confirmed")
	l := ilink.NewLogin(nil)
	l.BaseURL, l.PollDelay = srv.URL, time.Millisecond

	qr, err := l.Start(context.Background(), nil)
	if err != nil || qr.Code != "qr-1" || qr.Content == "" {
		t.Fatalf("qr=%+v err=%v", qr, err)
	}
	var states []string
	cred, err := l.Wait(context.Background(), qr,
		func(context.Context) (string, error) { return "1234", nil },
		func(s string) { states = append(states, s) })
	if err != nil {
		t.Fatal(err)
	}
	if cred.BotToken != ilinktest.Token || cred.UserID != ilinktest.OwnerID || cred.BaseURL != srv.URL {
		t.Fatalf("cred = %+v", cred)
	}
	if len(states) != 3 || states[2] != "confirmed" {
		t.Fatalf("states = %v", states)
	}
}

func TestLoginExpired(t *testing.T) {
	srv := ilinktest.New()
	defer srv.Close()
	srv.SetLoginStates("scaned", "expired")
	l := ilink.NewLogin(nil)
	l.BaseURL, l.PollDelay = srv.URL, time.Millisecond
	qr, _ := l.Start(context.Background(), nil)
	if _, err := l.Wait(context.Background(), qr, nil, nil); !errors.Is(err, ilink.ErrQRExpired) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginNeedsVerifyButNoCallback(t *testing.T) {
	srv := ilinktest.New()
	defer srv.Close()
	srv.SetLoginStates("need_verifycode")
	l := ilink.NewLogin(nil)
	l.BaseURL, l.PollDelay = srv.URL, time.Millisecond
	qr, _ := l.Start(context.Background(), nil)
	if _, err := l.Wait(context.Background(), qr, nil, nil); err == nil {
		t.Fatal("want error when verify callback is nil")
	}
}

func TestLoginConfirmedWithoutUserIDIsError(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"confirmed","bot_token":"t","ilink_bot_id":"b"}`))
	}))
	defer hs.Close()
	l := &ilink.Login{BaseURL: hs.URL, HTTP: hs.Client()}
	if _, err := l.Wait(context.Background(), ilink.QR{Code: "q"}, nil, nil); err == nil || !strings.Contains(err.Error(), "ilink_user_id") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginLiteralZeroDelayDoesNotSpin(t *testing.T) {
	var n int
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 2 {
			w.Write([]byte(`{"status":"wait"}`))
			return
		}
		w.Write([]byte(`{"status":"confirmed","bot_token":"t","ilink_bot_id":"b","ilink_user_id":"u"}`))
	}))
	defer hs.Close()
	l := &ilink.Login{BaseURL: hs.URL, HTTP: hs.Client()} // PollDelay、Timeout 都是零值
	start := time.Now()
	if _, err := l.Wait(context.Background(), ilink.QR{Code: "q"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("polled again after %v, want default 500ms delay", d)
	}
}
