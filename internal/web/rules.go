package web

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

// 设置页的文本框和结构化设置之间的转换。都是纯函数：解析失败返回带行号的中文错误，调用方不保存。

func splitList(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == '，' || r == '、' || r == ' ' || r == '\t'
	})
}

func lines(text string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

var clockRe = regexp.MustCompile(`^(\d{1,2})[:：](\d{2})$`)

// ParseClock 把「8:00」「08：00」规范成「08:00」。
func ParseClock(s string) (string, error) {
	m := clockRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("时间「%s」格式不对，应为 HH:MM", s)
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	if h > 23 || mi > 59 {
		return "", fmt.Errorf("时间「%s」超出范围", s)
	}
	return fmt.Sprintf("%02d:%02d", h, mi), nil
}

// ParseTimes 解析整理时间列表（换行、逗号或空格分隔），去重排序。允许为空：只手动整理。
func ParseTimes(text string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, f := range splitList(text) {
		c, err := ParseClock(f)
		if err != nil {
			return nil, err
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ParseWords 解析关键词列表，去空去重，保持顺序。
func ParseWords(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range splitList(text) {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

func splitPair(line string) (string, string, bool) {
	k, v, ok := strings.Cut(line, "=")
	if !ok {
		k, v, ok = strings.Cut(line, "＝")
	}
	return strings.TrimSpace(k), strings.TrimSpace(v), ok
}

func categoryFrom(s string) (model.Category, bool) {
	for _, c := range model.Categories {
		if string(c) == s || model.CategoryName(c) == s {
			return c, c != model.CatInbox
		}
	}
	return "", false
}

// ParsePrefixes 解析「关键词=分类」，分类可写代码（todo）或中文名（待办）。关键词末尾的冒号会去掉。
func ParsePrefixes(text string) ([]model.PrefixRule, error) {
	seen := map[string]bool{}
	var out []model.PrefixRule
	for i, l := range lines(text) {
		k, v, ok := splitPair(l)
		k = strings.TrimRight(k, ":：")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("分类关键词第 %d 行「%s」应写成「关键词=分类」", i+1, l)
		}
		if strings.ContainsAny(k, " \t") {
			return nil, fmt.Errorf("分类关键词第 %d 行：关键词不能含空格", i+1)
		}
		c, ok := categoryFrom(v)
		if !ok {
			return nil, fmt.Errorf("分类关键词第 %d 行：分类「%s」不认识，可用：待研究、稍后看、待办、点子、资料", i+1, v)
		}
		if seen[k] {
			return nil, fmt.Errorf("分类关键词第 %d 行：关键词「%s」重复", i+1, k)
		}
		seen[k] = true
		out = append(out, model.PrefixRule{Prefix: k, Category: c})
	}
	return out, nil
}

// ActionOps 是操作词能映射到的指令（计划 3 的 command 包认这些）。
var ActionOps = []string{"done", "postpone", "reschedule", "cancel", "list", "undo"}

// ParseActionWords 解析「操作词=指令」。
func ParseActionWords(text string) ([]model.ActionWord, error) {
	valid := map[string]bool{}
	for _, op := range ActionOps {
		valid[op] = true
	}
	seen := map[string]bool{}
	var out []model.ActionWord
	for i, l := range lines(text) {
		k, v, ok := splitPair(l)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("操作词表第 %d 行「%s」应写成「操作词=指令」", i+1, l)
		}
		if !valid[v] {
			return nil, fmt.Errorf("操作词表第 %d 行：指令「%s」不认识，可用：%s", i+1, v, strings.Join(ActionOps, " "))
		}
		if seen[k] {
			return nil, fmt.Errorf("操作词表第 %d 行：操作词「%s」重复", i+1, k)
		}
		seen[k] = true
		out = append(out, model.ActionWord{Word: k, Op: v})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("操作词表不能为空，否则微信里的指令都认不出来")
	}
	return out, nil
}

func FormatPrefixes(rules []model.PrefixRule) string {
	var b strings.Builder
	for _, r := range rules {
		fmt.Fprintf(&b, "%s=%s\n", r.Prefix, model.CategoryName(r.Category))
	}
	return b.String()
}

func FormatActionWords(words []model.ActionWord) string {
	var b strings.Builder
	for _, w := range words {
		fmt.Fprintf(&b, "%s=%s\n", w.Word, w.Op)
	}
	return b.String()
}
