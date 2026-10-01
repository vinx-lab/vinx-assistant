package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestItemPageAndEdit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.item(t, &model.Item{MsgID: "1", RawText: "合同", Category: model.CatInbox})
	code, body := e.get(t, "/items/1")
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	mustContain(t, body, "#1 合同", "未整理")
	resp, _ := e.post(t, "/items/1", withSeen(t, e, 1, url.Values{"category": {"todo"}, "tags": {"#合同、 法务,合同"}, "due_date": {"2026-10-08"}, "due_time": {""}, "priority": {"high"}}))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/items/1?msg=saved" {
		t.Fatalf("save code %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	it, _ := e.st.GetItem(ctx, id)
	if it.Category != model.CatTodo || it.CategoryBy != model.ByManual || it.Status != model.StatusOpen ||
		it.Priority != model.PriorityHigh || it.DueHasTime || !it.DueAt.Equal(clock.At(2026, 10, 8, 0, 0)) || len(it.Tags) != 2 {
		t.Fatalf("after save %+v", it)
	}
	resp, body = e.post(t, "/items/1", withSeen(t, e, 1, url.Values{"category": {"todo"}, "due_time": {"15:00"}, "priority": {""}}))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "填了时刻就要填日期") {
		t.Fatalf("bad edit: %d", resp.StatusCode)
	}
	if again, _ := e.st.GetItem(ctx, id); !again.DueAt.Equal(clock.At(2026, 10, 8, 0, 0)) || len(again.Tags) != 2 {
		t.Fatal("failed edit must not save")
	}
	// 已整理的条目，分类下拉里没有「未整理」，提交 inbox 也被拒绝。
	_, body = e.get(t, "/items/1")
	mustNotContain(t, body, `value="inbox"`)
	if resp, _ := e.post(t, "/items/1", withSeen(t, e, 1, url.Values{"category": {"inbox"}})); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("manual inbox accepted: %d", resp.StatusCode)
	}
	if again, _ := e.st.GetItem(ctx, id); again.Category != model.CatTodo {
		t.Fatalf("category = %s", again.Category)
	}
	if resp, _ := e.post(t, "/items/99", url.Values{"category": {"todo"}}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing item save code %d", resp.StatusCode)
	}
}

// 还在未整理里的条目只改标签时保持未整理（下拉默认选中当前分类）。
func TestItemInboxKeepsInbox(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.item(t, &model.Item{MsgID: "1", RawText: "合同", Category: model.CatInbox})
	_, body := e.get(t, "/items/1")
	mustContain(t, body, `<option value="inbox" selected>`)
	resp, _ := e.post(t, "/items/1", withSeen(t, e, 1, url.Values{"category": {"inbox"}, "tags": {"法务"}}))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("code %d", resp.StatusCode)
	}
	it, _ := e.st.GetItem(ctx, id)
	if it.Category != model.CatInbox || it.CategoryBy == model.ByManual || len(it.Tags) != 1 {
		t.Fatalf("%+v", it)
	}
}

