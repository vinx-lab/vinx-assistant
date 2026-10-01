package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// loginRequiredKey 是「需要登录」开关（settings 表，值 "1" / "0"；没有这个键表示关闭）。
const loginRequiredKey = "login_required"

// LoginRequired 读「需要登录」开关。升级前已设密码的部署由迁移 0007 写成打开。
func (s *Store) LoginRequired(ctx context.Context) (bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, loginRequiredKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return v == "1", err
}

func (s *Store) SetLoginRequired(ctx context.Context, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, loginRequiredKey, v)
	return err
}

// 验证码的数量和查询上限（spec 0004）。
const (
	MaxActiveLoginCodes = 5
	MaxLoginCodePolls   = 120
	// LoginCodeGrace 是验证码确认后、过期之后仍允许浏览器换会话的宽限：在最后几秒发的验证码也能用上。
	LoginCodeGrace = time.Minute
)

// LoginCodeState 是浏览器查询验证码的结果。
type LoginCodeState int

const (
	LoginCodeGone      LoginCodeState = iota // 不存在、已过期、已用过或查询次数用完
	LoginCodePending                         // 还没在微信里确认
	LoginCodeConfirmed                       // 已确认；这次查询同时把它作废，调用方据此建立会话
)

// cleanLoginCodes 删除过期的验证码（已确认的多留 LoginCodeGrace）。
func cleanLoginCodes(ctx context.Context, q querier, now time.Time) error {
	_, err := q.ExecContext(ctx, `DELETE FROM login_codes WHERE (confirmed_at IS NULL AND expires_at <= ?) OR expires_at <= ?`,
		now.Unix(), now.Add(-LoginCodeGrace).Unix())
	return err
}

// ErrTooManyLoginCodes 表示有效的验证码已达上限：拒绝生成新码，而不是挤掉别人还在用的码。
var ErrTooManyLoginCodes = errors.New("store: 有效的登录验证码太多")

// LoginClient 是发起登录的浏览器：来源地址和「浏览器 / 系统」摘要。
type LoginClient struct {
	IP, UA string
}

// liveCond 是「还能用」的验证码：未确认且未过期，或已确认且在宽限内。参数：now, now-grace。
const liveCond = `((confirmed_at IS NULL AND expires_at > ?) OR (confirmed_at IS NOT NULL AND expires_at > ?))`

// LoginCodeActive 报告是否有一个有效、未确认的验证码用这个哈希（生成时避免重复）。
func (s *Store) LoginCodeActive(ctx context.Context, codeHash string, now time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM login_codes WHERE code_hash = ? AND confirmed_at IS NULL AND expires_at > ?`, codeHash, now.Unix()).Scan(&n)
	return n > 0, err
}

// CreateLoginCode 保存一个验证码。同一浏览器原来的验证码先作废；还能用的验证码已有 MaxActiveLoginCodes 个时
// 返回 ErrTooManyLoginCodes，不挤掉别人的码（包括已确认、还没换会话的）。
func (s *Store) CreateLoginCode(ctx context.Context, codeHash, browserHash string, client LoginClient, now, expires time.Time) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if err := cleanLoginCodes(ctx, tx, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM login_codes WHERE browser_hash = ?`, browserHash); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM login_codes`).Scan(&n); err != nil {
			return err
		}
		if n >= MaxActiveLoginCodes {
			return ErrTooManyLoginCodes
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO login_codes (code_hash, browser_hash, created_at, expires_at, client_ip, client_ua) VALUES (?, ?, ?, ?, ?, ?)`,
			codeHash, browserHash, now.Unix(), expires.Unix(), client.IP, client.UA)
		return err
	})
}

// ConfirmLoginCode 在收到主人的微信消息时调用：有一个有效、未确认的验证码匹配就标记为已确认，返回 true 和发起方。
// 多个浏览器碰巧拿到同一个数字时（生成时已尽量避免）只确认最新的那个。
func (s *Store) ConfirmLoginCode(ctx context.Context, codeHash string, now time.Time) (LoginClient, bool, error) {
	var c LoginClient
	ok := false
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id, client_ip, client_ua FROM login_codes WHERE code_hash = ? AND confirmed_at IS NULL AND expires_at > ? ORDER BY id DESC LIMIT 1`,
			codeHash, now.Unix()).Scan(&id, &c.IP, &c.UA)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		ok = true
		_, err = tx.ExecContext(ctx, `UPDATE login_codes SET confirmed_at = ? WHERE id = ?`, now.Unix(), id)
		return err
	})
	return c, ok, err
}

// PollLoginCode 是浏览器用自己的随机值查询结果：每查一次计数，超过 MaxLoginCodePolls 次作废；
// 已确认时在同一事务里删除（单次使用），返回 LoginCodeConfirmed。
func (s *Store) PollLoginCode(ctx context.Context, browserHash string, now time.Time) (LoginCodeState, error) {
	state := LoginCodeGone
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := cleanLoginCodes(ctx, tx, now); err != nil {
			return err
		}
		var id int64
		var polls int
		var confirmed sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT id, polls, confirmed_at FROM login_codes WHERE browser_hash = ?`, browserHash).Scan(&id, &polls, &confirmed)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if confirmed.Valid {
			state = LoginCodeConfirmed
			_, err := tx.ExecContext(ctx, `DELETE FROM login_codes WHERE id = ?`, id)
			return err
		}
		if polls+1 > MaxLoginCodePolls {
			_, err := tx.ExecContext(ctx, `DELETE FROM login_codes WHERE id = ?`, id)
			return err
		}
		state = LoginCodePending
		_, err = tx.ExecContext(ctx, `UPDATE login_codes SET polls = polls + 1 WHERE id = ?`, id)
		return err
	})
	return state, err
}

// LoginCodeMatches 报告这个浏览器的验证码是否就是 codeHash 且还能用（刷新登录页时复用、「我已发送」回显前核对）。
func (s *Store) LoginCodeMatches(ctx context.Context, browserHash, codeHash string, now time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM login_codes WHERE browser_hash = ? AND code_hash = ? AND `+liveCond,
		browserHash, codeHash, now.Unix(), now.Add(-LoginCodeGrace).Unix()).Scan(&n)
	return n > 0, err
}

// DeleteAllLoginCodes 作废所有验证码（微信里「退出网页登录」时，连同已确认还没换会话的一起作废）。
func (s *Store) DeleteAllLoginCodes(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM login_codes`)
	return err
}

// DeleteLoginCode 作废这个浏览器的验证码。
func (s *Store) DeleteLoginCode(ctx context.Context, browserHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM login_codes WHERE browser_hash = ?`, browserHash)
	return err
}

// CountLoginCodes 是表里的验证码数（含已过期未清理的），测试用。
func (s *Store) CountLoginCodes(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM login_codes`).Scan(&n)
	return n, err
}
