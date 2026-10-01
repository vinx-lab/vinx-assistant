package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

const itemCols = `id, created_at, updated_at, msg_id, raw_text, url, link_title, link_desc, category, category_by, level, status, title, title_by, summary, detail, priority, due_at, due_has_time, processed_level, process_error, process_attempts, tokens_used, raw_json`

type scanner interface{ Scan(dest ...any) error }

func scanItem(sc scanner) (*model.Item, error) {
	var (
		it               model.Item
		created, updated int64
		due              sql.NullInt64
		hasTime          int
		cat, by, lvl     string
		prio, plvl       string
	)
	err := sc.Scan(&it.ID, &created, &updated, &it.MsgID, &it.RawText, &it.URL, &it.LinkTitle, &it.LinkDesc,
		&cat, &by, &lvl, &it.Status, &it.Title, &it.TitleBy, &it.Summary, &it.Detail, &prio, &due, &hasTime,
		&plvl, &it.ProcessError, &it.ProcessAttempts, &it.TokensUsed, &it.RawJSON)
	if err != nil {
		return nil, err
	}
	it.CreatedAt, it.UpdatedAt = fromUnix(created), fromUnix(updated)
	it.Category, it.CategoryBy, it.Level = model.Category(cat), model.CategoryBy(by), model.Level(lvl)
	it.Priority, it.ProcessedLevel = model.Priority(prio), model.Level(plvl)
	if due.Valid {
		t := fromUnix(due.Int64)
		it.DueAt = &t
	}
	it.DueHasTime = hasTime == 1
	return &it, nil
}

// loadTags 读条目上的标签，分成类别标签和内容标签。
func loadTags(ctx context.Context, q querier, itemID int64) (labels, topics []string, err error) {
	rows, err := q.QueryContext(ctx, `SELECT t.name, t.kind FROM item_tags x JOIN tags t ON t.id = x.tag_id WHERE x.item_id = ? ORDER BY t.name`, itemID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n, k string
		if err := rows.Scan(&n, &k); err != nil {
			return nil, nil, err
		}
		if k == TagKindLabel {
			labels = append(labels, n)
		} else {
			topics = append(topics, n)
		}
	}
	return labels, topics, rows.Err()
}

func fillDefaults(it *model.Item) {
	if it.Category == "" {
		it.Category = model.CatInbox
	}
	if it.CategoryBy == "" {
		it.CategoryBy = model.ByAI
	}
	if it.Level == "" {
		it.Level = model.LevelLight
	}
	if it.Status == "" {
		it.Status = model.DefaultStatus(it.Category)
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// InsertItem 新建条目并设置 it.ID。msg_id 已存在时返回 ErrDuplicate。
func (s *Store) InsertItem(ctx context.Context, it *model.Item) (int64, error) {
	now := s.now()
	if it.CreatedAt.IsZero() {
		it.CreatedAt = now
	}
	it.UpdatedAt = now
	fillDefaults(it)
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO items (created_at, updated_at, msg_id, raw_text, url, link_title, link_desc,
			category, category_by, level, status, title, title_by, summary, detail, priority, due_at, due_has_time,
			processed_level, process_error, process_attempts, tokens_used, raw_json)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(msg_id) DO NOTHING`,
			it.CreatedAt.Unix(), it.UpdatedAt.Unix(), it.MsgID, it.RawText, it.URL, it.LinkTitle, it.LinkDesc,
			it.Category, it.CategoryBy, it.Level, it.Status, it.Title, it.TitleBy, it.Summary, it.Detail, it.Priority,
			unixOrNil(it.DueAt), b2i(it.DueHasTime), it.ProcessedLevel, it.ProcessError, it.ProcessAttempts,
			it.TokensUsed, it.RawJSON)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrDuplicate
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		it.ID = id
		return s.Reindex(ctx, tx, id)
	})
	return it.ID, err
}

func (s *Store) GetItem(ctx context.Context, id int64) (*model.Item, error) {
	it, err := scanItem(s.db.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	it.Labels, it.Topics, err = loadTags(ctx, s.db, id)
	return it, err
}

// UpdateItem 写回全部可变列（不含 created_at、msg_id、raw_json），并重建全文索引。
func (s *Store) UpdateItem(ctx context.Context, it *model.Item) error {
	it.UpdatedAt = s.now()
	return s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE items SET updated_at=?, raw_text=?, url=?, link_title=?, link_desc=?,
			category=?, category_by=?, level=?, status=?, title=?, title_by=?, summary=?, detail=?, priority=?, due_at=?,
			due_has_time=?, processed_level=?, process_error=?, process_attempts=?, tokens_used=? WHERE id=?`,
			it.UpdatedAt.Unix(), it.RawText, it.URL, it.LinkTitle, it.LinkDesc, it.Category, it.CategoryBy,
			it.Level, it.Status, it.Title, it.TitleBy, it.Summary, it.Detail, it.Priority, unixOrNil(it.DueAt),
			b2i(it.DueHasTime), it.ProcessedLevel, it.ProcessError, it.ProcessAttempts, it.TokensUsed, it.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return s.Reindex(ctx, tx, it.ID)
	})
}

