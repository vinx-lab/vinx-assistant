package notify

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/ilink/ilinktest"
	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

func TestWeChatSendRecordsMessageID(t *testing.T) {
	srv := ilinktest.New()
	defer srv.Close()
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	sess := session.New(st, nil, nil)
	ctx := context.Background()
	sess.SaveCred(ctx, srv.Cred())
	n := NewWeChat(sess, st)

	if err := n.Send(ctx, "hi"); !errors.Is(err, ErrNoContext) {
		t.Fatalf("err = %v, want ErrNoContext", err)
	}
	sess.RememberContext(ctx, "ctx-7", clock.At(2026, 10, 1, 9, 0))
	if err := n.Send(ctx, "提醒 #12"); err != nil {
		t.Fatal(err)
	}
	s := srv.Sent()
	if len(s) != 1 || s[0].ContextToken != "ctx-7" || s[0].To != ilinktest.OwnerID {
		t.Fatalf("sent = %+v", s)
	}
	if body, ok, _ := st.SentBody(ctx, s[0].MsgID); !ok || body != "提醒 #12" {
		t.Fatalf("sent_msgs: %q %v", body, ok)
	}
	srv.SetSendRet(-2)
	var apiErr *ilink.APIError
	if err := n.Send(ctx, "x"); !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
}
