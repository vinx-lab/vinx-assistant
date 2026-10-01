package store

import (
	"context"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

// queryItems 执行一条 SELECT itemCols ... 查询，返回条目列表（不加载标签）。
func (s *Store) queryItems(ctx context.Context, q string, args ...any) ([]model.Item, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

// PendingForBatch 返回需要 AI 整理的条目：未整理的（还在收件箱、分类由 AI 定），或已处理深度低于要求深度的。
// 用户手动设回收件箱的（category_by='manual'）不算未整理，不会每次都被重新整理。
func (s *Store) PendingForBatch(ctx context.Context, maxAttempts int) ([]model.Item, error) {
	return s.queryItems(ctx, `SELECT `+itemCols+` FROM items
		WHERE process_attempts < ?
		  AND status NOT IN ('cancelled', 'dropped')
		  AND ((category = 'inbox' AND category_by = 'ai')
		       OR (CASE processed_level WHEN 'light' THEN 1 WHEN 'medium' THEN 2 WHEN 'deep' THEN 3 ELSE 0 END)
		        < (CASE level WHEN 'light' THEN 1 WHEN 'medium' THEN 2 WHEN 'deep' THEN 3 ELSE 0 END))
		ORDER BY id`, maxAttempts)
}
