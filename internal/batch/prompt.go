// Package batch 是定时 AI 整理：分类、打标签、写摘要、提取截止时间，按深度分档，受每日 token 上限约束。
package batch

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vinx-lab/vinx-assistant/internal/llm"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

const timeLayout = "2006-01-02 15:04"

var weekdays = [...]string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}

func levelName(l model.Level) string {
	switch l {
	case model.LevelMedium:
		return "中等"
	case model.LevelDeep:
		return "深度"
	}
	return "轻量"
}

type promptItem struct {
	ID            int64  `json:"id"`
	Received      string `json:"received"`
	Text          string `json:"text"`
	URL           string `json:"url,omitempty"`
	LinkTitle     string `json:"link_title,omitempty"`
	LinkDesc      string `json:"link_desc,omitempty"`
	FixedCategory string `json:"fixed_category,omitempty"`
	Content       string `json:"content,omitempty"`
	ContentNote   string `json:"content_note,omitempty"`
}

func toPromptItem(it *model.Item, maxText int) promptItem {
	p := promptItem{
		ID:        it.ID,
		Received:  it.CreatedAt.Format(timeLayout),
		Text:      model.TruncateRunes(it.RawText, maxText),
		URL:       it.URL,
		LinkTitle: it.LinkTitle,
		LinkDesc:  it.LinkDesc,
	}
	if it.CategoryBy == model.ByPrefix || it.CategoryBy == model.ByManual {
		p.FixedCategory = string(it.Category)
	}
	return p
}

func systemPrompt(level model.Level, now time.Time, tags []string) string {
	var b strings.Builder
	b.WriteString("你是个人收集箱的整理助手。用户把平时看到的内容、想到的点子和要做的事发给自己，你负责整理。\n")
	b.WriteString("只输出一个 JSON 对象，不要输出任何其他文字。\n\n")
	fmt.Fprintf(&b, "当前时间：%s %s（Asia/Shanghai）。条目里「月底前」「下周三」「明天下午三点」这类说法，按该条目的 received 时间换算成具体日期。\n\n",
		now.Format(timeLayout), weekdays[now.Weekday()])
	b.WriteString("category 只能是下面之一：\n")
	b.WriteString("- research：待研究，值得花时间深入了解的项目、技术、长文\n")
	b.WriteString("- later：稍后看，看看就行的文章、视频、帖子\n")
	b.WriteString("- todo：待办，需要用户自己去做的事\n")
	b.WriteString("- idea：点子、想法\n")
	b.WriteString("- archive：资料，票据、截图、文件等留存备查的东西\n")
	b.WriteString("条目带 fixed_category 时，category 必须原样照抄。\n\n")
	if len(tags) > 0 {
		fmt.Fprintf(&b, "已有标签（优先复用，意思相同就用已有的写法）：%s\n", strings.Join(tags, "、"))
	}
	b.WriteString("每条最多 5 个标签，每个不超过 20 个字。\n")
	b.WriteString("content 是抓取到的网页正文或 README，可能不完整；content_note 说明抓取失败的原因。\n\n")
	b.WriteString("输出格式：\n")
	b.WriteString(`{"items":[{"id":原样返回条目的 id,"category":"todo","tags":["发票"],"title":"不超过 30 字的标题","summary":"一句话摘要",`)
	switch level {
	case model.LevelMedium:
		b.WriteString(`"detail":"3 到 5 句话的摘要，最后一句说明值不值得花时间看、为什么",`)
	case model.LevelDeep:
		b.WriteString(`"detail":"Markdown 格式的研究笔记，包含三个小节：## 是什么、## 同类对比、## 上手步骤",`)
	}
	b.WriteString(`"due":"YYYY-MM-DD 或 YYYY-MM-DD HH:MM，没有截止时间就留空字符串","priority":"high、medium 或 low"}]}`)
	b.WriteString("\n每个输入条目都必须输出一项。\n")
	if level == model.LevelDeep {
		b.WriteString("「同类对比」只基于你已有的知识，不要编造链接、数据或版本号；不确定的地方直接写不确定。\n")
	}
	return b.String()
}

func userPrompt(items []promptItem) string {
	b, _ := json.Marshal(map[string]any{"items": items})
	return "需要整理的条目：\n" + string(b)
}

func maxTokens(level model.Level, n int) int {
	switch level {
	case model.LevelMedium:
		return 1500
	case model.LevelDeep:
		return 4000
	}
	return min(300*n+200, 6000)
}

func buildRequest(level model.Level, now time.Time, tags []string, items []promptItem, images []string, modelName string) llm.Request {
	user := llm.User(userPrompt(items))
	if len(images) > 0 {
		user = llm.UserWithImages(userPrompt(items), images)
	}
	return llm.Request{
		Model:     modelName,
		Messages:  []llm.Message{llm.System(systemPrompt(level, now, tags)), user},
		MaxTokens: maxTokens(level, len(items)),
	}
}

// imageTokens 是一张低清晰度图片的保守估算。
const imageTokens = 300

// Estimate 保守估算一次请求的 token：文本按字符数算（中文约 1 字 1 token，英文会高估），
// 每张图按 300 算，再加上 max_tokens。
func Estimate(req llm.Request) int64 {
	var n int64
	for _, m := range req.Messages {
		switch c := m.Content.(type) {
		case string:
			n += int64(utf8.RuneCountInString(c))
		case []llm.Part:
			for _, p := range c {
				if p.Type == "text" {
					n += int64(utf8.RuneCountInString(p.Text))
				} else {
					n += imageTokens
				}
			}
		}
	}
	return n + int64(req.MaxTokens)
}

// Budget 是当天的 token 预算。Limit <= 0 表示不限。
type Budget struct {
	Limit int64
	Used  int64
}

func (b *Budget) Remaining() int64 {
	if b.Limit <= 0 {
		return math.MaxInt64
	}
	return b.Limit - b.Used
}

func (b *Budget) Fits(est int64) bool { return est <= b.Remaining() }
func (b *Budget) Spend(n int64)       { b.Used += n }
