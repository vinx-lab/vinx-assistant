package ingest

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/ilink/ilinktest"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/notify"
	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

type env struct {
	srv    *ilinktest.Server
	st     *store.Store
	sess   *session.Session
	clk    *clock.Fake
	svc    *Service
	media  string
	dbPath string
}

func newEnv(t *testing.T, mod func(*Deps)) *env {
	t.Helper()
	srv := ilinktest.New()
	t.Cleanup(srv.Close)
	dbPath := filepath.Join(t.TempDir(), "t.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := clock.NewFake(clock.At(2026, 10, 1, 9, 0))
	st.SetClock(clk)
	sess := session.New(st, nil, clk)
	sess.NewClient = func(c ilink.Cred) *ilink.Client {
		cl := ilink.New(c, nil)
		cl.CDNBaseURL = srv.URL
		return cl
	}
	sess.SaveCred(context.Background(), srv.Cred())
	d := Deps{Store: st, Session: sess, Notifier: notify.NewWeChat(sess, st), Clock: clk, MediaDir: t.TempDir()}
	if mod != nil {
		mod(&d)
	}
	return &env{srv: srv, st: st, sess: sess, clk: clk, svc: New(d), media: d.MediaDir, dbPath: dbPath}
}

func (e *env) handle(t *testing.T, raw string) {
	t.Helper()
	var m ilink.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
}

func (e *env) item(t *testing.T, id int64) *model.Item {
	t.Helper()
	it, err := e.st.GetItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func TestHandleTextWithPrefixAndAck(t *testing.T) {
	e := newEnv(t, nil)
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "待办：月底前交发票 https://example.com/a，看看"))
	it := e.item(t, 1)
	if it.Category != model.CatTodo || it.CategoryBy != model.ByPrefix || it.Status != model.StatusOpen {
		t.Fatalf("item = %+v", it)
	}
	if it.RawText != "月底前交发票 https://example.com/a，看看" || it.URL != "https://example.com/a" {
		t.Fatalf("text=%q url=%q", it.RawText, it.URL)
	}
	if strings.Contains(it.RawJSON, "ctx-1") || it.RawJSON == "" {
		t.Fatalf("raw_json not redacted: %s", it.RawJSON)
	}
	if tok, _, ok, _ := e.sess.Context(context.Background()); !ok || tok != "ctx-1" {
		t.Fatalf("context token = %q", tok)
	}
	e.svc.FlushAcks(context.Background())
	if len(e.srv.Sent()) != 0 {
		t.Fatal("ack sent before quiet period")
	}
	e.clk.Advance(6 * time.Second)
	e.svc.FlushAcks(context.Background())
	sent := e.srv.Sent()
	if len(sent) != 1 || !strings.HasPrefix(sent[0].Text, "✓ 已收：待办｜月底前交发票") {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestHandleKeywordsBecomeTags(t *testing.T) {
	e := newEnv(t, nil)
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "这个想法也算待办"))
	it := e.item(t, 1)
	if !reflect.DeepEqual(it.Tags, []string{"待办", "点子"}) || it.Category != model.CatIdea {
		t.Fatalf("cat=%s tags=%v", it.Category, it.Tags)
	}
}

func TestTagFailureDoesNotBreakIngest(t *testing.T) {
	e := newEnv(t, nil)
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER no_tags BEFORE INSERT ON item_tags BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef")
	enc, _ := ilink.EncryptECB([]byte("voice-bytes"), key)
	e.srv.AddMedia("v1", enc)
	e.handle(t, ilinktest.VoiceMsg(1, ilinktest.OwnerID, "v1", key, "待办：交发票"))
	var id int64
	var cat string
	if err := db.QueryRow(`SELECT id, category FROM items WHERE msg_id = '1'`).Scan(&id, &cat); err != nil || cat != string(model.CatTodo) {
		t.Fatalf("cat=%q err=%v", cat, err)
	}
	if atts, _ := e.st.ListAttachments(context.Background(), id); len(atts) != 1 {
		t.Fatalf("atts = %+v", atts)
	}
	e.clk.Advance(6 * time.Second)
	e.svc.FlushAcks(context.Background())
	if len(e.srv.Sent()) != 1 {
		t.Fatalf("receipt not queued: %+v", e.srv.Sent())
	}
}

