// Package ingest 处理收到的微信消息：去重、前缀和关键词、指令分流、入库、附件、回执。不调用 AI。
package ingest

import (
	"strings"
	"unicode"

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
	Text       string // 关键词在开头时去掉关键词后的文本，否则是原文
}

func isSep(r rune) bool {
	return r == ':' || r == '：' || r == ',' || r == '，' || unicode.IsSpace(r)
}

// matchKeyword 在文本里找分类关键词，出现在任何位置都算，不要求分隔符。
//  1. 取所有合法关键词（非空、分类合法）的最早出现位置；同位置多个命中取更长的（「待研究」优先于「研究」）。
//  2. 命中在开头：去掉关键词和紧随其后的分隔符，剩下的作为正文；去掉后为空则保留原文。
//  3. 命中不在开头：正文保持原文。
func matchKeyword(text string, prefixes []model.PrefixRule) (model.Category, string, bool) {
	best, bestPos := -1, 0
	for i, p := range prefixes {
		if p.Prefix == "" || !model.ValidCategory(p.Category) {
			continue
		}
		pos := strings.Index(text, p.Prefix)
		if pos < 0 {
			continue
		}
		if best < 0 || pos < bestPos || (pos == bestPos && len(p.Prefix) > len(prefixes[best].Prefix)) {
			best, bestPos = i, pos
		}
	}
	if best < 0 {
		return "", "", false
	}
	cat := prefixes[best].Category
	if bestPos != 0 {
		return cat, text, true
	}
	rest := strings.TrimSpace(strings.TrimLeftFunc(text[len(prefixes[best].Prefix):], isSep))
	if rest == "" {
		rest = text
	}
	return cat, rest, true
}

func Classify(in Input, rules model.Rules, aiImages bool) Parsed {
	text := strings.TrimSpace(in.Text)
	p := Parsed{Category: model.CatInbox, CategoryBy: model.ByAI, Level: model.LevelLight, Text: text}
	if cat, rest, ok := matchKeyword(text, rules.Prefixes); ok {
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
