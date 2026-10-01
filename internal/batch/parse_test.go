package batch

import (
	"strings"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		"```json\n{\"items\":[]}\n```":       `{"items":[]}`,
		"好的，结果如下：\n{\"a\":{\"b\":1}}\n希望有帮助": `{"a":{"b":1}}`,
		`{"x":1}`: `{"x":1}`,
	}
	for in, want := range cases {
		got, err := extractJSON(in)
		if err != nil || got != want {
			t.Errorf("extractJSON(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := extractJSON("抱歉，我无法处理"); err == nil {
		t.Error("want error for no JSON")
	}
}

func TestParseDue(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		hasTime bool
		ok      bool
	}{
		{"", "", false, true},
		{"2026-10-31", "2026-10-31 00:00", false, true},
		{"2026-10-08 15:00", "2026-10-08 15:00", true, true},
		{"2026-10-08T15:00", "2026-10-08 15:00", true, true},
		{"2026-10-08 15:00:00", "2026-10-08 15:00", true, true},
		{"下周三", "", false, false},
		{"10-08", "", false, false},
	}
	for _, c := range cases {
		got, hasTime, err := ParseDue(c.in)
		if (err == nil) != c.ok {
			t.Errorf("ParseDue(%q) err = %v", c.in, err)
			continue
		}
		if c.want == "" {
			if got != nil {
				t.Errorf("ParseDue(%q) = %v, want nil", c.in, got)
			}
			continue
		}
		if got == nil || got.Format(timeLayout) != c.want || hasTime != c.hasTime || got.Location() != clock.Zone {
			t.Errorf("ParseDue(%q) = %v %v", c.in, got, hasTime)
		}
	}
}

func items(ids ...int64) []*model.Item {
	var out []*model.Item
	for _, id := range ids {
		out = append(out, &model.Item{ID: id, CreatedAt: clock.At(2026, 10, 1, 9, 0)})
	}
	return out
}

func TestParseItems(t *testing.T) {
	content := "```json\n" + `{"items":[
		{"id":1,"category":"待办","tags":["#发票"],"title":"交发票","summary":"s","due":"2026-10-31","priority":"高"},
		{"id":2,"category":"inbox","title":"x"},
		{"id":99,"category":"todo","title":"不属于本批"},
		{"id":"4","category":"idea","title":"字符串 id 也接受","priority":""}
	]}` + "\n```"
	res, errs, err := parseItems(content, items(1, 2, 3, 4), false)
	if err != nil {
		t.Fatal(err)
	}
	r1 := res[1]
	if r1.Category != model.CatTodo || r1.Priority != model.PriorityHigh || r1.Tags[0] != "发票" || r1.Due == nil || r1.DueHasTime {
		t.Fatalf("r1 = %+v", r1)
	}
	if _, ok := res[4]; !ok {
		t.Fatal("string id rejected")
	}
	if errs[2] == nil || !strings.Contains(errs[2].Error(), "分类") {
		t.Fatalf("errs[2] = %v", errs[2])
	}
	if errs[3] == nil || !strings.Contains(errs[3].Error(), "没有返回") {
		t.Fatalf("errs[3] = %v", errs[3])
	}
	if _, ok := res[99]; ok {
		t.Fatal("foreign id accepted")
	}
}

func TestParseItemsSingleObjectAndDetail(t *testing.T) {
	res, _, err := parseItems(`{"id":5,"category":"research","title":"t","summary":"s","detail":"## 是什么\n..."}`, items(5), true)
	if err != nil || res[5].Detail == "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	_, errs, err := parseItems(`{"items":[{"id":5,"category":"research","title":"t"}]}`, items(5), true)
	if err == nil || errs[5] == nil || !strings.Contains(errs[5].Error(), "detail") {
		t.Fatalf("missing detail must fail: err=%v errs=%v", err, errs)
	}
}

