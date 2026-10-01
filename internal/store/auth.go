package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// authKey 是 settings 表里保存网页密码记录的键（内容见 auth.Record，store 不解析）。
const authKey = "auth"

// PasswordRecord 读网页密码记录；没设密码时返回 ""。
func (s *Store) PasswordRecord(ctx context.Context) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, authKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetPassword 保存新的密码记录，并吊销除 keep 之外的所有会话（keep 为空时全部吊销）。
func (s *Store) SetPassword(ctx context.Context, record, keep string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, authKey, record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM web_sessions WHERE token_hash <> ?`, keep)
		return err
	})
}

// SetPasswordIfUnset 只在还没设密码时保存记录（首次设置用，防止覆盖并发设好的密码），并吊销全部会话。
// 已有密码时不改动任何东西，返回 false。
func (s *Store) SetPasswordIfUnset(ctx context.Context, record string) (bool, error) {
	var inserted bool
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM settings WHERE key = ?)`, authKey, record, authKey)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		inserted = true
		_, err = tx.ExecContext(ctx, `DELETE FROM web_sessions`)
		return err
	})
	return inserted, err
}

// ClearPassword 清除密码并吊销所有会话，恢复成无密码状态。
func (s *Store) ClearPassword(ctx context.Context) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, authKey); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM web_sessions`)
		return err
	})
}

// CreateSession 保存一个会话（令牌哈希），顺带清理已过期的会话。
func (s *Store) CreateSession(ctx context.Context, tokenHash string, now, expires time.Time) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM web_sessions WHERE expires_at <= ?`, now.Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO web_sessions (token_hash, created_at, expires_at) VALUES (?, ?, ?)`, tokenHash, now.Unix(), expires.Unix())
		return err
	})
}

// SessionValid 报告会话是否存在且未过期；过期的顺手删掉。
func (s *Store) SessionValid(ctx context.Context, tokenHash string, now time.Time) (bool, error) {
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM web_sessions WHERE token_hash = ?`, tokenHash).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if exp > now.Unix() {
		return true, nil
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE token_hash = ?`, tokenHash)
	return false, err
}

// DeleteSession 吊销一个会话（退出当前设备）。
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteAllSessions 吊销全部会话（退出所有设备）。
func (s *Store) DeleteAllSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions`)
	return err
}

// CountSessions 是当前保存的会话数（含未清理的过期会话），测试和排查用。
func (s *Store) CountSessions(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM web_sessions`).Scan(&n)
	return n, err
}
