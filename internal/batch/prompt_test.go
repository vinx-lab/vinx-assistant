package batch

import (
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/llm"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestSystemPrompt(t *testing.T) {
	now := clock.At(2026, 10, 1, 9, 0)
	p := systemPrompt(model.LevelDeep, now, []string{"发票", "Go"})
	for _, want := range []string{"2026-10-01 09:00", "星期四", "Asia/Shanghai", "发票、Go", "fixed_category", "## 是什么", "## 同类对比", "## 上手步骤", `"detail"`} {
		if !strings.Contains(p, want) {
			t.Errorf("deep prompt missing %q", want)
		}
	}
	light := systemPrompt(model.LevelLight, now, nil)
	if strings.Contains(light, `"detail"`) || strings.Contains(light, "已有标签") {
		t.Errorf("light prompt has detail/tags: %s", light)
	}
	if !strings.Contains(systemPrompt(model.LevelMedium, now, nil), "值不值得") {
		t.Error("medium prompt must ask whether worth reading")
	}
}

func TestToPromptItem(t *testing.T) {
	it := &model.Item{ID: 7, CreatedAt: clock.At(2026, 10, 1, 9, 30), RawText: "一二三四五", URL: "https://x", Category: model.CatTodo, CategoryBy: model.ByPrefix}
	p := toPromptItem(it, 3)
	if p.ID != 7 || p.Received != "2026-10-01 09:30" || p.Text != "一二三…" || p.FixedCategory != "todo" || p.URL != "https://x" {
		t.Fatalf("p = %+v", p)
	}
	it.CategoryBy = model.ByAI
	if toPromptItem(it, 10).FixedCategory != "" {
		t.Fatal("AI-classified item must not be fixed")
	}
	if s := userPrompt([]promptItem{p}); !strings.Contains(s, `"fixed_category":"todo"`) {
		t.Fatalf("user prompt = %s", s)
	}
}

func TestEstimateAndBudget(t *testing.T) {
	req := llm.Request{Messages: []llm.Message{llm.System("ab"), llm.User("一二三")}, MaxTokens: 10}
	if got := Estimate(req); got != 15 {
		t.Fatalf("Estimate = %d, want 15", got)
	}
	req.Messages[1] = llm.UserWithImages("一二三", []string{"data:x"})
	if got := Estimate(req); got != 315 {
		t.Fatalf("Estimate with image = %d, want 315", got)
	}
	b := &Budget{Limit: 100, Used: 60}
	if !b.Fits(40) || b.Fits(41) {
		t.Fatal("Fits boundary wrong")
	}
	b.Spend(40)
	if b.Remaining() != 0 {
		t.Fatalf("remaining = %d", b.Remaining())
	}
	if !(&Budget{Limit: 0, Used: 1 << 40}).Fits(1 << 40) {
		t.Fatal("limit 0 must mean unlimited")
	}
}

func TestBuildRequest(t *testing.T) {
	now := clock.At(2026, 10, 1, 9, 0)
	req := buildRequest(model.LevelLight, now, nil, []promptItem{{ID: 1}, {ID: 2}}, []string{"data:image/jpeg;base64,AA"}, "m")
	if req.Model != "m" || len(req.Messages) != 2 || req.MaxTokens != maxTokens(model.LevelLight, 2) {
		t.Fatalf("req = %+v", req)
	}
	if _, ok := req.Messages[1].Content.([]llm.Part); !ok {
		t.Fatal("images must use parts")
	}
}

func TestPromptUsesShanghaiTime(t *testing.T) {
	utc := time.Date(2026, 10, 1, 17, 0, 0, 0, time.UTC)
	p := systemPrompt(model.LevelLight, utc, nil)
	if !strings.Contains(p, "2026-10-02 01:00 星期五") {
		t.Fatalf("prompt = %s", p)
	}
	it := &model.Item{ID: 1, CreatedAt: utc}
	if got := toPromptItem(it, 10).Received; got != "2026-10-02 01:00" {
		t.Fatalf("received = %s", got)
	}
}
