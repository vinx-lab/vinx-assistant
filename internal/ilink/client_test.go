package ilink_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/ilink/ilinktest"
)

func newClient(t *testing.T) (*ilink.Client, *ilinktest.Server) {
	t.Helper()
	srv := ilinktest.New()
	t.Cleanup(srv.Close)
	c := ilink.New(srv.Cred(), nil)
	c.CDNBaseURL = srv.URL
	return c, srv
}

func TestGetUpdatesParsesTextAndKeepsRaw(t *testing.T) {
	c, srv := newClient(t)
	srv.Push(ilinktest.TextMsg(7511267372956042376, ilinktest.OwnerID, "发一行文字"))
	u, err := c.GetUpdates(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Msgs) != 1 || u.Buf != "buf-1" {
		t.Fatalf("updates = %+v", u)
	}
	m := u.Msgs[0]
	if m.ID() != "7511267372956042376" || m.Items[0].Text.Text != "发一行文字" || m.ContextToken == "" {
		t.Fatalf("msg = %+v", m)
	}
	if !bytes.Contains(m.Raw, []byte("7511267372956042376")) {
		t.Fatal("raw JSON not kept")
	}
}

func TestGetUpdatesEmptyLongPoll(t *testing.T) {
	c, _ := newClient(t)
	u, err := c.GetUpdates(context.Background(), "keep")
	if err != nil || len(u.Msgs) != 0 {
		t.Fatalf("u=%+v err=%v", u, err)
	}
}

func TestGetUpdatesFollowsServerPollHint(t *testing.T) {
	c, srv := newClient(t)
	if c.PollTimeout() != 35*time.Second {
		t.Fatalf("default poll timeout = %v", c.PollTimeout())
	}
	srv.PollHintMs = 25000
	if _, err := c.GetUpdates(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if c.PollTimeout() != 25*time.Second {
		t.Fatalf("poll timeout = %v, want 25s", c.PollTimeout())
	}
}

func TestRefMessageForms(t *testing.T) {
	c, srv := newClient(t)
	srv.Push(ilinktest.RefTextMsg(1, ilinktest.OwnerID, "完成", "提醒 #12"))
	srv.Push(ilinktest.RefSvrMsg(2, ilinktest.OwnerID, "完成", "9000000000000000001"))
	u, _ := c.GetUpdates(context.Background(), "")
	r1, r2 := u.Msgs[0].Items[0].RefMsg, u.Msgs[1].Items[0].RefMsg
	if r1 == nil || r1.MessageItem == nil || r1.MessageItem.Text.Text != "提醒 #12" {
		t.Fatalf("ref1 = %+v", r1)
	}
	if r2 == nil || r2.SvrID.String() != "9000000000000000001" || r2.MessageItem != nil {
		t.Fatalf("ref2 = %+v", r2)
	}
}

func TestNonImageWithoutKeyIsRejected(t *testing.T) {
	it := ilink.Item{Type: ilink.TypeFile, File: &ilink.FileItem{Media: ilink.Media{EncryptQueryParam: "p"}}}
	if _, err := it.MediaKey(); !errors.Is(err, ilink.ErrNoMediaKey) {
		t.Fatalf("err = %v", err)
	}
	img := ilink.Item{Type: ilink.TypeImage, Image: &ilink.ImageItem{Media: ilink.Media{EncryptQueryParam: "p"}}}
	if k, err := img.MediaKey(); k != nil || err != nil {
		t.Fatalf("image without key must be plaintext: %v %v", k, err)
	}
}

func TestGetUpdatesSessionExpired(t *testing.T) {
	c, srv := newClient(t)
	srv.SetUpdatesRet(-14)
	if _, err := c.GetUpdates(context.Background(), ""); !errors.Is(err, ilink.ErrSessionExpired) {
		t.Fatalf("err = %v", err)
	}
}

func TestSendText(t *testing.T) {
	c, srv := newClient(t)
	id, err := c.SendText(context.Background(), ilinktest.OwnerID, "ctx-1", "你好")
	if err != nil {
		t.Fatal(err)
	}
	sent := srv.Sent()
	if len(sent) != 1 || id == "" || sent[0] != (ilinktest.Sent{To: ilinktest.OwnerID, ContextToken: "ctx-1", Text: "你好", MsgID: id}) {
		t.Fatalf("id=%q sent = %+v", id, sent)
	}
	srv.SetSendRet(-2)
	_, err = c.SendText(context.Background(), ilinktest.OwnerID, "old", "x")
	var apiErr *ilink.APIError
	if !errors.As(err, &apiErr) || apiErr.Ret != -2 {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadDecryptsAllKeyFormats(t *testing.T) {
	c, srv := newClient(t)
	key := []byte("0123456789abcdef")
	plain := []byte("\xff\xd8\xff fake jpeg")
	enc, _ := ilink.EncryptECB(plain, key)
	srv.AddMedia("p1", enc)
	sum := md5.Sum(plain)
	for name, raw := range map[string]string{
		"image": ilinktest.ImageMsg(1, ilinktest.OwnerID, "p1", key),
		"voice": ilinktest.VoiceMsg(2, ilinktest.OwnerID, "p1", key, "转写"),
		"file":  ilinktest.FileMsg(3, ilinktest.OwnerID, "p1", key, "a.pdf", hex.EncodeToString(sum[:])),
	} {
		srv.Push(raw)
		u, err := c.GetUpdates(context.Background(), "")
		if err != nil || len(u.Msgs) != 1 {
			t.Fatalf("%s: %v %+v", name, err, u)
		}
		got, err := c.Download(context.Background(), u.Msgs[0].Items[0])
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("%s: download err=%v got=%q", name, err, got)
		}
	}
}

func TestDownloadMissingMedia(t *testing.T) {
	c, srv := newClient(t)
	srv.Push(ilinktest.ImageMsg(1, ilinktest.OwnerID, "nope", []byte("0123456789abcdef")))
	u, _ := c.GetUpdates(context.Background(), "")
	if _, err := c.Download(context.Background(), u.Msgs[0].Items[0]); err == nil {
		t.Fatal("want 404 error")
	}
}

func TestLenientIDsAndBadMessageDoNotBlockBatch(t *testing.T) {
	c, srv := newClient(t)
	srv.Push(`{"seq":1,"message_id":"abc","item_list":[{"type":4,"file_item":{"len":""}},{"type":1,"ref_msg":{"svr_id":"abc"}}]}`)
	srv.Push(`{"seq":2,"message_id":2,"item_list":"oops"}`)
	srv.Push(ilinktest.TextMsg(3, ilinktest.OwnerID, "ok"))
	u, err := c.GetUpdates(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Msgs) != 2 || len(u.Undecodable) != 1 || u.Buf != "buf-1" {
		t.Fatalf("msgs=%d undecodable=%d buf=%q", len(u.Msgs), len(u.Undecodable), u.Buf)
	}
	m := u.Msgs[0]
	if m.Items[0].File.Len != "" || m.Items[1].RefMsg.SvrID != "abc" || m.ID() != "abc" || len(m.Raw) == 0 {
		t.Fatalf("msg = %+v", m)
	}
	if !bytes.Contains(u.Undecodable[0], []byte("oops")) || u.Msgs[1].ID() != "3" {
		t.Fatalf("undecodable=%s second=%+v", u.Undecodable[0], u.Msgs[1])
	}
}
