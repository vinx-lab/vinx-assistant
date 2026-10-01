package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AICommand 是一条等后台 AI 翻译执行的指令。
type AICommand struct {
	MsgID     string
	Text      string
	RefText   string
	HasWord   bool // 以操作词开头：处理不了时回用法提示；否则当普通条目保存
	CreatedAt time.Time
	Done      bool
	DoneAt    time.Time
	Reply     string // 处理完时回给用户的文字（可能为空）
}

const aiCmdCols = `msg_id, text, ref_text, has_word, created_at, done_at, reply`

func scanAICommand(sc interface{ Scan(...any) error }) (*AICommand, error) {
	var c AICommand
	var hasWord int
	var created int64
	var done sql.NullInt64
	if err := sc.Scan(&c.MsgID, &c.Text, &c.RefText, &hasWord, &created, &done, &c.Reply); err != nil {
		return nil, err
	}
	c.HasWord, c.CreatedAt = hasWord == 1, fromUnix(created)
	if done.Valid {
		c.Done, c.DoneAt = true, fromUnix(done.Int64)
	}
	return &c, nil
}

// EnqueueAICommand 按 msg_id 落库一条待处理指令。同一 msg_id 已存在（处理中或已处理）返回 ErrDuplicate。
func (s *Store) EnqueueAICommand(ctx context.Context, c *AICommand) error {
	if c.MsgID == "" {
		return errors.New("store: AI 指令缺少 msg_id")
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = s.now()
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO ai_commands (msg_id, text, ref_text, has_word, created_at) VALUES (?,?,?,?,?)`,
		c.MsgID, c.Text, c.RefText, b2i(c.HasWord), c.CreatedAt.Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDuplicate
	}
	return nil
}

// AICommandByMsgID 没有则 ErrNotFound。
func (s *Store) AICommandByMsgID(ctx context.Context, msgID string) (*AICommand, error) {
	c, err := scanAICommand(s.db.QueryRowContext(ctx, `SELECT `+aiCmdCols+` FROM ai_commands WHERE msg_id = ?`, msgID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// PendingAICommands 还没处理完的指令，按收到的先后。
func (s *Store) PendingAICommands(ctx context.Context, limit int) ([]AICommand, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+aiCmdCols+` FROM ai_commands WHERE done_at IS NULL ORDER BY created_at, rowid LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AICommand
	for rows.Next() {
		c, err := scanAICommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// FinishAICommand 标记处理完，记下回复。
func (s *Store) FinishAICommand(ctx context.Context, msgID, reply string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE ai_commands SET done_at = ?, reply = ? WHERE msg_id = ?`, s.now().Unix(), reply, msgID)
	return err
}

// PruneAICommands 删除 before 之前已处理完的记录（到这时消息早已 MarkSeen，不会再重放）。
func (s *Store) PruneAICommands(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ai_commands WHERE done_at IS NOT NULL AND done_at < ?`, before.Unix())
	return err
}
