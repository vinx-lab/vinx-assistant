package store

import (
	"context"
	"strconv"
	"time"
)

// 租约存在 kv 里，值是「到期 Unix 秒:持有者」。用来保证 serve 进程和 batch 子命令不会同时整理。
// 只有持有者能续期和释放；过期的租约任何人都能接管。

// leaseOwnerIs 判断 kv.value 的持有者部分（第一个冒号之后）是否等于参数。
const leaseOwnerIs = `substr(value, instr(value, ':') + 1) = ?`

func leaseValue(now time.Time, ttl time.Duration, owner string) string {
	return strconv.FormatInt(now.Add(ttl).Unix(), 10) + ":" + owner
}

// TryLease 抢租约：不存在或已过期时成功。
func (s *Store) TryLease(ctx context.Context, key, owner string, now time.Time, ttl time.Duration) (bool, error) {
	// CAST 取值开头的数字部分，即到期时间。
	res, err := s.db.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value WHERE CAST(kv.value AS INTEGER) <= ?`,
		key, leaseValue(now, ttl, owner), now.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RenewLease 把到期时间顺延到 now+ttl。租约已不属于 owner 时返回 false。
func (s *Store) RenewLease(ctx context.Context, key, owner string, now time.Time, ttl time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE kv SET value = ? WHERE key = ? AND `+leaseOwnerIs,
		leaseValue(now, ttl, owner), key, owner)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseLease 释放 owner 持有的租约；已被别人接管时什么也不做。
func (s *Store) ReleaseLease(ctx context.Context, key, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kv WHERE key = ? AND `+leaseOwnerIs, key, owner)
	return err
}
