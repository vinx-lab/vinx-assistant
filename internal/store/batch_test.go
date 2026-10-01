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
	// 手动设回收件箱、且已处理到要求深度的：不反复整理。
	manual := &model.Item{MsgID: "h", Category: model.CatInbox, CategoryBy: model.ByManual, Level: model.LevelLight, ProcessedLevel: model.LevelLight}
	if _, err := st.InsertItem(ctx, manual); err != nil {
		t.Fatal(err)
	}
	// 手动设回收件箱、但还没处理过的：照常按深度整理。
	m2 := &model.Item{MsgID: "i", Category: model.CatInbox, CategoryBy: model.ByManual, Level: model.LevelLight}
	id2, err := st.InsertItem(ctx, m2)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, id2)
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
	if ok, err := st.TryLease(ctx, "l", "a", now, 10*time.Minute); !ok || err != nil {
		t.Fatalf("first lease ok=%v err=%v", ok, err)
	}
	if ok, _ := st.TryLease(ctx, "l", "b", now.Add(time.Minute), 10*time.Minute); ok {
		t.Fatal("second lease must fail while held")
	}
	// 续期延长到期时间：原本 11 分钟时已过期，续期后 11 分钟时别人仍抢不到。
	if ok, err := st.RenewLease(ctx, "l", "a", now.Add(5*time.Minute), 10*time.Minute); !ok || err != nil {
		t.Fatalf("renew ok=%v err=%v", ok, err)
	}
	if ok, _ := st.TryLease(ctx, "l", "b", now.Add(11*time.Minute), 10*time.Minute); ok {
		t.Fatal("renewed lease taken before new expiry")
	}
	// 别的持有者不能续期、不能释放。
	if ok, _ := st.RenewLease(ctx, "l", "b", now.Add(6*time.Minute), 10*time.Minute); ok {
		t.Fatal("other owner renewed")
	}
	if err := st.ReleaseLease(ctx, "l", "b"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.TryLease(ctx, "l", "b", now.Add(12*time.Minute), 10*time.Minute); ok {
		t.Fatal("other owner released the lease")
	}
	// 过期后可被接管，原持有者随后不能续期也不能释放新租约。
	if ok, _ := st.TryLease(ctx, "l", "b", now.Add(16*time.Minute), 10*time.Minute); !ok {
		t.Fatal("expired lease must be re-acquirable")
	}
	if ok, _ := st.RenewLease(ctx, "l", "a", now.Add(17*time.Minute), 10*time.Minute); ok {
		t.Fatal("old owner renewed after takeover")
	}
	st.ReleaseLease(ctx, "l", "a")
	if ok, _ := st.TryLease(ctx, "l", "c", now.Add(18*time.Minute), 10*time.Minute); ok {
		t.Fatal("old owner released new owner's lease")
	}
	st.ReleaseLease(ctx, "l", "b")
	if ok, _ := st.TryLease(ctx, "l", "c", now.Add(18*time.Minute), 10*time.Minute); !ok {
		t.Fatal("released lease must be acquirable")
	}
}
