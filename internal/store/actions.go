package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
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