func TestFlushAllAcksSendsImmediately(t *testing.T) {
	e := newEnv(t, nil)
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "待办：交发票"))
	e.svc.FlushAcks(context.Background())
	if len(e.srv.Sent()) != 0 {
		t.Fatal("regular flush sent inside quiet period")
	}
	e.svc.FlushAllAcks(context.Background())
	sent := e.srv.Sent()
	if len(sent) != 1 || !strings.HasPrefix(sent[0].Text, "✓ 已收：待办｜交发票") {
		t.Fatalf("sent = %+v", sent)
	}
	e.svc.FlushAllAcks(context.Background())
	if len(e.srv.Sent()) != 1 {
		t.Fatal("flushed twice")
	}
}

func TestHandleDuplicateIgnored(t *testing.T) {
	e := newEnv(t, nil)
	raw := ilinktest.TextMsg(1, ilinktest.OwnerID, "hello")
	e.handle(t, raw)
	e.handle(t, raw)
	if _, err := e.st.GetItem(context.Background(), 2); err == nil {
		t.Fatal("duplicate created a second item")
	}
}

func TestHandleIgnoresStrangers(t *testing.T) {
	e := newEnv(t, nil)
	e.handle(t, ilinktest.TextMsg(1, "stranger@im.wechat", "hi"))
	if _, err := e.st.GetItem(context.Background(), 1); err == nil {
		t.Fatal("stranger message stored")
	}
	e.clk.Advance(time.Minute)
	e.svc.FlushAcks(context.Background())
	if len(e.srv.Sent()) != 0 {
		t.Fatal("acked a stranger")
	}
}

func TestHandleImageArchivedAndSaved(t *testing.T) {
	e := newEnv(t, nil)
	key := []byte("0123456789abcdef")
	plain := []byte("\xff\xd8\xff jpeg")
	enc, _ := ilink.EncryptECB(plain, key)
	e.srv.AddMedia("img", enc)
	e.handle(t, ilinktest.ImageMsg(1, ilinktest.OwnerID, "img", key))
	it := e.item(t, 1)
	if it.Category != model.CatArchive {
		t.Fatalf("category = %s", it.Category)
	}
	atts, _ := e.st.ListAttachments(context.Background(), 1)
	if len(atts) != 1 || atts[0].State != "ok" || atts[0].RelPath != "2026/10/1-0.jpg" {
		t.Fatalf("atts = %+v", atts)
	}
	if b, _ := os.ReadFile(filepath.Join(e.media, atts[0].RelPath)); string(b) != string(plain) {
		t.Fatal("saved bytes differ")
	}
}

func TestHandleFileKeepsNameAndChecksMD5(t *testing.T) {
	e := newEnv(t, nil)
	key := []byte("0123456789abcdef")
	plain := []byte("%PDF-1.7")
	enc, _ := ilink.EncryptECB(plain, key)
	e.srv.AddMedia("f", enc)
	sum := md5.Sum(plain)
	e.handle(t, ilinktest.FileMsg(1, ilinktest.OwnerID, "f", key, "合同.pdf", hex.EncodeToString(sum[:])))
	it := e.item(t, 1)
	if it.RawText != "[文件] 合同.pdf" {
		t.Fatalf("raw_text = %q", it.RawText)
	}
	atts, _ := e.st.ListAttachments(context.Background(), 1)
	if atts[0].FileName != "合同.pdf" || atts[0].State != "ok" || atts[0].MD5 != hex.EncodeToString(sum[:]) {
		t.Fatalf("att = %+v", atts[0])
	}
}

func TestDownloadFailureKeepsItemAndRetries(t *testing.T) {
	e := newEnv(t, nil)
	key := []byte("0123456789abcdef")
	e.handle(t, ilinktest.ImageMsg(1, ilinktest.OwnerID, "later", key))
	atts, _ := e.st.ListAttachments(context.Background(), 1)
	if len(atts) != 1 || atts[0].State != "pending" || atts[0].Attempts != 1 {
		t.Fatalf("att = %+v", atts)
	}
	e.clk.Advance(6 * time.Second)
	e.svc.FlushAcks(context.Background())
	if len(e.srv.Sent()) != 1 {
		t.Fatal("ack must still be sent when download fails")
	}
	enc, _ := ilink.EncryptECB([]byte("\xff\xd8\xff ok"), key)
	e.srv.AddMedia("later", enc)
	e.svc.RetryAttachments(context.Background())
	atts, _ = e.st.ListAttachments(context.Background(), 1)
	if atts[0].State != "ok" || atts[0].RelPath == "" {
		t.Fatalf("after retry = %+v", atts[0])
	}
}

