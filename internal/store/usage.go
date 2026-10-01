package store

import (
	"context"
	"database/sql"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

// Usage 是一次 AI 调用的用量。ItemID 为 0 表示批量调用，不关联单个条目。
type Usage struct {
	Day              string
	Level            string
	Provider         string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	ItemID           int64
}

// UsageLevelCommand 是微信指令 AI 翻译记进 llm_usage 的 level；不计入整理的每日限额。
const UsageLevelCommand = "command"

func (s *Store) AddUsage(ctx context.Context, u Usage) error {
	var item any
	if u.ItemID != 0 {
		item = u.ItemID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO llm_usage (day, level, provider, model, prompt_tokens, completion_tokens, item_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, u.Day, u.Level, u.Provider, u.Model, u.PromptTokens, u.CompletionTokens, item, s.now().Unix())
	return err
}

// TokensOn 返回某天（上海日期 "YYYY-MM-DD"）整理已用的 token 总数（不含指令翻译），供每日限额判断。
func (s *Store) TokensOn(ctx context.Context, day string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0) FROM llm_usage WHERE day = ? AND level <> ?`, day, UsageLevelCommand).Scan(&n)
	return n, err
}

type DayUsage struct {
	Day              string
	PromptTokens     int64
	CompletionTokens int64
	Calls            int
}

func (s *Store) UsageByDay(ctx context.Context, limit int) ([]DayUsage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT day, SUM(prompt_tokens), SUM(completion_tokens), count(*) FROM llm_usage GROUP BY day ORDER BY day DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayUsage
	for rows.Next() {
		var d DayUsage
		if err := rows.Scan(&d.Day, &d.PromptTokens, &d.CompletionTokens, &d.Calls); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) TopItemsByTokens(ctx context.Context, limit int) ([]model.Item, error) {
	return s.queryItems(ctx, `SELECT `+itemCols+` FROM items WHERE tokens_used > 0 ORDER BY tokens_used DESC, id LIMIT ?`, limit)
}

func (s *Store) FailedItems(ctx context.Context, limit int) ([]model.Item, error) {
	return s.queryItems(ctx, `SELECT `+itemCols+` FROM items WHERE process_error != '' ORDER BY updated_at DESC, id DESC LIMIT ?`, limit)
}

// DB 暴露底层连接，只给测试和只读诊断用。
func (s *Store) DB() *sql.DB { return s.db }
