package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestMigration0004ClassifiesCategoryNamesAsLabels(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	// 还原成迁移前的状态：没有 kind 列，标签表里有旧数据
	for _, q := range []string{
		`ALTER TABLE tags DROP COLUMN kind`,
		`INSERT INTO tags (name) VALUES ('待办'), ('点子'), ('待研究'), ('稍后看'), ('资料'), ('发票'), ('Go')`,
		`DELETE FROM schema_migrations WHERE version = '0004_tag_kind.sql'`,
	} {
		if _, err := st.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := st.db.QueryContext(ctx, `SELECT name, kind FROM tags`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var n, k string
		rows.Scan(&n, &k)
		got[n] = k
	}
	want := map[string]string{"待办": "label", "点子": "label", "待研究": "label", "稍后看": "label", "资料": "label", "发票": "topic", "Go": "topic"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestLabelsAndTopicsStoredSeparately(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	id := newTagItem(t, st, "m1")
	if err := st.AddTags(ctx, id, TagKindLabel, []string{"待办", "票据"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTags(ctx, id, TagKindTopic, []string{"发票", "财务"}); err != nil {
		t.Fatal(err)
	}
	it, _ := st.GetItem(ctx, id)
	if !reflect.DeepEqual(it.Labels, []string{"待办", "票据"}) || !reflect.DeepEqual(it.Topics, []string{"发票", "财务"}) {
		t.Fatalf("labels=%v topics=%v", it.Labels, it.Topics)
	}
	// SetTags 只替换该 kind
	if err := st.SetTags(ctx, id, TagKindTopic, []string{"报销"}); err != nil {
		t.Fatal(err)
	}
	it, _ = st.GetItem(ctx, id)
	if !reflect.DeepEqual(it.Labels, []string{"待办", "票据"}) || !reflect.DeepEqual(it.Topics, []string{"报销"}) {
		t.Fatalf("after SetTags topic: labels=%v topics=%v", it.Labels, it.Topics)
	}
	if err := st.SetTags(ctx, id, TagKindLabel, nil); err != nil {
		t.Fatal(err)
	}
	it, _ = st.GetItem(ctx, id)
	if len(it.Labels) != 0 || !reflect.DeepEqual(it.Topics, []string{"报销"}) {
		t.Fatalf("after clearing labels: labels=%v topics=%v", it.Labels, it.Topics)
	}
	// AllTags 按 kind 过滤
	lc, _ := st.AllTags(ctx, TagKindLabel)
	tc, _ := st.AllTags(ctx, TagKindTopic)
	all, _ := st.AllTags(ctx, "")
	if len(lc) != 0 || len(tc) != 1 || tc[0].Name != "报销" || len(all) != 1 {
		t.Fatalf("lc=%v tc=%v all=%v", lc, tc, all)
	}
	if err := st.AddTags(ctx, id, "bogus", []string{"x"}); err == nil {
		t.Fatal("unknown kind must fail")
	}
}

func TestTopicSkippedWhenLabelWithSameNameExists(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	a, b := newTagItem(t, st, "a"), newTagItem(t, st, "b")
	if err := st.AddTags(ctx, a, TagKindLabel, []string{"Todo"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTags(ctx, b, TagKindTopic, []string{"todo", "其他"}); err != nil {
		t.Fatal(err)
	}
	it, _ := st.GetItem(ctx, b)
	if len(it.Labels) != 0 || !reflect.DeepEqual(it.Topics, []string{"其他"}) {
		t.Fatalf("labels=%v topics=%v", it.Labels, it.Topics)
	}
	// 反过来：已有同名 topic 时加 label，升级为 label，名称仍然唯一
	if err := st.AddTags(ctx, b, TagKindLabel, []string{"其他"}); err != nil {
		t.Fatal(err)
	}
	it, _ = st.GetItem(ctx, b)
	if !reflect.DeepEqual(it.Labels, []string{"其他"}) || len(it.Topics) != 0 {
		t.Fatalf("labels=%v topics=%v", it.Labels, it.Topics)
	}
}

func TestPromptSettingRoundTrip(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	s, _ := st.LoadSettings(ctx)
	if s.Prompt != "" || s.Rules.LabelRules != nil {
		t.Fatalf("defaults: %+v", s)
	}
	s.Prompt = "说明"
	s.Rules.LabelRules = []model.LabelRule{{Keyword: "测试", Label: "测试"}}
	if err := st.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, _ := st.LoadSettings(ctx)
	if got.Prompt != "说明" || !reflect.DeepEqual(got.Rules.LabelRules, s.Rules.LabelRules) {
		t.Fatalf("got %+v", got)
	}
}
