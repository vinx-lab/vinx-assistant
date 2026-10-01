package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func newTagItem(t *testing.T, st *Store, msg string) int64 {
	t.Helper()
	it := &model.Item{MsgID: msg, RawText: "x " + msg}
	id, err := st.InsertItem(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAddTagsMergesWithoutDuplicates(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	id := newTagItem(t, st, "m1")
	if err := st.AddTags(ctx, id, TagKindTopic, []string{"待办", "发票"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTags(ctx, id, TagKindTopic, []string{"#待办", "报销"}); err != nil {
		t.Fatal(err)
	}
	it, _ := st.GetItem(ctx, id)
	if len(it.Topics) != 3 {
		t.Fatalf("tags = %v", it.Topics)
	}
	if err := st.AddTags(ctx, id, TagKindTopic, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSetTagsReplaces(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	id := newTagItem(t, st, "m1")
	if err := st.AddTags(ctx, id, TagKindTopic, []string{"待办", "发票"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTags(ctx, id, TagKindTopic, []string{"点子"}); err != nil {
		t.Fatal(err)
	}
	it, _ := st.GetItem(ctx, id)
	if !reflect.DeepEqual(it.Topics, []string{"点子"}) {
		t.Fatalf("tags = %v", it.Topics)
	}
}

func TestAllTagsCountsAndOrder(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	a, b := newTagItem(t, st, "m1"), newTagItem(t, st, "m2")
	if err := st.AddTags(ctx, a, TagKindTopic, []string{"待办", "点子"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTags(ctx, b, TagKindTopic, []string{"待办"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.AllTags(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []TagCount{{"待办", TagKindTopic, 2}, {"点子", TagKindTopic, 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestTagsAreIndexedInFTS(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	id := newTagItem(t, st, "m1")
	if err := st.AddTags(ctx, id, TagKindTopic, []string{"待办"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM items_fts WHERE rowid = ? AND tags LIKE '%待办%'`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if err := st.SetTags(ctx, id, TagKindTopic, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT count(*) FROM items_fts WHERE rowid = ? AND tags LIKE '%待办%'`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after clear n=%d err=%v", n, err)
	}
}
