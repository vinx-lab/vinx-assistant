package store

import (
	"context"
	"errors"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestDeleteItemRemovesAssociations(t *testing.T) {
	st, fc := openTest(t)
	ctx := context.Background()
	id, err := st.InsertItem(ctx, &model.Item{MsgID: "m1", RawText: "报销发票", Category: model.CatTodo})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := st.InsertItem(ctx, &model.Item{MsgID: "m2", RawText: "另一条发票"})
	if err := st.SetTags(ctx, id, TagKindLabel, []string{"待办"}); err != nil {
		t.Fatal(err)
	}
	st.SetTags(ctx, other, TagKindLabel, []string{"待办"})
	for _, a := range []*model.Attachment{
		{ItemID: id, Kind: "image", RelPath: "2026/10/1-0.jpg", State: "ok"},
		{ItemID: id, Kind: "file", RelPath: "2026/10/1-1.pdf", State: "ok"},
		{ItemID: id, Kind: "file", State: "pending"}, // 还没下载，没有路径
		{ItemID: other, Kind: "image", RelPath: "2026/10/2-0.jpg", State: "ok"},
	} {
		if _, err := st.InsertAttachment(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.InsertReminder(ctx, &Reminder{Kind: "due", ItemID: id, ScheduledAt: clock.At(2026, 10, 2, 9, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAction(ctx, &Action{MsgID: "a1", Command: "done", ItemID: id, Before: "{}", After: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddUsage(ctx, Usage{Day: "2026-10-01", Level: "light", Model: "m", PromptTokens: 5, ItemID: id}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkSeen(ctx, "m1", fc.Now()); err != nil {
		t.Fatal(err)
	}

	paths, err := st.DeleteItem(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "2026/10/1-0.jpg" || paths[1] != "2026/10/1-1.pdf" {
		t.Fatalf("paths = %v", paths)
	}
	count := func(q string, args ...any) int {
		var n int
		if err := st.db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, q := range []string{
		`SELECT count(*) FROM items WHERE id = ?`,
		`SELECT count(*) FROM item_tags WHERE item_id = ?`,
		`SELECT count(*) FROM attachments WHERE item_id = ?`,
		`SELECT count(*) FROM reminders WHERE item_id = ?`,
		`SELECT count(*) FROM actions WHERE item_id = ?`,
		`SELECT count(*) FROM items_fts WHERE rowid = ?`,
		`SELECT count(*) FROM llm_usage WHERE item_id = ?`,
	} {
		if n := count(q, id); n != 0 {
			t.Errorf("%s 残留 %d 行", q, n)
		}
	}
	if n := count(`SELECT count(*) FROM actions`); n != 0 {
		t.Errorf("actions 应为空，有 %d 行", n)
	}
	if n := count(`SELECT count(*) FROM llm_usage`); n != 1 {
		t.Errorf("用量记录应保留（item_id 置空），有 %d 行", n)
	}
	if n := count(`SELECT count(*) FROM seen_msgs WHERE msg_id = 'm1'`); n != 1 {
		t.Errorf("seen_msgs 应保留，有 %d 行", n)
	}
	if n := count(`SELECT count(*) FROM tags WHERE name = '待办'`); n != 1 {
		t.Errorf("tags 表不应动，有 %d 行", n)
	}
	// 别的条目不受影响
	if n := count(`SELECT count(*) FROM items WHERE id = ?`, other) + count(`SELECT count(*) FROM attachments WHERE item_id = ?`, other) +
		count(`SELECT count(*) FROM item_tags WHERE item_id = ?`, other) + count(`SELECT count(*) FROM items_fts WHERE rowid = ?`, other); n != 4 {
		t.Errorf("其他条目受影响：%d", n)
	}

	if _, err := st.DeleteItem(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除：%v", err)
	}
	if seen, _ := st.Seen(ctx, "m1"); !seen {
		t.Error("Seen 应仍为 true")
	}
}
