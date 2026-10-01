package ingest

import (
	"reflect"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestClassify(t *testing.T) {
	rules := model.DefaultSettings().Rules
	cases := []struct {
		name   string
		in     Input
		images bool
		cat    model.Category
		by     model.CategoryBy
		level  model.Level
		text   string
	}{
		{"中文冒号前缀", Input{Text: "待办：交发票"}, false, model.CatTodo, model.ByPrefix, model.LevelLight, "交发票"},
		{"英文冒号前缀", Input{Text: "点子: 做个收集箱"}, false, model.CatIdea, model.ByPrefix, model.LevelLight, "做个收集箱"},
		{"语音转写的逗号", Input{Text: "待办，明天交发票"}, false, model.CatTodo, model.ByPrefix, model.LevelLight, "明天交发票"},
		{"长前缀优先", Input{Text: "待研究：htmx"}, false, model.CatResearch, model.ByPrefix, model.LevelLight, "htmx"},
		{"短前缀", Input{Text: "研究：htmx"}, false, model.CatResearch, model.ByPrefix, model.LevelLight, "htmx"},
		{"没有分隔符也算", Input{Text: "待办事项很多"}, false, model.CatTodo, model.ByPrefix, model.LevelLight, "待办事项很多"},
		{"待办冒烟测试", Input{Text: "待办冒烟测试"}, false, model.CatTodo, model.ByPrefix, model.LevelLight, "待办冒烟测试"},
		{"代办", Input{Text: "代办，交发票"}, false, model.CatTodo, model.ByPrefix, model.LevelLight, "交发票"},
		{"关键词在中间", Input{Text: "这个待办清单App不错"}, false, model.CatTodo, model.ByPrefix, model.LevelLight, "这个待办清单App不错"},
		{"想法在中间", Input{Text: "我有个想法：做个收集箱"}, false, model.CatIdea, model.ByPrefix, model.LevelLight, "我有个想法：做个收集箱"},
		{"最早出现者优先", Input{Text: "研究了下资料"}, false, model.CatResearch, model.ByPrefix, model.LevelLight, "研究了下资料"},
		{"同位置取长", Input{Text: "待研究htmx"}, false, model.CatResearch, model.ByPrefix, model.LevelLight, "待研究htmx"},
		{"只有关键词保留原文", Input{Text: "待办"}, false, model.CatTodo, model.ByPrefix, model.LevelLight, "待办"},
		{"深入研究里的研究", Input{Text: "深入研究一下这个框架"}, false, model.CatResearch, model.ByPrefix, model.LevelDeep, "深入研究一下这个框架"},
		{"中等关键词", Input{Text: "https://github.com/x/y 研究一下"}, false, model.CatResearch, model.ByPrefix, model.LevelMedium, "https://github.com/x/y 研究一下"},
		{"无关键词", Input{Text: "https://github.com/x/y 不错"}, false, model.CatInbox, model.ByAI, model.LevelLight, "https://github.com/x/y 不错"},
		{"深度优先", Input{Text: "稍后看：深入研究一下这个"}, false, model.CatLater, model.ByPrefix, model.LevelDeep, "深入研究一下这个"},
		{"无前缀图片且开关关", Input{HasImage: true}, false, model.CatArchive, model.ByPrefix, model.LevelLight, ""},
		{"无前缀图片且开关开", Input{HasImage: true}, true, model.CatInbox, model.ByAI, model.LevelLight, ""},
		{"图片带文字", Input{Text: "这个界面不错", HasImage: true}, false, model.CatInbox, model.ByAI, model.LevelLight, "这个界面不错"},
		{"前后空白", Input{Text: "  资料：  合同  "}, false, model.CatArchive, model.ByPrefix, model.LevelLight, "合同"},
	}
	for _, c := range cases {
		got := Classify(c.in, rules, c.images)
		want := Parsed{Category: c.cat, CategoryBy: c.by, Level: c.level, Text: c.text, Labels: got.Labels}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, want)
		}
	}
}

func TestClassifyLabels(t *testing.T) {
	rules := model.DefaultSettings().Rules
	cases := []struct {
		name   string
		text   string
		cat    model.Category
		labels []string
	}{
		{"多个关键词多个标签", "这个想法也算待办", model.CatIdea, []string{"点子", "待办"}},
		{"代办同待办", "代办，交发票", model.CatTodo, []string{"待办"}},
		{"去重", "待研究：研究一下 htmx", model.CatResearch, []string{"待研究"}},
		{"无关键词", "今天天气不错", model.CatInbox, nil},
	}
	for _, c := range cases {
		got := Classify(Input{Text: c.text}, rules, false)
		if got.Category != c.cat || !reflect.DeepEqual(got.Labels, c.labels) {
			t.Errorf("%s: cat=%s labels=%q, want %s %q", c.name, got.Category, got.Labels, c.cat, c.labels)
		}
	}
	if got := Classify(Input{HasImage: true}, rules, false); len(got.Labels) != 0 {
		t.Errorf("无文字图片不应有标签: %q", got.Labels)
	}
}

func TestIsCommand(t *testing.T) {
	words := model.DefaultSettings().Rules.ActionWords
	cases := []struct {
		text   string
		hasRef bool
		want   bool
	}{
		{"完成 12", false, true},
		{"完成#12", false, true},
		{"搞定发票那个", false, true}, // 交给 AI 翻译
		{"列表", false, true},
		{"撤销", false, true},
		{"  推迟 15 明天", false, true},
		{"今天完成了 3 件事", false, false},
		{"好的", true, true},
		{"", true, false},
		{"待办：交发票", false, false},
	}
	for _, c := range cases {
		if got := IsCommand(c.text, c.hasRef, words); got != c.want {
			t.Errorf("IsCommand(%q, %v) = %v, want %v", c.text, c.hasRef, got, c.want)
		}
	}
}

func TestClassifyLabelRules(t *testing.T) {
	rules := model.DefaultSettings().Rules
	rules.LabelRules = []model.LabelRule{{Keyword: "测试", Label: "测试"}, {Keyword: "发票", Label: "票据"}, {Keyword: "OpenAI", Label: "AI"}, {Keyword: "", Label: "空"}}
	got := Classify(Input{Text: "这个待办要测试 openai 的发票"}, rules, false)
	if got.Category != model.CatTodo {
		t.Errorf("label rules must not change the category rules: %s", got.Category)
	}
	if want := []string{"待办", "测试", "票据", "AI"}; !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("labels = %q, want %q", got.Labels, want)
	}
	if got := Classify(Input{Text: "没有命中"}, rules, false); got.Category != model.CatInbox || got.Labels != nil {
		t.Errorf("no hit: %+v", got)
	}
}

func TestClassifyIgnoresInvalidCategoryPrefix(t *testing.T) {
	rules := model.Rules{Prefixes: []model.PrefixRule{{Prefix: "乱", Category: "bogus"}, {Prefix: "待办", Category: model.CatTodo}}}
	p := Classify(Input{Text: "乱：x"}, rules, false)
	if p.Category != model.CatInbox || p.CategoryBy != model.ByAI || p.Text != "乱：x" {
		t.Fatalf("invalid-category prefix applied: %+v", p)
	}
	if p := Classify(Input{Text: "待办：y"}, rules, false); p.Category != model.CatTodo {
		t.Fatalf("valid prefix lost: %+v", p)
	}
}
