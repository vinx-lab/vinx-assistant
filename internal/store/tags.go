package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

type TagCount struct {
	Name  string
	Kind  string
	Count int
}

const (
	TagKindLabel = "label" // 类别标签：关键词规则产生
	TagKindTopic = "topic" // 内容标签：AI 生成
)

// tagID 按名称忽略大小写查找标签，没有就按 kind 新建。名称全局唯一（忽略大小写）：
//   - 已有同名 label 时，想加 topic 返回 ok=false（跳过）；
//   - 已有同名 topic 时，想加 label 把它升级为 label。
func tagID(ctx context.Context, tx *sql.Tx, name, kind string) (id int64, ok bool, err error) {
	var cur string
	err = tx.QueryRowContext(ctx, `SELECT id, kind FROM tags WHERE name = ? COLLATE NOCASE ORDER BY id LIMIT 1`, name).Scan(&id, &cur)
	if err == nil {
		switch {
		case cur == kind:
		case kind == TagKindLabel:
			if _, err := tx.ExecContext(ctx, `UPDATE tags SET kind = ? WHERE id = ?`, TagKindLabel, id); err != nil {
				return 0, false, err
			}
		default:
			return 0, false, nil
		}
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO tags (name, kind) VALUES (?, ?)`, name, kind)
	if err != nil {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	return id, err == nil, err
}

func checkKind(kind string) error {
	if kind != TagKindLabel && kind != TagKindTopic {
		return fmt.Errorf("store: 未知标签类型 %q", kind)
	}
	return nil
}

func (s *Store) linkTags(ctx context.Context, tx *sql.Tx, itemID int64, kind string, tags []string) error {
	for _, name := range model.NormalizeTags(tags) {
		id, ok, err := tagID(ctx, tx, name, kind)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO item_tags (item_id, tag_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, itemID, id); err != nil {
			return err
		}
	}
	return s.Reindex(ctx, tx, itemID)
}

// AddTags 把某种标签合并到条目上（关键词规则、AI 批次用）。
func (s *Store) AddTags(ctx context.Context, itemID int64, kind string, tags []string) error {
	if err := checkKind(kind); err != nil {
		return err
	}
	if len(model.NormalizeTags(tags)) == 0 {
		return nil
	}
	return s.Tx(ctx, func(tx *sql.Tx) error { return s.linkTags(ctx, tx, itemID, kind, tags) })
}

// SetTags 整体替换条目上某一种标签（网页上手改用），另一种不动。
func (s *Store) SetTags(ctx context.Context, itemID int64, kind string, tags []string) error {
	if err := checkKind(kind); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM item_tags WHERE item_id = ? AND tag_id IN (SELECT id FROM tags WHERE kind = ?)`, itemID, kind); err != nil {
			return err
		}
		return s.linkTags(ctx, tx, itemID, kind, tags)
	})
}

// AllTags 返回标签及条目数；kind 为空返回全部。
func (s *Store) AllTags(ctx context.Context, kind string) ([]TagCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.name, t.kind, count(*) AS c FROM tags t JOIN item_tags x ON x.tag_id = t.id WHERE (? = '' OR t.kind = ?) GROUP BY t.id ORDER BY c DESC, t.name`, kind, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TagCount
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Name, &tc.Kind, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}