// ModifyItem 在一个事务里读出条目（含标签）、交给 fn 修改、写回全部可变列并重建索引，返回修改后的条目。
// 用于「读出后要等很久才写回」的场景（如 AI 批处理）：读和写在同一事务里，不会覆盖期间别处的修改。
// fn 返回错误则回滚并原样返回；条目不存在返回 ErrNotFound。
func (s *Store) ModifyItem(ctx context.Context, id int64, fn func(it *model.Item) error) (*model.Item, error) {
	var out *model.Item
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.modifyTx(ctx, tx, id, fn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// modifyTx 是 ModifyItem 的事务体：读出条目、交给 fn 修改、写回全部可变列并重建索引。
func (s *Store) modifyTx(ctx context.Context, tx *sql.Tx, id int64, fn func(it *model.Item) error) (*model.Item, error) {
	it, err := scanItem(tx.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if it.Labels, it.Topics, err = loadTags(ctx, tx, id); err != nil {
		return nil, err
	}
	if err := fn(it); err != nil {
		return nil, err
	}
	it.ID = id
	it.UpdatedAt = s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE items SET updated_at=?, raw_text=?, url=?, link_title=?, link_desc=?,
		category=?, category_by=?, level=?, status=?, title=?, title_by=?, summary=?, detail=?, priority=?, due_at=?,
		due_has_time=?, processed_level=?, process_error=?, process_attempts=?, tokens_used=? WHERE id=?`,
		it.UpdatedAt.Unix(), it.RawText, it.URL, it.LinkTitle, it.LinkDesc, it.Category, it.CategoryBy,
		it.Level, it.Status, it.Title, it.TitleBy, it.Summary, it.Detail, it.Priority, unixOrNil(it.DueAt),
		b2i(it.DueHasTime), it.ProcessedLevel, it.ProcessError, it.ProcessAttempts, it.TokensUsed, id); err != nil {
		return nil, err
	}
	return it, s.Reindex(ctx, tx, id)
}

// SetLink 由轻处理写入链接和网页标题、简介。
func (s *Store) SetLink(ctx context.Context, id int64, url, title, desc string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE items SET url=?, link_title=?, link_desc=?, updated_at=? WHERE id=?`, url, title, desc, s.now().Unix(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return s.Reindex(ctx, tx, id)
	})
}

// Reindex 重建一个条目的全文索引行。改了标题、摘要、标签等之后都要在同一事务里调用。
func (s *Store) Reindex(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM items_fts WHERE rowid = ?`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO items_fts (rowid, title, raw_text, summary, detail, tags, link_title)
		SELECT i.id, i.title, i.raw_text, i.summary, i.detail,
		       COALESCE((SELECT group_concat(t.name, ' ') FROM item_tags x JOIN tags t ON t.id = x.tag_id WHERE x.item_id = i.id), ''),
		       i.link_title
		FROM items i WHERE i.id = ?`, id)
	return err
}

// DeleteItem 在一个事务里删除条目及其关联数据：标签关联、附件记录、提醒、全文索引行、引用它的指令记录。
// tags 表本身、seen_msgs（保留去重，同一条消息重投时不会再入库）和 llm_usage（用量记录，item_id 置空）不删。
// 返回该条目附件在媒体目录下的相对路径，事务提交后由调用方删文件。条目不存在时返回 ErrNotFound。
func (s *Store) DeleteItem(ctx context.Context, id int64) ([]string, error) {
	var paths []string
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM items WHERE id = ?`, id).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT rel_path FROM attachments WHERE item_id = ? AND rel_path <> '' ORDER BY id`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			paths = append(paths, p)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM item_tags WHERE item_id = ?`,
			`DELETE FROM attachments WHERE item_id = ?`,
			`DELETE FROM reminders WHERE item_id = ?`,
			`DELETE FROM actions WHERE item_id = ?`,
			`DELETE FROM items_fts WHERE rowid = ?`,
			`DELETE FROM items WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}
