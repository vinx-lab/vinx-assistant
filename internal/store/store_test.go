package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func openTest(t *testing.T) (*Store, *clock.Fake) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fc := clock.NewFake(clock.At(2026, 10, 1, 9, 0))
	st.SetClock(fc)
	return st, fc
}

func TestOpenTwiceRunsMigrationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	for i := 0; i < 2; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		st.Close()
	}
}

func TestItemRoundTrip(t *testing.T) {
	st, fc := openTest(t)
	ctx := context.Background()
	due := clock.At(2026, 10, 8, 15, 0)
	it := &model.Item{MsgID: "m1", RawText: "交发票", Category: model.CatTodo, CategoryBy: model.ByPrefix, DueAt: &due, DueHasTime: true}
	id, err := st.InsertItem(ctx, it)
	if err != nil || id == 0 || it.ID != id {
		t.Fatalf("insert: id=%d err=%v", id, err)
	}
	got, err := st.GetItem(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusOpen || got.Level != model.LevelLight || !got.DueAt.Equal(due) || !got.DueHasTime {
		t.Fatalf("got %+v", got)
	}
	if !got.CreatedAt.Equal(fc.Now()) {
		t.Fatalf("created_at = %v", got.CreatedAt)
	}

	fc.Advance(time.Hour)
	got.Title = "报销发票"
	got.DueAt = nil
	if err := st.UpdateItem(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ := st.GetItem(ctx, id)
	if again.Title != "报销发票" || again.DueAt != nil || !again.UpdatedAt.Equal(fc.Now()) {
		t.Fatalf("after update %+v", again)
	}
}

func TestInsertDuplicateMsgID(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	if _, err := st.InsertItem(ctx, &model.Item{MsgID: "dup"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertItem(ctx, &model.Item{MsgID: "dup"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

func TestGetAndUpdateMissing(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	if _, err := st.GetItem(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get err = %v", err)
	}
	if err := st.UpdateItem(ctx, &model.Item{ID: 99, Category: model.CatInbox, CategoryBy: model.ByAI, Level: model.LevelLight, Status: "new"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update err = %v", err)
	}
}

func TestFTSTrigramFindsChinese(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	id, _ := st.InsertItem(ctx, &model.Item{MsgID: "f1", RawText: "月底前处理报销发票"})
	if err := st.SetLink(ctx, id, "https://example.com", "示例网页标题", "简介"); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		var n int
		if err := st.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM items_fts WHERE items_fts MATCH ?`, `"报销发票"`); n != 1 {
		t.Fatalf("MATCH 报销发票 = %d", n)
	}
	if n := count(`SELECT count(*) FROM items_fts WHERE raw_text LIKE ?`, "%发票%"); n != 1 {
		t.Fatalf("LIKE 发票 = %d", n)
	}
	if n := count(`SELECT count(*) FROM items_fts WHERE items_fts MATCH ?`, `"网页标题"`); n != 1 {
		t.Fatalf("link_title not indexed: %d", n)
	}
}

func TestKVAndSeen(t *testing.T) {
	st, fc := openTest(t)
	ctx := context.Background()
	if _, ok, _ := st.GetKV(ctx, "k"); ok {
		t.Fatal("unexpected key")
	}
	st.SetKV(ctx, "k", "1")
	st.SetKV(ctx, "k", "2")
	if v, ok, _ := st.GetKV(ctx, "k"); !ok || v != "2" {
		t.Fatalf("kv = %q %v", v, ok)
	}
	if seen, _ := st.Seen(ctx, "m"); seen {
		t.Fatal("seen before mark")
	}
	if err := st.MarkSeen(ctx, "m", fc.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkSeen(ctx, "m", fc.Now()); err != nil {
		t.Fatalf("second mark must be no-op: %v", err)
	}
	if seen, _ := st.Seen(ctx, "m"); !seen {
		t.Fatal("not seen after mark")
	}
}

func TestAttachments(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	id, _ := st.InsertItem(ctx, &model.Item{MsgID: "a"})
	a := &model.Attachment{ItemID: id, Kind: "image", State: "pending", MediaJSON: `{"type":2}`}
	if _, err := st.InsertAttachment(ctx, a); err != nil {
		t.Fatal(err)
	}
	list, _ := st.RetryableAttachments(ctx, 5)
	if len(list) != 1 || list[0].MediaJSON != `{"type":2}` {
		t.Fatalf("retryable = %+v", list)
	}
	a.State, a.RelPath, a.Size = "ok", "2026/10/1-0.jpg", 3
	if err := st.UpdateAttachment(ctx, a); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.RetryableAttachments(ctx, 5); len(list) != 0 {
		t.Fatalf("still retryable: %+v", list)
	}
	all, _ := st.ListAttachments(ctx, id)
	if len(all) != 1 || all[0].RelPath != "2026/10/1-0.jpg" {
		t.Fatalf("list = %+v", all)
	}
}

func TestSettingsDefaultsAndRoundTrip(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	s, err := st.LoadSettings(ctx)
	if err != nil || s.Schedule.DigestTime != "09:00" {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	s.AI.Images = true
	s.AI.Providers = []model.Provider{{ID: "p1", Name: "x", BaseURL: "http://x", APIKey: "k"}}
	if err := st.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, _ := st.LoadSettings(ctx)
	if !got.AI.Images || len(got.AI.Providers) != 1 || got.Schedule.DigestTime != "09:00" {
		t.Fatalf("round trip: %+v", got)
	}
}

func TestSnapshot(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	st.InsertItem(ctx, &model.Item{MsgID: "s", RawText: "快照"})
	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := st.Snapshot(ctx, dst); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM items`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("snapshot items = %d err=%v", n, err)
	}
}

func TestSentMsgs(t *testing.T) {
	st, fc := openTest(t)
	ctx := context.Background()
	if err := st.RecordSent(ctx, "", "ignored"); err != nil {
		t.Fatal(err)
	}
	st.RecordSent(ctx, "900", "提醒 #12")
	if body, ok, _ := st.SentBody(ctx, "900"); !ok || body != "提醒 #12" {
		t.Fatalf("body=%q ok=%v", body, ok)
	}
	fc.Advance(31 * 24 * time.Hour)
	st.PruneSent(ctx, fc.Now().Add(-30*24*time.Hour))
	if _, ok, _ := st.SentBody(ctx, "900"); ok {
		t.Fatal("not pruned")
	}
	st.InsertItem(ctx, &model.Item{MsgID: "m-1", RawText: "原文"})
	if it, err := st.GetItemByMsgID(ctx, "m-1"); err != nil || it.RawText != "原文" {
		t.Fatalf("by msg id: %+v %v", it, err)
	}
}
