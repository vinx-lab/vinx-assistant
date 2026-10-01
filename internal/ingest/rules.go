// Package ingest 处理收到的微信消息：去重、前缀和关键词、指令分流、入库、附件、回执。不调用 AI。
package ingest

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

type Input struct {
	Text     string // 文字和语音转写合并后的文本
	HasImage bool
	HasOther bool // 语音、文件、视频
}

type Parsed struct {
	Category   model.Category
	CategoryBy model.CategoryBy
	Level      model.Level
	Text       string // 去掉前缀后的文本
}

func isSep(r rune) bool {
	return r == ':' || r == '：' || r == ',' || r == '，' || unicode.IsSpace(r)
}

func matchPrefix(text string, prefixes []model.PrefixRule) (model.Category, string, bool) {
	sorted := append([]model.PrefixRule(nil), prefixes...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].Prefix) > len(sorted[j].Prefix) })
	for _, p := range sorted {
		if p.Prefix == "" || !model.ValidCategory(p.Category) || !strings.HasPrefix(text, p.Prefix) {
			continue
		}
		rest := text[len(p.Prefix):]
		r, _ := utf8.DecodeRuneInString(rest)
		if rest == "" || !isSep(r) {
			continue
		}
		return p.Category, strings.TrimSpace(strings.TrimLeftFunc(rest, isSep)), true
	}
	return "", "", false
}

func Classify(in Input, rules model.Rules, aiImages bool) Parsed {
	text := strings.TrimSpace(in.Text)
	p := Parsed{Category: model.CatInbox, CategoryBy: model.ByAI, Level: model.LevelLight, Text: text}
	if cat, rest, ok := matchPrefix(text, rules.Prefixes); ok {
		p.Category, p.CategoryBy, p.Text = cat, model.ByPrefix, rest
	} else if text == "" && in.HasImage && !aiImages {
		p.Category, p.CategoryBy = model.CatArchive, model.ByPrefix
	}
	for _, k := range rules.DeepKeywords {
		if k != "" && strings.Contains(p.Text, k) {
			p.Level = model.LevelDeep
			return p
		}
	}
	for _, k := range rules.MediumKeywords {
		if k != "" && strings.Contains(p.Text, k) {
			p.Level = model.LevelMedium
			return p
		}
	}
	return p
}

// IsCommand 判断是否交给指令处理：以操作词开头，或引用了别的消息。
func IsCommand(text string, hasRef bool, words []model.ActionWord) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if hasRef {
		return true
	}
	for _, w := range words {
		if w.Word != "" && strings.HasPrefix(t, w.Word) {
			return true
		}
	}
	return false
}