func TestParseItemsDue(t *testing.T) {
	content := `{"items":[
		{"id":1,"category":"todo","due":"2025-10-31"},
		{"id":2,"category":"todo","due":"月底"},
		{"id":3,"category":"todo","due":"2026-10-01 18:00"}
	]}`
	res, errs, err := parseItems(content, items(1, 2, 3), false)
	if err != nil || len(errs) != 0 {
		t.Fatalf("err=%v errs=%v", err, errs)
	}
	if res[1].Due != nil || len(res[1].Warnings) == 0 {
		t.Fatalf("past due kept: %+v", res[1])
	}
	if res[2].Due != nil || len(res[2].Warnings) == 0 {
		t.Fatalf("bad due kept: %+v", res[2])
	}
	if res[3].Due == nil || !res[3].DueHasTime {
		t.Fatalf("same-day due dropped: %+v", res[3])
	}
}

func TestParseItemsNothingUsable(t *testing.T) {
	if _, _, err := parseItems(`{"items":[{"id":99,"category":"todo"}]}`, items(1), false); err == nil {
		t.Fatal("want error when no usable item")
	}
	if _, _, err := parseItems(`{"items":`, items(1), false); err == nil {
		t.Fatal("want error for truncated JSON")
	}
}

func TestApplyKeepsFixedCategory(t *testing.T) {
	userDue := clock.At(2026, 10, 20, 15, 0)
	aiDue := clock.At(2026, 11, 1, 0, 0)
	r := Result{Category: model.CatResearch, Title: "新标题", Summary: "新摘要", Priority: model.PriorityLow, Due: &aiDue}

	manual := &model.Item{Category: model.CatIdea, CategoryBy: model.ByManual, Status: model.StatusKept, Priority: model.PriorityHigh, DueAt: &userDue, DueHasTime: true, ProcessError: "旧错误"}
	apply(manual, r, model.LevelLight)
	if manual.Category != model.CatIdea || manual.Status != model.StatusKept {
		t.Fatalf("manual category changed: %+v", manual)
	}
	if manual.Priority != model.PriorityHigh || !manual.DueAt.Equal(userDue) || !manual.DueHasTime {
		t.Fatalf("user priority/due overwritten: %+v", manual)
	}
	if manual.Title != "新标题" || manual.ProcessedLevel != model.LevelLight || manual.ProcessError != "" {
		t.Fatalf("fields not applied: %+v", manual)
	}

	ai := &model.Item{Category: model.CatInbox, CategoryBy: model.ByAI, Status: model.StatusNew, ProcessedLevel: model.LevelDeep}
	apply(ai, r, model.LevelLight)
	if ai.Category != model.CatResearch || ai.Status != model.StatusNew || ai.Priority != model.PriorityLow || ai.DueAt == nil {
		t.Fatalf("ai item = %+v", ai)
	}
	if ai.ProcessedLevel != model.LevelDeep {
		t.Fatal("processed level must not go down")
	}
	todo := Result{Category: model.CatTodo}
	apply(ai, todo, model.LevelLight)
	if ai.Status != model.StatusOpen {
		t.Fatalf("status not reset on category change: %s", ai.Status)
	}
}

func TestApplyMergesTagsAndResetsAttempts(t *testing.T) {
	it := &model.Item{CategoryBy: model.ByManual, Category: model.CatIdea, Status: model.StatusKept, Tags: []string{"点子", "我的"}, ProcessError: "e", ProcessAttempts: 2, Title: "旧", RawText: "原文"}
	apply(it, Result{Category: model.CatTodo, Tags: []string{"ai", "我的"}}, model.LevelLight)
	if strings.Join(it.Tags, ",") != "点子,我的,ai" {
		t.Fatalf("tags = %v", it.Tags)
	}
	if it.ProcessAttempts != 0 || it.ProcessError != "" || it.Title != "旧" || it.RawText != "原文" || it.Category != model.CatIdea {
		t.Fatalf("it = %+v", it)
	}
}
