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
	mustContain(t, body, `今日整理已用</span><span class="v">0<`, `今日指令</span><span class="v">0<`, "还没有 AI 调用记录")
}

func TestUsagePageWithData(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.item(t, &model.Item{MsgID: "1", RawText: "烧 token 的条目", TokensUsed: 150})
	e.st.AddUsage(ctx, store.Usage{Day: "2026-10-01", Level: "light", Provider: "p", Model: "m", PromptTokens: 100, CompletionTokens: 50, ItemID: id})
	e.st.AddUsage(ctx, store.Usage{Day: "2026-09-30", Level: "light", Provider: "p", Model: "m", PromptTokens: 40, CompletionTokens: 10})
	e.st.AddUsage(ctx, store.Usage{Day: "2026-10-01", Level: store.UsageLevelCommand, Provider: "p", Model: "m", PromptTokens: 20, CompletionTokens: 5})
	_, body := e.get(t, "/usage")
	// 今日与 30 天列表同一口径：整理与指令分开列，今日整理不含指令
	mustContain(t, body, `今日整理已用</span><span class="v">150<`, `今日指令</span><span class="v">25<`, "整理 150 + 指令 25 = 175", "整理 50 + 指令 0 = 50", "2026-09-30", "w100", "w28")
	mustNotContain(t, body, "还没有 AI 调用记录")
	// 耗 token 最多的条目列表不能和顶栏状态（Page.Top）重名，否则有数据时整页渲染失败
	mustContain(t, body, "烧 token 的条目", "微信")
}
