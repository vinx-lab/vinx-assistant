package remind

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

func item(id int64, text string, due *time.Time, hasTime bool) model.Item {
	return model.Item{ID: id, RawText: text, DueAt: due, DueHasTime: hasTime}
}

func tp(t time.Time) *time.Time { return &t }

func TestBuildDigest(t *testing.T) {
	now := clock.At(2026, 10, 1, 9, 0)
	d := store.DigestData{
		Overdue:         []model.Item{item(3, "报销发票", tp(clock.At(2026, 9, 28, 0, 0)), false)},
		DueToday:        []model.Item{item(12, "交材料", tp(clock.At(2026, 10, 1, 15, 0)), true)},
		ResearchBacklog: 5,
		TopResearch:     []model.Item{{ID: 20, Title: "htmx", Priority: model.PriorityHigh}},
		NewYesterday:    7,
	}
	want := strings.Join([]string{
		"📋 今日摘要 10-01 周四",
		"逾期（1）：",
		"#3 报销发票 · 09-28 周一",
		"今天到期（1）：",
		"#12 交材料 · 10-01 周四 15:00",
		"待研究积压 5 条，优先看：",
		"#20 htmx",
		"昨天新收 7 条。",
		"回复「完成 编号」标记完成，「推迟 编号 明天」顺延。",
	}, "\n")
	if got := BuildDigest(now, d); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildDigestEmpty(t *testing.T) {
	got := BuildDigest(clock.At(2026, 10, 1, 9, 0), store.DigestData{})
	if !strings.Contains(got, "今天没有到期的待办。") || !strings.Contains(got, "昨天新收 0 条。") || strings.Contains(got, "待研究") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestBuildDigestTruncates(t *testing.T) {
	var many []model.Item
	for i := 1; i <= 200; i++ {
		many = append(many, item(int64(i), fmt.Sprintf("一个比较长的待办标题第%d条", i), tp(clock.At(2026, 9, 1, 0, 0)), false))
	}
	got := BuildDigest(clock.At(2026, 10, 1, 9, 0), store.DigestData{Overdue: many})
	if n := utf8.RuneCountInString(got); n > MaxRunes {
		t.Fatalf("len = %d", n)
	}
	if !strings.Contains(got, "条，见网页") || !strings.HasSuffix(got, "顺延。") {
		t.Fatalf("tail:\n%s", got[len(got)-200:])
	}
}

func TestBuildDigestTomorrowDateOnly(t *testing.T) {
	now := clock.At(2026, 10, 1, 9, 0)
	d := store.DigestData{DueTomorrow: []model.Item{item(8, "订机票", tp(clock.At(2026, 10, 2, 0, 0)), false)}}
	got := BuildDigest(now, d)
	if !strings.Contains(got, "明天到期（1）：\n#8 订机票 · 10-02 周五\n") || strings.Contains(got, "今天没有到期") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestBuildNormalizesToZone(t *testing.T) {
	// UTC 的 07:00 即上海 15:00；now 以 UTC 传入也要按上海日期显示。
	now := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
	got := BuildDue([]model.Item{item(12, "交材料", &due, true)}, now, HeaderDue)
	if !strings.Contains(got, "#12 交材料 · 10-01 周四 15:00") {
		t.Fatalf("got %q", got)
	}
	late := time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC) // 上海 10-01 04:00
	if g := BuildDigest(late, store.DigestData{}); !strings.HasPrefix(g, "📋 今日摘要 10-01 周四") {
		t.Fatalf("got %q", g)
	}
}

func TestBuildDue(t *testing.T) {
	now := clock.At(2026, 10, 1, 15, 0)
	one := BuildDue([]model.Item{item(12, "交材料", tp(clock.At(2026, 10, 1, 15, 0)), true)}, now, HeaderDue)
	if one != "⏰ 到期提醒\n#12 交材料 · 10-01 周四 15:00\n回复「完成 12」标记完成。" {
		t.Fatalf("one = %q", one)
	}
	two := BuildDue([]model.Item{
		item(12, "交材料", tp(clock.At(2026, 10, 1, 15, 0)), true),
		item(13, "打电话", tp(clock.At(2026, 10, 1, 15, 5)), true),
	}, now, HeaderResend)
	if !strings.HasPrefix(two, "⏰ 补发提醒\n#12") || !strings.Contains(two, "#13 打电话") || !strings.HasSuffix(two, "回复「完成 编号」标记完成。") {
		t.Fatalf("two = %q", two)
	}
}
