package ilink_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/ilink/ilinktest"
)

func TestLoginWithVerifyCode(t *testing.T) {
	srv := ilinktest.New()
	defer srv.Close()
	srv.SetLoginStates("wait", "scaned", "need_verifycode", "confirmed")
	l := ilink.NewLogin(nil)
	l.BaseURL, l.PollDelay = srv.URL, 0

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
	l.BaseURL, l.PollDelay = srv.URL, 0
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
	l.BaseURL, l.PollDelay = srv.URL, 0
	qr, _ := l.Start(context.Background(), nil)
	if _, err := l.Wait(context.Background(), qr, nil, nil); err == nil {
		t.Fatal("want error when verify callback is nil")
	}
}
