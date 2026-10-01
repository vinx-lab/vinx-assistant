package command

import (
	"context"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/model"
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