func TestVoiceUsesTranscript(t *testing.T) {
	e := newEnv(t, nil)
	key := []byte("0123456789abcdef")
	enc, _ := ilink.EncryptECB([]byte("#!SILK_V3"), key)
	e.srv.AddMedia("v", enc)
	e.handle(t, ilinktest.VoiceMsg(1, ilinktest.OwnerID, "v", key, "待办，明天交发票"))
	it := e.item(t, 1)
	if it.Category != model.CatTodo || it.RawText != "明天交发票" {
		t.Fatalf("item = %+v", it)
	}
}

type fakeCmd struct {
	handled bool
	got     []CommandInput
}

func (f *fakeCmd) Handle(ctx context.Context, in CommandInput) (string, bool, error) {
	f.got = append(f.got, in)
	return "已完成 #12", f.handled, nil
}

func TestCommandHandledIsNotStored(t *testing.T) {
	cmd := &fakeCmd{handled: true}
	e := newEnv(t, func(d *Deps) { d.Commands = cmd })
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "完成 12"))
	if _, err := e.st.GetItem(context.Background(), 1); err == nil {
		t.Fatal("command stored as item")
	}
	if sent := e.srv.Sent(); len(sent) != 1 || sent[0].Text != "已完成 #12" {
		t.Fatalf("reply = %+v", sent)
	}
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "完成 12"))
	if len(cmd.got) != 1 {
		t.Fatal("replayed command executed twice")
	}
}

func TestCommandNotHandledBecomesItem(t *testing.T) {
	cmd := &fakeCmd{handled: false}
	e := newEnv(t, func(d *Deps) { d.Commands = cmd })
	e.handle(t, ilinktest.RefTextMsg(1, ilinktest.OwnerID, "这个也要研究一下", "提醒 #12"))
	if len(cmd.got) != 1 || cmd.got[0].RefText != "提醒 #12" || !cmd.got[0].HasRef {
		t.Fatalf("cmd input = %+v", cmd.got)
	}
	if it := e.item(t, 1); it.Level != model.LevelMedium {
		t.Fatalf("item = %+v", it)
	}
}

type fakeDeferred struct {
	mu        sync.Mutex
	committed []bool
}

func (f *fakeDeferred) TakeDeferred(ctx context.Context) (string, func(context.Context, bool) error, error) {
	return "⏰ 补发：交发票 #3", func(_ context.Context, sent bool) error {
		f.mu.Lock()
		f.committed = append(f.committed, sent)
		f.mu.Unlock()
		return nil
	}, nil
}

func TestAckCarriesDeferred(t *testing.T) {
	def := &fakeDeferred{}
	e := newEnv(t, func(d *Deps) { d.Deferred = def })
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "hello"))
	e.clk.Advance(6 * time.Second)
	e.svc.FlushAcks(context.Background())
	sent := e.srv.Sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Text, "⏰ 补发：交发票 #3") {
		t.Fatalf("sent = %+v", sent)
	}
	if len(def.committed) != 1 || !def.committed[0] {
		t.Fatalf("committed = %v", def.committed)
	}
}

type fakeEnricher struct {
	mu   sync.Mutex
	urls []string
}

func (f *fakeEnricher) Enrich(ctx context.Context, id int64, url string) {
	f.mu.Lock()
	f.urls = append(f.urls, url)
	f.mu.Unlock()
}

func TestEnricherCalledForURL(t *testing.T) {
	en := &fakeEnricher{}
	e := newEnv(t, func(d *Deps) { d.Enricher = en })
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "看看 https://github.com/x/y"))
	e.handle(t, ilinktest.TextMsg(2, ilinktest.OwnerID, "没有链接"))
	if len(en.urls) != 1 || en.urls[0] != "https://github.com/x/y" {
		t.Fatalf("urls = %v", en.urls)
	}
}

func TestRefBySvrIDResolvesOurSentMessage(t *testing.T) {
	cmd := &fakeCmd{handled: true}
	e := newEnv(t, func(d *Deps) { d.Commands = cmd })
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "待办：交发票"))
	e.clk.Advance(6 * time.Second)
	e.svc.FlushAcks(context.Background())
	ack := e.srv.Sent()[0]
	e.handle(t, ilinktest.RefSvrMsg(2, ilinktest.OwnerID, "完成", ack.MsgID))
	if len(cmd.got) != 1 || cmd.got[0].RefText != ack.Text {
		t.Fatalf("cmd input = %+v, want RefText %q", cmd.got, ack.Text)
	}
	e.handle(t, ilinktest.RefSvrMsg(3, ilinktest.OwnerID, "完成", "424242"))
	if cmd.got[1].RefText != "" || !cmd.got[1].HasRef {
		t.Fatalf("unknown svr_id: %+v", cmd.got[1])
	}
}

