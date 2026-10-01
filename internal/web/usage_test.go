package web

import (
	"context"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

func TestUsagePage(t *testing.T) {
	e := newEnv(t)
	code, body := e.get(t, "/usage")
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	mustContain(t, body, "今日已用 0", "还没有 AI 调用记录")
}

func TestUsagePageWithData(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.item(t, &model.Item{MsgID: "1", RawText: "烧 token 的条目"})
	e.st.AddUsage(ctx, store.Usage{Day: "2026-10-01", Level: "light", Provider: "p", Model: "m", PromptTokens: 100, CompletionTokens: 50, ItemID: id})
	e.st.AddUsage(ctx, store.Usage{Day: "2026-09-30", Level: "light", Provider: "p", Model: "m", PromptTokens: 40, CompletionTokens: 10})
	_, body := e.get(t, "/usage")
	mustContain(t, body, "今日已用 150", "2026-09-30", "w100", "w33")
	mustNotContain(t, body, "还没有 AI 调用记录")
}
