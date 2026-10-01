package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

// LoadSettings 读 rules、schedule、ai 三个键；缺失的部分用默认值。
func (s *Store) LoadSettings(ctx context.Context) (model.Settings, error) {
	st := model.DefaultSettings()
	targets := map[string]any{"rules": &st.Rules, "schedule": &st.Schedule, "ai": &st.AI}
	for key, dst := range targets {
		var raw string
		err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return st, err
		}
		if err := json.Unmarshal([]byte(raw), dst); err != nil {
			return st, err
		}
	}
	return st, nil
}

func (s *Store) SaveSettings(ctx context.Context, st model.Settings) error {
	vals := map[string]any{"rules": st.Rules, "schedule": st.Schedule, "ai": st.AI}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		for key, v := range vals {
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(b)); err != nil {
				return err
			}
		}
		return nil
	})
}
