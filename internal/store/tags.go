package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

type TagCount struct {
	Name  string
	Count int
}

// tagID 按名称忽略大小写查找标签，没有就新建。
func tagID(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE name = ? COLLATE NOCASE ORDER BY id LIMIT 1`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO tags (name) VALUES (?)`, name)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) linkTags(ctx context.Context, tx *sql.Tx, itemID int64, tags []string) error {
	for _, name := range model.NormalizeTags(tags) {
		id, err := tagID(ctx, tx, name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO item_tags (item_id, tag_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, itemID, id); err != nil {
			return err
		}
	}
	return s.Reindex(ctx, tx, itemID)
}

// AddTags 把标签合并到条目上（AI 批次用）。
func (s *Store) AddTags(ctx context.Context, itemID int64, tags []string) error {
	if len(model.NormalizeTags(tags)) == 0 {
		return nil
	}
	return s.Tx(ctx, func(tx *sql.Tx) error { return s.linkTags(ctx, tx, itemID, tags) })
}

// SetTags 整体替换条目的标签（网页上手改用）。
func (s *Store) SetTags(ctx context.Context, itemID int64, tags []string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM item_tags WHERE item_id = ?`, itemID); err != nil {
			return err
		}
		return s.linkTags(ctx, tx, itemID, tags)
	})
}

func (s *Store) AllTags(ctx context.Context) ([]TagCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.name, count(*) AS c FROM tags t JOIN item_tags x ON x.tag_id = t.id GROUP BY t.id ORDER BY c DESC, t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TagCount
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Name, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}
