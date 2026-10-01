package batch

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/llm/llmtest"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

func TestCustomPromptReplacesDefaultAndEmptyFallsBack(t *testing.T) {
	e := newTEnv(t, func(s *model.Settings) { s.Prompt = "我的自定义说明XYZ" })
	id := e.add(t, &model.Item{RawText: "买纸"})
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{{"id": id, "category": "todo", "title": "买纸"}}}))
	e.run(t)
	r := e.llm.Requests()[0]
	for _, want := range []string{"我的自定义说明XYZ", "只输出一个 JSON 对象", "2026-10-01 09:00", "fixed_category", `"items"`} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(r.Text, "你是个人收集箱的整理助手") {
		t.Error("custom prompt must replace the default text")
	}

	st, _ := e.st.LoadSettings(context.Background())
	if st.Prompt != "我的自定义说明XYZ" {
		t.Fatalf("prompt not persisted: %q", st.Prompt)
	}
	st.Prompt = "  "
	e.st.SaveSettings(context.Background(), st)
	id2 := e.add(t, &model.Item{RawText: "买笔"})
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{{"id": id2, "category": "todo", "title": "买笔"}}}))
	e.run(t)
	if r := e.llm.Requests()[1]; !strings.Contains(r.Text, DefaultPrompt) {
		t.Error("blank prompt must fall back to DefaultPrompt")
	}
}

func TestPreviewPromptMatchesRequest(t *testing.T) {
	st := model.DefaultSettings()
	now := clock.At(2026, 10, 1, 9, 0)
	got := PreviewPrompt(st, now, []string{"发票"}, []string{"待办"}, model.LevelLight)
	if got != systemPrompt("", model.LevelLight, now, []string{"发票"}, []string{"待办"}) {
		t.Error("preview differs from systemPrompt")
	}
	for _, want := range []string{DefaultPrompt, "已有内容标签", "发票", "类别标签", "待办"} {
		if !strings.Contains(got, want) {
			t.Errorf("preview missing %q", want)
		}
	}
}

func TestAITopicsSkipLabelNamesAndCapAtThree(t *testing.T) {
	e := newTEnv(t, func(s *model.Settings) { s.Rules.LabelRules = []model.LabelRule{{Keyword: "发票", Label: "票据"}} })
	id := e.add(t, &model.Item{RawText: "交发票"})
	if err := e.st.AddTags(context.Background(), id, store.TagKindLabel, []string{"待办"}); err != nil {
		t.Fatal(err)
	}
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{{"id": id, "category": "todo", "tags": []string{"待办", "票据", "财务", "报销", "月底", "多余"}, "title": "交发票"}}}))
	e.run(t)
	it := e.get(t, id)
	if !reflect.DeepEqual(it.Topics, []string{"报销", "月底", "财务"}) {
		t.Fatalf("topics = %v", it.Topics)
	}
	if !reflect.DeepEqual(it.Labels, []string{"待办"}) {
		t.Fatalf("labels = %v", it.Labels)
	}
	if r := e.llm.Requests()[0]; !strings.Contains(r.Text, "票据") {
		t.Error("label-rule labels should be listed in the prompt")
	}
}
