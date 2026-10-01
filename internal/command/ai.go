package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/llm"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

// Translation 是 AI 返回的标准指令。
type Translation struct {
	Op         Op      `json:"op"`
	ID         int64   `json:"id"`
	When       string  `json:"when"` // "YYYY-MM-DD" 或 "YYYY-MM-DD HH:MM"
	Candidates []int64 `json:"candidates"`
}

// Translator 把一句自然语言翻译成标准指令。todos 是当前未完成的待办。
type Translator interface {
	Translate(ctx context.Context, text string, todos []model.Item, now time.Time) (Translation, error)
}

// Chatter 是一次 LLM 调用。装配时用 LLMChatter 适配 llm 客户端。
type Chatter interface {
	Chat(ctx context.Context, system, user string) (content string, promptTokens, completionTokens int, err error)
}

const systemPrompt = `你是一个待办助手的指令翻译器。用户用一句中文操作自己的待办，你把它翻译成一条标准指令。
只输出一个 JSON 对象，不要输出其他文字。字段：
- op：done（完成）、cancel（取消）、reschedule（改截止时间，「推迟」也用它）、list（列出待办）、undo（撤销上一次操作）、ask（拿不准是哪一条）、none（这句话不是在操作待办）
- id：要操作的待办编号，只能从给出的列表里选
- when：op=reschedule 时的新截止时间，"YYYY-MM-DD" 或 "YYYY-MM-DD HH:MM"；按给出的当前时间换算「明天」「月底」「下周三」这类说法
- candidates：op=ask 时可能的编号（2 到 3 个）
拿不准就用 ask，不要猜；列表里没有对应的待办，或这句话不是在操作待办，就用 none。`

type AITranslator struct {
	Chat   Chatter
	Record func(ctx context.Context, promptTokens, completionTokens int) // 记录用量，可为 nil
}

func (a *AITranslator) Translate(ctx context.Context, text string, todos []model.Item, now time.Time) (Translation, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "当前时间：%s %s（Asia/Shanghai）\n未完成的待办：\n", now.Format("2006-01-02 15:04"), model.WeekdayName(now))
	if len(todos) == 0 {
		b.WriteString("（无）\n")
	}
	for _, it := range todos {
		fmt.Fprintf(&b, "#%d %s", it.ID, model.TruncateRunes(it.DisplayTitle(), 40))
		if it.DueAt != nil {
			layout := "2006-01-02"
			if it.DueHasTime {
				layout = "2006-01-02 15:04"
			}
			fmt.Fprintf(&b, "（截止 %s）", it.DueAt.In(now.Location()).Format(layout))
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n用户说：%s\n", text)
	content, pt, ct, err := a.Chat.Chat(ctx, systemPrompt, b.String())
	if err != nil {
		return Translation{}, err
	}
	if a.Record != nil {
		a.Record(ctx, pt, ct)
	}
	return parseTranslation(content)
}

// parseTranslation 取回复里第一个「{」到最后一个「}」之间的 JSON，容忍代码块围栏和前后废话。
func parseTranslation(s string) (Translation, error) {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return Translation{}, errors.New("command: AI 回复里没有 JSON")
	}
	var t Translation
	if err := json.Unmarshal([]byte(s[i:j+1]), &t); err != nil {
		return Translation{}, fmt.Errorf("command: AI 回复的 JSON 不合法：%w", err)
	}
	t.Op = Op(strings.ToLower(strings.TrimSpace(string(t.Op))))
	return t, nil
}

// LLMChatter 把 llm 客户端适配成 Chatter。Model 应取轻量档（指令翻译只需要轻量模型）。
type LLMChatter struct {
	Client    llm.Chatter
	Model     string
	MaxTokens int // 0 用默认值 200
}

func (c *LLMChatter) Chat(ctx context.Context, system, user string) (string, int, int, error) {
	maxTokens := c.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 200
	}
	resp, err := c.Client.Chat(ctx, llm.Request{Model: c.Model, Messages: []llm.Message{llm.System(system), llm.User(user)}, MaxTokens: maxTokens})
	if err != nil {
		return "", 0, 0, err
	}
	return resp.Content, int(resp.Usage.PromptTokens), int(resp.Usage.CompletionTokens), nil
}

// UsageLevel 是指令翻译记进 llm_usage 的 level；不计入每日批量整理的限额判断。
const UsageLevel = "command"

// UsageRecorder 返回给 AITranslator.Record 用的函数：用量按上海日期记进 llm_usage（level=command）。
func UsageRecorder(st *store.Store, clk clock.Clock, provider, modelName string) func(ctx context.Context, promptTokens, completionTokens int) {
	return func(ctx context.Context, pt, ct int) {
		if err := st.AddUsage(context.WithoutCancel(ctx), store.Usage{Day: clock.DayString(clk.Now()), Level: UsageLevel, Provider: provider, Model: modelName,
			PromptTokens: int64(pt), CompletionTokens: int64(ct)}); err != nil {
			slog.Default().Error("记录指令翻译用量失败", "err", err)
		}
	}
}