func TestRefBySvrIDResolvesOwnItem(t *testing.T) {
	cmd := &fakeCmd{handled: true}
	e := newEnv(t, func(d *Deps) { d.Commands = cmd })
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "待办：交发票"))
	e.handle(t, ilinktest.RefSvrMsg(2, ilinktest.OwnerID, "完成", "1"))
	if cmd.got[0].RefText != "交发票" {
		t.Fatalf("RefText = %q", cmd.got[0].RefText)
	}
}

func TestDriftRecordedOnce(t *testing.T) {
	e := newEnv(t, nil)
	raw := strings.Replace(ilinktest.TextMsg(1, ilinktest.OwnerID, "x"), `"seq":1`, `"seq":1,"brand_new":true`, 1)
	e.handle(t, raw)
	v, ok, _ := e.st.GetKV(context.Background(), keyDrift)
	if !ok || !strings.Contains(v, "msg.brand_new") {
		t.Fatalf("drift kv = %q", v)
	}
	if _, err := e.st.GetItem(context.Background(), 1); err != nil {
		t.Fatal("message with unknown field must still be stored")
	}
}

func TestFileWithoutKeyFailsImmediately(t *testing.T) {
	e := newEnv(t, nil)
	raw := `{"seq":1,"message_id":1,"from_user_id":"` + ilinktest.OwnerID + `","message_type":1,"context_token":"c","item_list":[{"type":4,"file_item":{"media":{"encrypt_query_param":"x"},"file_name":"a.pdf"}}]}`
	e.handle(t, raw)
	atts, _ := e.st.ListAttachments(context.Background(), 1)
	if len(atts) != 1 || atts[0].State != "failed" {
		t.Fatalf("att = %+v", atts)
	}
}

func TestOwnerWithoutUserIDRejected(t *testing.T) {
	e := newEnv(t, nil)
	c := e.srv.Cred()
	c.UserID = ""
	if err := e.sess.SaveCred(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "hi"))
	if _, err := e.st.GetItem(context.Background(), 1); err == nil {
		t.Fatal("message accepted although cred has no user id")
	}
}

func TestCommandErrorReplyIsGeneric(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Commands = errCmd{} })
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "完成 12"))
	sent := e.srv.Sent()
	if len(sent) != 1 || sent[0].Text != "指令执行失败，请稍后再试" {
		t.Fatalf("sent = %+v", sent)
	}
}

type errCmd struct{}

func (errCmd) Handle(context.Context, CommandInput) (string, bool, error) {
	return "", false, errors.New("secret internal detail")
}

func TestRetryAttachmentsDuringPauseKeepsAttempts(t *testing.T) {
	e := newEnv(t, nil)
	key := []byte("0123456789abcdef")
	e.handle(t, ilinktest.ImageMsg(1, ilinktest.OwnerID, "later", key))
	if err := e.sess.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		e.svc.RetryAttachments(context.Background())
	}
	atts, _ := e.st.ListAttachments(context.Background(), 1)
	if atts[0].State != "pending" || atts[0].Attempts != 1 {
		t.Fatalf("att during pause = %+v", atts[0])
	}
	enc, _ := ilink.EncryptECB([]byte("\xff\xd8\xff ok"), key)
	e.srv.AddMedia("later", enc)
	e.clk.Advance(session.PauseDuration)
	e.svc.RetryAttachments(context.Background())
	atts, _ = e.st.ListAttachments(context.Background(), 1)
	if atts[0].State != "ok" {
		t.Fatalf("after resume = %+v", atts[0])
	}
}

func TestRefByMessageItemMsgIDResolvesOurSentMessage(t *testing.T) {
	cmd := &fakeCmd{handled: true}
	e := newEnv(t, func(d *Deps) { d.Commands = cmd })
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "待办：交发票"))
	e.clk.Advance(6 * time.Second)
	e.svc.FlushAcks(context.Background())
	ack := e.srv.Sent()[0]
	e.handle(t, ilinktest.RefMsgIDMsg(2, ilinktest.OwnerID, "完成", ack.MsgID))
	if len(cmd.got) != 1 || cmd.got[0].RefText != ack.Text || !cmd.got[0].HasRef {
		t.Fatalf("cmd input = %+v, want RefText %q", cmd.got, ack.Text)
	}
	e.handle(t, ilinktest.RefMsgIDMsg(3, ilinktest.OwnerID, "完成", "424242"))
	if cmd.got[1].RefText != "" || !cmd.got[1].HasRef {
		t.Fatalf("unknown msg_id: %+v", cmd.got[1])
	}
}
