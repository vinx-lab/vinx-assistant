package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

type Action struct {
	ID        int64
	MsgID     string // 来源消息的 msg_id，空表示无
	Command   string
	ItemID    int64
	Before    string // JSON
	After     string // JSON
	Undone    bool
	CreatedAt time.Time
}

const actCols = `id, msg_id, command, COALESCE(item_id, 0), before, after, undone, created_at`

// InsertAction 记录一次指令。MsgID 非空且已存在时返回 ErrDuplicate。
func (s *Store) InsertAction(ctx context.Context, a *Action) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = s.now()
	}
	var item any
	if a.ItemID != 0 {
		item = a.ItemID
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO actions (msg_id, command, item_id, before, after, undone, created_at) VALUES (?,?,?,?,?,0,?)`,
		a.MsgID, a.Command, item, a.Before, a.After, a.CreatedAt.Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDuplicate
	}
	a.ID, err = res.LastInsertId()
	return err
}

func scanAction(row *sql.Row) (*Action, error) {
	var a Action
	var undone int
	var created int64
	err := row.Scan(&a.ID, &a.MsgID, &a.Command, &a.ItemID, &a.Before, &a.After, &undone, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Undone, a.CreatedAt = undone == 1, fromUnix(created)
	return &a, nil
}

// LastAction 最近一条还没撤销的指令记录。
func (s *Store) LastAction(ctx context.Context) (*Action, error) {
	return scanAction(s.db.QueryRowContext(ctx, `SELECT `+actCols+` FROM actions WHERE undone = 0 ORDER BY id DESC LIMIT 1`))
}

// ActionByMsgID 按来源消息 msg_id 查指令记录（含已撤销的），没有则 ErrNotFound。
func (s *Store) ActionByMsgID(ctx context.Context, msgID string) (*Action, error) {
	return scanAction(s.db.QueryRowContext(ctx, `SELECT `+actCols+` FROM actions WHERE msg_id = ? AND msg_id <> ''`, msgID))
}

func (s *Store) MarkUndone(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions SET undone = 1 WHERE id = ?`, id)
	return err
}

func insertActionTx(ctx context.Context, tx *sql.Tx, a *Action, undone bool, now time.Time) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	var item any
	if a.ItemID != 0 {
		item = a.ItemID
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO actions (msg_id, command, item_id, before, after, undone, created_at) VALUES (?,?,?,?,?,?,?)`,
		a.MsgID, a.Command, item, a.Before, a.After, b2i(undone), a.CreatedAt.Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDuplicate
	}
	a.ID, err = res.LastInsertId()
	return err
}

// ModifyItemWithAction 在同一事务里改条目并记录指令。fn 返回 nil action 表示无改动（整体回滚，
// 返回 fn 的错误，通常是哨兵）；action.MsgID 已存在则整体回滚并返回 ErrDuplicate。
// action.ItemID 由本方法填成 id。
func (s *Store) ModifyItemWithAction(ctx context.Context, id int64, fn func(it *model.Item) (*Action, error)) (*model.Item, error) {
	var out *model.Item
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var act *Action
		it, err := s.modifyTx(ctx, tx, id, func(it *model.Item) error {
			var err error
			act, err = fn(it)
			return err
		})
		if err != nil {
			return err
		}
		act.ItemID = id
		out = it
		return insertActionTx(ctx, tx, act, false, s.now())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UndoAction 在同一事务里撤销 a：fn 把条目恢复到快照；a 标为已撤销；再记一条
// {msgID, "撤销", before=a.After, after=a.Before, undone=1} 用于重放去重（undone=1 使 LastAction 跳过它）。
// 条目不存在返回 ErrNotFound（整体回滚），重复 msg_id 返回 ErrDuplicate。
func (s *Store) UndoAction(ctx context.Context, a *Action, msgID string, fn func(it *model.Item) error) (*model.Item, error) {
	var out *model.Item
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		it, err := s.modifyTx(ctx, tx, a.ItemID, fn)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET undone = 1 WHERE id = ?`, a.ID); err != nil {
			return err
		}
		rec := &Action{MsgID: msgID, Command: "撤销", ItemID: a.ItemID, Before: a.After, After: a.Before}
		if err := insertActionTx(ctx, tx, rec, true, s.now()); err != nil {
			return err
		}
		out = it
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
