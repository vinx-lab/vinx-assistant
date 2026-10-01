package store

import (
	"context"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func seedBrowse(t *testing.T) (*Store, *clock.Fake) {
	t.Helper()
	st, fc := openTest(t)
	ctx := context.Background()
	d1, d2 := clock.At(2026, 10, 3, 0, 0), clock.At(2026, 10, 2, 15, 0)
	items := []*model.Item{
		{MsgID: "1", RawText: "月底前处理报销发票", Category: model.CatTodo, DueAt: &d1},
		{MsgID: "2", RawText: "交房租", Category: model.CatTodo, DueAt: &d2, DueHasTime: true},
		{MsgID: "3", RawText: "已经办完的事", Category: model.CatTodo, Status: model.StatusDone},
		{MsgID: "4", RawText: "看看 htmx 和 AND OR 语法", Category: model.CatResearch},
		{MsgID: "5", RawText: "100% 纯手工_说明", Category: model.CatIdea},
	}
	for _, it := range items {
		if _, err := st.InsertItem(ctx, it); err != nil {
			t.Fatal(err)
		}
		fc.Advance(time.Hour)
	}
	if err := st.SetTags(ctx, 4, []string{"前端"}); err != nil {
		t.Fatal(err)
	}
	return st, fc
}

func ids(items []model.Item) []int64 {
	var out []int64
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func TestListItemsAndCounts(t *testing.T) {
	st, _ := seedBrowse(t)
	ctx := context.Background()
	todo, err := st.ListItems(ctx, ListQuery{Category: model.CatTodo})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(todo); len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("open todos by due = %v", got)
	}
	all, _ := st.ListItems(ctx, ListQuery{Category: model.CatTodo, All: true})
	if len(all) != 3 {
		t.Fatalf("all todos = %v", ids(all))
	}
	tagged, _ := st.ListItems(ctx, ListQuery{Category: model.CatResearch, Tag: "前端"})
	if len(tagged) != 1 || tagged[0].Tags[0] != "前端" {
		t.Fatalf("tagged = %+v", tagged)
	}
	counts, _ := st.CategoryCounts(ctx, false)
	if counts[model.CatTodo] != 2 || counts[model.CatResearch] != 1 || counts[model.CatIdea] != 0 {
		t.Fatalf("open counts = %v", counts)
	}
	allCounts, _ := st.CategoryCounts(ctx, true)
	if allCounts[model.CatTodo] != 3 || allCounts[model.CatIdea] != 1 {
		t.Fatalf("all counts = %v", allCounts)
	}
}

func TestSearch(t *testing.T) {
	st, fc := seedBrowse(t)
	ctx := context.Background()
	cases := []struct {
		q    SearchQuery
		want []int64
	}{
		{SearchQuery{Q: "发票"}, []int64{1}},     // 两个字：走 LIKE
		{SearchQuery{Q: "报销发票"}, []int64{1}},   // 三个字以上：走 MATCH
		{SearchQuery{Q: "AND OR"}, []int64{4}}, // FTS 关键字按字面
		{SearchQuery{Q: `"htmx`}, nil},         // 不成对的引号不报错
		{SearchQuery{Q: "100%"}, []int64{5}},   // % 按字面
		{SearchQuery{Q: "_"}, []int64{5}},      // _ 按字面
		{SearchQuery{Q: "前端"}, []int64{4}},     // 标签也能搜
		{SearchQuery{Category: model.CatTodo, Status: model.StatusDone}, []int64{3}},
		{SearchQuery{From: fc.Now().Add(-2 * time.Hour)}, []int64{5, 4}},
		{SearchQuery{To: clock.At(2026, 10, 1, 10, 0)}, []int64{1}},
	}
	for _, c := range cases {
		got, err := st.Search(ctx, c.q)
		if err != nil {
			t.Fatalf("%+v: %v", c.q, err)
		}
		if g := ids(got); len(g) != len(c.want) || (len(g) > 0 && g[0] != c.want[0]) {
			t.Errorf("Search(%+v) = %v, want %v", c.q, g, c.want)
		}
	}
}

func TestFTSPhrase(t *testing.T) {
	if got := FTSPhrase(`a "b" c`); got != `"a ""b"" c"` {
		t.Fatalf("got %s", got)
	}
}

func TestAttachmentByPathAndFirstImages(t *testing.T) {
	st, _ := seedBrowse(t)
	ctx := context.Background()
	st.InsertAttachment(ctx, &model.Attachment{ItemID: 4, Kind: "file", RelPath: "2026/10/4-0-a.pdf", FileName: "a.pdf", State: "ok"})
	st.InsertAttachment(ctx, &model.Attachment{ItemID: 4, Kind: "image", RelPath: "2026/10/4-1.jpg", State: "ok"})
	a, err := st.AttachmentByPath(ctx, "2026/10/4-0-a.pdf")
	if err != nil || a.FileName != "a.pdf" {
		t.Fatalf("att=%+v err=%v", a, err)
	}
	if _, err := st.AttachmentByPath(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("err = %v", err)
	}
	m, _ := st.FirstImages(ctx, []int64{1, 4})
	if len(m) != 1 || m[4] != "2026/10/4-1.jpg" {
		t.Fatalf("first images = %v", m)
	}
}
