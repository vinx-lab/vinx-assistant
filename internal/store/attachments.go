package store

import (
	"context"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

const attCols = `id, item_id, kind, rel_path, file_name, size, md5, state, attempts, last_error, media_json, created_at`

func (s *Store) InsertAttachment(ctx context.Context, a *model.Attachment) (int64, error) {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = s.now()
	}
	if a.State == "" {
		a.State = "pending"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO attachments (item_id, kind, rel_path, file_name, size, md5, state, attempts, last_error, media_json, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`, a.ItemID, a.Kind, a.RelPath, a.FileName, a.Size, a.MD5, a.State, a.Attempts, a.LastError, a.MediaJSON, a.CreatedAt.Unix())
	if err != nil {
		return 0, err
	}
	a.ID, err = res.LastInsertId()
	return a.ID, err
}

func (s *Store) UpdateAttachment(ctx context.Context, a *model.Attachment) error {
	res, err := s.db.ExecContext(ctx, `UPDATE attachments SET rel_path=?, file_name=?, size=?, md5=?, state=?, attempts=?, last_error=? WHERE id=?`,
		a.RelPath, a.FileName, a.Size, a.MD5, a.State, a.Attempts, a.LastError, a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListAttachments(ctx context.Context, itemID int64) ([]model.Attachment, error) {
	return s.queryAttachments(ctx, `SELECT `+attCols+` FROM attachments WHERE item_id = ? ORDER BY id`, itemID)
}

// RetryableAttachments 返回下载失败、还没超过重试次数的附件。
func (s *Store) RetryableAttachments(ctx context.Context, maxAttempts int) ([]model.Attachment, error) {
	return s.queryAttachments(ctx, `SELECT `+attCols+` FROM attachments WHERE state = 'pending' AND attempts < ? ORDER BY id`, maxAttempts)
}

func (s *Store) queryAttachments(ctx context.Context, q string, args ...any) ([]model.Attachment, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Attachment
	for rows.Next() {
		var a model.Attachment
		var created int64
		if err := rows.Scan(&a.ID, &a.ItemID, &a.Kind, &a.RelPath, &a.FileName, &a.Size, &a.MD5, &a.State, &a.Attempts, &a.LastError, &a.MediaJSON, &created); err != nil {
			return nil, err
		}
		a.CreatedAt = fromUnix(created)
		out = append(out, a)
	}
	return out, rows.Err()
}