func TestItemDetailMarkdownAndAttachments(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.item(t, &model.Item{MsgID: "1", RawText: "资料", Category: model.CatResearch, Detail: "## 要点\n\n- **一**"})
	for _, a := range []*model.Attachment{
		{ItemID: id, Kind: "file", RelPath: "2026/10/1-0-合同.pdf", FileName: "合同.pdf", State: "ok"},
		{ItemID: id, Kind: "image", RelPath: "2026/10/1-1.jpg", State: "ok"},
		{ItemID: id, Kind: "video", State: "failed", LastError: "超时"},
	} {
		if _, err := e.st.InsertAttachment(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	_, body := e.get(t, "/items/1")
	mustContain(t, body, "<h2>要点</h2>", "<strong>一</strong>", `href="/media/2026/10/1-0-%e5%90%88%e5%90%8c.pdf"`, "合同.pdf",
		`src="/media/2026/10/1-1.jpg"`, "下载失败：超时")
}

func TestApplyEdit(t *testing.T) {
	base := func() *model.Item { return &model.Item{Category: model.CatResearch, Status: model.StatusDoing} }
	cases := []struct {
		name   string
		f      editForm
		err    bool
		status string
		due    string
	}{
		{"研究中改待办，状态重置为待办", editForm{Category: "todo"}, false, model.StatusOpen, ""},
		{"同分类保留状态", editForm{Category: "research"}, false, model.StatusDoing, ""},
		{"日期加时刻", editForm{Category: "research", DueDate: "2026-12-31", DueTime: "23:30"}, false, model.StatusDoing, "2026-12-31 23:30"},
		{"非法分类", editForm{Category: "foo"}, true, "", ""},
		{"不能手动改回未整理", editForm{Category: "inbox"}, true, "", ""},
		{"非法日期", editForm{Category: "todo", DueDate: "2026-02-30"}, true, "", ""},
		{"非法优先级", editForm{Category: "todo", Priority: "urgent"}, true, "", ""},
	}
	done := &model.Item{Category: model.CatResearch, Status: model.StatusDone}
	if _, err := applyEdit(done, editForm{Category: "todo"}); err != nil || done.Status != model.StatusDone || done.CategoryBy != model.ByManual {
		t.Errorf("done 在待办里也合法，应保留：%+v", done)
	}
	for _, c := range cases {
		it := base()
		_, err := applyEdit(it, c.f)
		if (err != nil) != c.err {
			t.Errorf("%s: err=%v", c.name, err)
			continue
		}
		if c.err {
			if it.Category != model.CatResearch {
				t.Errorf("%s: item modified on error", c.name)
			}
			continue
		}
		if it.Status != c.status {
			t.Errorf("%s: status=%s", c.name, it.Status)
		}
		if c.due != "" && (it.DueAt == nil || it.DueAt.Format("2006-01-02 15:04") != c.due || !it.DueHasTime) {
			t.Errorf("%s: due=%v", c.name, it.DueAt)
		}
	}
}

// withSeen 给详情页表单补上打开页面时条目的 updated_at（页面里的隐藏字段）。
func withSeen(t *testing.T, e *env, id int64, v url.Values) url.Values {
	t.Helper()
	it, err := e.st.GetItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	v.Set("updated_at", strconv.FormatInt(it.UpdatedAt.Unix(), 10))
	return v
}

func TestItemSaveRejectsStaleForm(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.item(t, &model.Item{MsgID: "1", RawText: "交发票", Category: model.CatTodo})
	_, body := e.get(t, "/items/1")
	it, _ := e.st.GetItem(ctx, id)
	seen := strconv.FormatInt(it.UpdatedAt.Unix(), 10)
	mustContain(t, body, `name="updated_at" value="`+seen+`"`)

	// 页面打开期间，微信指令把条目标成完成、改了截止
	e.clk.Advance(time.Minute)
	due := clock.At(2026, 10, 9, 0, 0)
	if _, err := e.st.ModifyItem(ctx, id, func(it *model.Item) error { it.Status, it.DueAt = model.StatusDone, &due; return nil }); err != nil {
		t.Fatal(err)
	}
	resp, body := e.post(t, "/items/1", url.Values{"updated_at": {seen}, "category": {"todo"}, "tags": {"法务"}, "due_date": {"2026-10-08"}, "priority": {"high"}})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "条目已被更新，请刷新后再改") {
		t.Fatalf("stale save: code %d", resp.StatusCode)
	}
	// 拒绝后表单换成最新值和新的 updated_at
	cur, _ := e.st.GetItem(ctx, id)
	mustContain(t, body, `value="2026-10-09"`, `name="updated_at" value="`+strconv.FormatInt(cur.UpdatedAt.Unix(), 10)+`"`)
	if cur.Status != model.StatusDone || !cur.DueAt.Equal(due) || cur.Priority == model.PriorityHigh || len(cur.Tags) != 0 {
		t.Fatalf("stale form overwrote item: %+v", cur)
	}
	// 缺少 updated_at 也拒绝
	if resp, _ := e.post(t, "/items/1", url.Values{"category": {"todo"}, "priority": {"high"}}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("missing updated_at: code %d", resp.StatusCode)
	}
	// 刷新后再改：成功
	if resp, _ := e.post(t, "/items/1", withSeen(t, e, id, url.Values{"category": {"todo"}, "due_date": {"2026-10-09"}, "priority": {"high"}})); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("fresh save: code %d", resp.StatusCode)
	}
}
