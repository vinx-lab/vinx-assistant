// Package remind 负责每日摘要、到期提醒和发送失败后的补发。
package remind

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

const (
	MaxRunes     = 1500
	HeaderDue    = "⏰ 到期提醒"
	HeaderResend = "⏰ 补发提醒"
	digestFooter = "回复「完成 编号」标记完成，「推迟 编号 明天」顺延。"
)

func itemLine(it model.Item, now time.Time) string {
	line := fmt.Sprintf("#%d %s", it.ID, model.TruncateRunes(it.DisplayTitle(), 30))
	if it.DueAt != nil {
		line += " · " + model.FormatDue(*it.DueAt, it.DueHasTime, now)
	}
	return line
}

// BuildDigest 生成每日摘要。逾期的每天都列；只有日期的截止出现在当天和前一天（明天到期段）的摘要里。
func BuildDigest(now time.Time, d store.DigestData) string {
	return fit(digestLines(now.In(clock.Zone), d), digestFooter)
}

// digestLines 摘要正文各行（不含页脚）；now 须已是 clock.Zone。
func digestLines(now time.Time, d store.DigestData) []string {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	lines := []string{"📋 今日摘要 " + model.FormatDue(today, false, now)}
	sections := []struct {
		title string
		items []model.Item
	}{{"逾期", d.Overdue}, {"今天到期", d.DueToday}, {"明天到期", d.DueTomorrow}}
	empty := true
	for _, s := range sections {
		if len(s.items) == 0 {
			continue
		}
		empty = false
		lines = append(lines, fmt.Sprintf("%s（%d）：", s.title, len(s.items)))
		for _, it := range s.items {
			lines = append(lines, itemLine(it, now))
		}
	}
	if empty {
		lines = append(lines, "今天没有到期的待办。")
	}
	if d.ResearchBacklog > 0 {
		head := fmt.Sprintf("待研究积压 %d 条", d.ResearchBacklog)
		if len(d.TopResearch) > 0 {
			head += "，优先看："
		}
		lines = append(lines, head)
		for _, it := range d.TopResearch {
			lines = append(lines, fmt.Sprintf("#%d %s", it.ID, model.TruncateRunes(it.DisplayTitle(), 30)))
		}
	}
	lines = append(lines, fmt.Sprintf("昨天新收 %d 条。", d.NewYesterday))
	return lines
}

// BuildDue 生成到期提醒（或补发）文本，每条都带 #编号。
func BuildDue(items []model.Item, now time.Time, header string) string {
	return buildMessage(now, nil, items, header)
}

// buildMessage 把摘要（可为 nil）和到期条目合成一条消息：摘要在前，到期段在后，
// 整体按 MaxRunes 截断，页脚只保留一个。
func buildMessage(now time.Time, d *store.DigestData, due []model.Item, header string) string {
	now = now.In(clock.Zone)
	var lines []string
	if d != nil {
		lines = digestLines(now, *d)
	}
	if len(due) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, header)
		for _, it := range due {
			lines = append(lines, itemLine(it, now))
		}
	}
	footer := "回复「完成 编号」标记完成。"
	switch {
	case d != nil:
		footer = digestFooter
	case len(due) == 1:
		footer = fmt.Sprintf("回复「完成 %d」标记完成。", due[0].ID)
	}
	return fit(lines, footer)
}

// fit 超过 MaxRunes 时从后面截掉条目行，补一行「…还有 N 条，见网页」，页脚始终保留。
func fit(lines []string, footer string) string {
	full := strings.Join(append(append([]string(nil), lines...), footer), "\n")
	if utf8.RuneCountInString(full) <= MaxRunes {
		return full
	}
	budget := MaxRunes - utf8.RuneCountInString(footer) - 20
	var out []string
	used := 0
	for i, l := range lines {
		n := utf8.RuneCountInString(l) + 1
		if used+n > budget {
			omitted := 0
			for _, rest := range lines[i:] {
				if strings.HasPrefix(rest, "#") {
					omitted++
				}
			}
			out = append(out, fmt.Sprintf("…还有 %d 条，见网页", omitted))
			break
		}
		out = append(out, l)
		used += n
	}
	return strings.Join(append(out, footer), "\n")
}
