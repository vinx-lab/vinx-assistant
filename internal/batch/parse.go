package batch

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

var categoryAlias = map[string]model.Category{
	"research": model.CatResearch, "待研究": model.CatResearch,
	"later": model.CatLater, "稍后看": model.CatLater,
	"todo": model.CatTodo, "待办": model.CatTodo,
	"idea": model.CatIdea, "点子": model.CatIdea,
	"archive": model.CatArchive, "资料": model.CatArchive,
}

var priorityAlias = map[string]model.Priority{
	"": model.PriorityNone, "none": model.PriorityNone,
	"high": model.PriorityHigh, "高": model.PriorityHigh,
	"medium": model.PriorityMedium, "中": model.PriorityMedium,
	"low": model.PriorityLow, "低": model.PriorityLow,
}

type Result struct {
	ID         int64
	Category   model.Category
	Tags       []string
	Title      string
	Summary    string
	Detail     string
	Due        *time.Time
	DueHasTime bool
	Priority   model.Priority
	Warnings   []string
}

type rawResult struct {
	ID       json.Number `json:"id"`
	Category string      `json:"category"`
	Tags     []string    `json:"tags"`
	Title    string      `json:"title"`
	Summary  string      `json:"summary"`
	Detail   string      `json:"detail"`
	Due      string      `json:"due"`
	Priority string      `json:"priority"`
}

// extractJSON 取第一个「{」到最后一个「}」之间的内容，顺带去掉代码块和前后的说明文字。
func extractJSON(s string) (string, error) {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return "", errors.New("AI 返回的不是 JSON")
	}
	return s[i : j+1], nil
}

var dueLayouts = []struct {
	layout  string
	hasTime bool
}{
	{"2006-01-02 15:04", true},
	{"2006-01-02T15:04", true},
	{"2006-01-02 15:04:05", true},
	{"2006-01-02T15:04:05", true},
	{"2006-01-02", false},
}

// ParseDue 解析 AI 给的截止时间（上海时间）。空串返回 nil。
func ParseDue(s string) (*time.Time, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false, nil
	}
	for _, l := range dueLayouts {
		if t, err := time.ParseInLocation(l.layout, s, clock.Zone); err == nil {
			return &t, l.hasTime, nil
		}
	}
	return nil, false, fmt.Errorf("截止时间格式不对：%q", s)
}

func startOfDay(t time.Time) time.Time {
	t = t.In(clock.Zone)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, clock.Zone)
}

func toResult(raw rawResult, received time.Time, needDetail bool) (Result, error) {
	id, _ := raw.ID.Int64()
	cat, ok := categoryAlias[strings.ToLower(strings.TrimSpace(raw.Category))]
	if !ok {
		return Result{}, fmt.Errorf("AI 给的分类不合法：%q", raw.Category)
	}
	prio, ok := priorityAlias[strings.ToLower(strings.TrimSpace(raw.Priority))]
	if !ok {
		return Result{}, fmt.Errorf("AI 给的优先级不合法：%q", raw.Priority)
	}
	res := Result{
		ID:       id,
		Category: cat,
		Priority: prio,
		Tags:     model.NormalizeTags(raw.Tags),
		Title:    model.TruncateRunes(strings.TrimSpace(raw.Title), 60),
		Summary:  model.TruncateRunes(strings.TrimSpace(raw.Summary), 300),
		Detail:   strings.TrimSpace(raw.Detail),
	}
	if needDetail && res.Detail == "" {
		return Result{}, errors.New("AI 没有给出 detail")
	}
	due, hasTime, err := ParseDue(raw.Due)
	switch {
	case err != nil:
		res.Warnings = append(res.Warnings, err.Error()+"，已忽略")
	case due != nil && due.Before(startOfDay(received)):
		res.Warnings = append(res.Warnings, "截止时间早于收到的日期，已忽略："+raw.Due)
	default:
		res.Due, res.DueHasTime = due, hasTime
	}
	return res, nil
}

// parseItems 解析并校验 AI 的回答。不属于本批的 id 忽略；漏掉或不合法的条目放进 errs。
// 整个回答不是 JSON、或一条可用的都没有时返回 err，调用方据此重试。
func parseItems(content string, items []*model.Item, needDetail bool) (map[int64]Result, map[int64]error, error) {
	js, err := extractJSON(content)
	if err != nil {
		return nil, nil, err
	}
	var out struct {
		Items []rawResult `json:"items"`
	}
	if err := json.Unmarshal([]byte(js), &out); err != nil {
		return nil, nil, fmt.Errorf("AI 返回的 JSON 无法解析：%w", err)
	}
	if len(out.Items) == 0 {
		var single rawResult
		if json.Unmarshal([]byte(js), &single) == nil && single.ID != "" {
			out.Items = []rawResult{single}
		}
	}
	byID := make(map[int64]*model.Item, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}
	results := map[int64]Result{}
	errs := map[int64]error{}
	for _, raw := range out.Items {
		id, err := raw.ID.Int64()
		it := byID[id]
		if err != nil || it == nil {
			continue
		}
		if _, done := results[id]; done {
			continue
		}
		res, err := toResult(raw, it.CreatedAt, needDetail)
		if err != nil {
			errs[id] = err
			continue
		}
		delete(errs, id)
		results[id] = res
	}
	for _, it := range items {
		if _, ok := results[it.ID]; ok {
			continue
		}
		if _, ok := errs[it.ID]; !ok {
			errs[it.ID] = errors.New("AI 没有返回这一条")
		}
	}
	if len(results) == 0 {
		return results, errs, errors.New("AI 返回里没有可用的条目")
	}
	return results, errs, nil
}

// apply 把 AI 的结果写回条目（纯函数，只改 AI 负责的字段，调用方在 store.ModifyItem 回调里调用）。
func apply(it *model.Item, r Result, level model.Level) {
	if it.CategoryBy == model.ByAI && r.Category != it.Category {
		it.Category = r.Category
		it.Status = model.DefaultStatus(r.Category)
	}
	if r.Title != "" {
		it.Title = r.Title
	}
	if r.Summary != "" {
		it.Summary = r.Summary
	}
	if r.Detail != "" {
		it.Detail = r.Detail
	}
	if it.Priority == model.PriorityNone {
		it.Priority = r.Priority
	}
	if it.DueAt == nil && r.Due != nil {
		it.DueAt, it.DueHasTime = r.Due, r.DueHasTime
	}
	// 标签只增不减：保留用户和关键词打的标签，AI 的标签追加在后面。
	if len(r.Tags) > 0 {
		it.Tags = model.NormalizeTags(append(append([]string(nil), it.Tags...), r.Tags...))
	}
	if level.Rank() > it.ProcessedLevel.Rank() {
		it.ProcessedLevel = level
	}
	it.ProcessError = ""
	it.ProcessAttempts = 0
}
