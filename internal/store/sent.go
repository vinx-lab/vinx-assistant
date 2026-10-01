package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

// RecordSent 记下发出的一条消息。msgID 为空（服务端没返回）时不记。
func (s *Store) RecordSent(ctx context.Context, msgID, body string) error {
	if msgID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sent_msgs (msg_id, body, sent_at) VALUES (?, ?, ?) ON CONFLICT(msg_id) DO NOTHING`, msgID, body, s.now().Unix())
	return err
}

func (s *Store) SentBody(ctx context.Context, msgID string) (string, bool, error) {
	var body string
	err := s.db.QueryRowContext(ctx, `SELECT body FROM sent_msgs WHERE msg_id = ?`, msgID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return body, err == nil, err
}

// PruneSent 删除早于 before 的记录（引用很少跨月，保留 30 天就够，与上游默认一致）。
func (s *Store) PruneSent(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sent_msgs WHERE sent_at < ?`, before.Unix())
	return err
}

// GetItemByMsgID 按微信消息 ID 找条目（用户引用自己发过的消息时用）。
func (s *Store) GetItemByMsgID(ctx context.Context, msgID string) (*model.Item, error) {
	it, err := scanItem(s.db.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE msg_id = ?`, msgID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return it, err
}
