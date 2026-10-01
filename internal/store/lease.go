package store

import (
	"context"
	"strconv"
	"time"
)

// TryLease 抢一个带过期时间的租约（存在 kv 里，值是到期的 Unix 秒）。
// 用来保证 serve 进程和 batch 子命令不会同时整理。
func (s *Store) TryLease(ctx context.Context, key string, now time.Time, ttl time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value WHERE CAST(kv.value AS INTEGER) <= ?`,
		key, strconv.FormatInt(now.Add(ttl).Unix(), 10), now.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) ReleaseLease(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, key)
	return err
}
