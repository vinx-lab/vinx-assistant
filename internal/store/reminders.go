package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

type Reminder struct {
	ID          int64
	Kind        string // digest due
	ItemID      int64  // 0 表示没有关联条目（摘要）
	ScheduledAt time.Time
	State       string // pending sent deferred dropped
	Attempts    int
	LastError   string
	Body        string
	CreatedAt   time.Time
	SentAt      *time.Time
}

const remCols = `id, kind, COALESCE(item_id, 0), scheduled_at, state, attempts, last_error, body, created_at, sent_at`

// InsertReminder 新建提醒。同一条目同一截止时刻、或同一摘要时刻已经有提醒时返回 ErrDuplicate。
func (s *Store) InsertReminder(ctx context.Context, r *Reminder) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = s.now()
	}
	if r.State == "" {
		r.State = "pending"
	}
	var item any
	if r.ItemID != 0 {
		item = r.ItemID
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO reminders (kind, item_id, scheduled_at, state, attempts, last_error, body, created_at)
		VALUES (?,?,?,?,?,?,?,?)`, r.Kind, item, r.ScheduledAt.Unix(), r.State, r.Attempts, r.LastError, r.Body, r.CreatedAt.Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDuplicate
	}
	r.ID, err = res.LastInsertId()
	return err
}

func (s *Store) HasDigest(ctx context.Context, at time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM reminders WHERE kind = 'digest' AND scheduled_at = ?`, at.Unix()).Scan(&n)
	return n > 0, err
}

// MarkReminders 批量改状态。sent、deferred 记一次尝试；sent 记发送时间。
func (s *Store) MarkReminders(ctx context.Context, ids []int64, state, lastErr string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	var sentAt any
	if state == "sent" {
		sentAt = at.Unix()
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			_, err := tx.ExecContext(ctx, `UPDATE reminders SET state = ?, last_error = ?,
				attempts = attempts + CASE WHEN ? IN ('sent','deferred') THEN 1 ELSE 0 END,
				sent_at = COALESCE(?, sent_at) WHERE id = ?`, state, lastErr, state, sentAt, id)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) RemindersByState(ctx context.Context, state string) ([]Reminder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+remCols+` FROM reminders WHERE state = ? ORDER BY scheduled_at, id`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reminder
	for rows.Next() {
		var r Reminder
		var sched, created int64
		var sent sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Kind, &r.ItemID, &sched, &r.State, &r.Attempts, &r.LastError, &r.Body, &created, &sent); err != nil {
			return nil, err
		}
		r.ScheduledAt, r.CreatedAt = fromUnix(sched), fromUnix(created)
		if sent.Valid {
			t := fromUnix(sent.Int64)
			r.SentAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DueCandidates 返回截止时刻早于 until、还没为这个截止时刻建过提醒的未完成待办（只看带具体时刻的）。
func (s *Store) DueCandidates(ctx context.Context, until time.Time) ([]model.Item, error) {
	return s.queryItems(ctx, `SELECT `+itemCols+` FROM items i
		WHERE category = 'todo' AND status = 'open' AND due_has_time = 1 AND due_at < ?
		  AND NOT EXISTS (SELECT 1 FROM reminders r WHERE r.kind = 'due' AND r.item_id = i.id AND r.scheduled_at = i.due_at)
		ORDER BY due_at, id`, until.Unix())
}
