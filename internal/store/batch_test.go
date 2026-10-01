package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestUsage(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	id, _ := st.InsertItem(ctx, &model.Item{MsgID: "u1"})
	st.AddUsage(ctx, Usage{Day: "2026-10-01", Level: "light", Model: "m", PromptTokens: 100, CompletionTokens: 50})
	st.AddUsage(ctx, Usage{Day: "2026-10-01", Level: "deep", Model: "m", PromptTokens: 10, CompletionTokens: 5, ItemID: id})
	st.AddUsage(ctx, Usage{Day: "2026-09-30", Level: "light", Model: "m", PromptTokens: 1, CompletionTokens: 1})
	if n, _ := st.TokensOn(ctx, "2026-10-01"); n != 165 {
		t.Fatalf("TokensOn = %d", n)
	}
	if n, _ := st.TokensOn(ctx, "2026-10-02"); n != 0 {
		t.Fatalf("empty day = %d", n)
	}
	days, _ := st.UsageByDay(ctx, 10)
	if len(days) != 2 || days[0] != (DayUsage{Day: "2026-10-01", PromptTokens: 110, CompletionTokens: 55, Calls: 2}) {
		t.Fatalf("days = %+v", days)
	}

	it, _ := st.GetItem(ctx, id)
	it.TokensUsed, it.ProcessError = 15, "AI 没有返回这一条"
	st.UpdateItem(ctx, it)
	if top, _ := st.TopItemsByTokens(ctx, 5); len(top) != 1 || top[0].ID != id {
		t.Fatalf("top = %+v", top)
	}
	if failed, _ := st.FailedItems(ctx, 5); len(failed) != 1 || failed[0].ProcessError == "" {
		t.Fatalf("failed = %+v", failed)
	}
}

func TestPendingForBatch(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	add := func(msg string, cat model.Category, lvl, done model.Level, status string, attempts int) int64 {
		it := &model.Item{MsgID: msg, Category: cat, Level: lvl, ProcessedLevel: done, Status: status, ProcessAttempts: attempts}
		id, err := st.InsertItem(ctx, it)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	want := []int64{
		add("a", model.CatInbox, model.LevelLight, "", "", 0),
		add("b", model.CatTodo, model.LevelLight, "", "", 0),
		add("d", model.CatResearch, model.LevelDeep, model.LevelLight, "", 2),
	}
	add("c", model.CatLater, model.LevelLight, model.LevelLight, "", 0)     // 已处理
	add("e", model.CatInbox, model.LevelLight, "", "", 3)                   // 次数用完
	add("f", model.CatTodo, model.LevelLight, "", model.StatusCancelled, 0) // 已取消
	add("g", model.CatResearch, model.LevelMedium, model.LevelDeep, "", 0)  // 已处理得更深
	got, err := st.PendingForBatch(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, it := range got {
		ids = append(ids, it.ID)
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("pending = %v, want %v", ids, want)
	}
}

func TestLease(t *testing.T) {
	st, fc := openTest(t)
	ctx := context.Background()
	now := fc.Now()
	if ok, err := st.TryLease(ctx, "l", now, 30*time.Minute); !ok || err != nil {
		t.Fatalf("first lease ok=%v err=%v", ok, err)
	}
	if ok, _ := st.TryLease(ctx, "l", now.Add(time.Minute), 30*time.Minute); ok {
		t.Fatal("second lease must fail while held")
	}
	if ok, _ := st.TryLease(ctx, "l", now.Add(31*time.Minute), 30*time.Minute); !ok {
		t.Fatal("expired lease must be re-acquirable")
	}
	st.ReleaseLease(ctx, "l")
	if ok, _ := st.TryLease(ctx, "l", now.Add(32*time.Minute), 30*time.Minute); !ok {
		t.Fatal("released lease must be acquirable")
	}
}
