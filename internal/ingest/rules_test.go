package ingest

import (
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
		{"没有分隔符不算前缀", Input{Text: "待办事项很多"}, false, model.CatInbox, model.ByAI, model.LevelLight, "待办事项很多"},
		{"中等关键词", Input{Text: "https://github.com/x/y 研究一下"}, false, model.CatInbox, model.ByAI, model.LevelMedium, "https://github.com/x/y 研究一下"},
		{"深度优先", Input{Text: "稍后看：深入研究一下这个"}, false, model.CatLater, model.ByPrefix, model.LevelDeep, "深入研究一下这个"},
		{"无前缀图片且开关关", Input{HasImage: true}, false, model.CatArchive, model.ByPrefix, model.LevelLight, ""},
		{"无前缀图片且开关开", Input{HasImage: true}, true, model.CatInbox, model.ByAI, model.LevelLight, ""},
		{"图片带文字", Input{Text: "这个界面不错", HasImage: true}, false, model.CatInbox, model.ByAI, model.LevelLight, "这个界面不错"},
		{"前后空白", Input{Text: "  资料：  合同  "}, false, model.CatArchive, model.ByPrefix, model.LevelLight, "合同"},
	}
	for _, c := range cases {
		got := Classify(c.in, rules, c.images)
		want := Parsed{Category: c.cat, CategoryBy: c.by, Level: c.level, Text: c.text}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, want)
		}
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
