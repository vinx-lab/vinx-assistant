package store

import (
	"context"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

// OpenTodos 未完成待办：有截止的在前（按截止升序），没截止的在后（按编号）。
func (s *Store) OpenTodos(ctx context.Context, limit int) ([]model.Item, error) {
	return s.queryItems(ctx, `SELECT `+itemCols+` FROM items WHERE category = 'todo' AND status = 'open'
		ORDER BY due_at IS NULL, due_at, id LIMIT ?`, limit)
}

type DigestData struct {
	Overdue         []model.Item
	DueToday        []model.Item
	DueTomorrow     []model.Item
	ResearchBacklog int
	TopResearch     []model.Item
	NewYesterday    int
}

func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// DigestData 汇总每日摘要需要的数据。
// 逾期：带时刻的截止早于现在，或只有日期的截止早于今天。
// 今天到期：截止在今天且未逾期（只有日期的算全天）。明天到期：截止在明天。
func (s *Store) DigestData(ctx context.Context, now time.Time) (DigestData, error) {
	var d DigestData
	today := dayStart(now)
	tomorrow, after, yesterday := today.AddDate(0, 0, 1), today.AddDate(0, 0, 2), today.AddDate(0, 0, -1)
	base := `SELECT ` + itemCols + ` FROM items WHERE category = 'todo' AND status = 'open' AND due_at IS NOT NULL AND `
	var err error
	if d.Overdue, err = s.queryItems(ctx, base+`((due_has_time = 1 AND due_at < ?) OR (due_has_time = 0 AND due_at < ?)) ORDER BY due_at, id`,
		now.Unix(), today.Unix()); err != nil {
		return d, err
	}
	if d.DueToday, err = s.queryItems(ctx, base+`due_at >= ? AND due_at < ? AND NOT (due_has_time = 1 AND due_at < ?) ORDER BY due_at, id`,
		today.Unix(), tomorrow.Unix(), now.Unix()); err != nil {
		return d, err
	}
	if d.DueTomorrow, err = s.queryItems(ctx, base+`due_at >= ? AND due_at < ? ORDER BY due_at, id`,
		tomorrow.Unix(), after.Unix()); err != nil {
		return d, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE category = 'research' AND status IN ('new','doing')`).Scan(&d.ResearchBacklog); err != nil {
		return d, err
	}
	if d.TopResearch, err = s.queryItems(ctx, `SELECT `+itemCols+` FROM items WHERE category = 'research' AND status IN ('new','doing')
		ORDER BY CASE priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 ELSE 3 END, created_at, id LIMIT 3`); err != nil {
		return d, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE created_at >= ? AND created_at < ?`, yesterday.Unix(), today.Unix()).Scan(&d.NewYesterday)
	return d, err
}
