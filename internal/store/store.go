// Package store 是 SQLite 的唯一入口。其他包只通过这里读写数据。
//
// 只开一个连接（单用户，串行写最简单）。因此在 Tx 回调里只能用传进来的 tx，
// 不能再调用 s.db 上的方法，否则会死锁。
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var (
	ErrNotFound  = errors.New("store: 记录不存在")
	ErrDuplicate = errors.New("store: 消息已存在")
)

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type Store struct {
	db  *sql.DB
	clk clock.Clock
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: 打开 %s：%w", path, err)
	}
	s := &Store{db: db, clk: clock.Real{}}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error           { return s.db.Close() }
func (s *Store) SetClock(c clock.Clock) { s.clk = c }
func (s *Store) now() time.Time         { return s.clk.Now() }

func (s *Store) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version = ?`, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		err = s.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, name, time.Now().Unix())
			return err
		})
		if err != nil {
			return fmt.Errorf("store: 迁移 %s：%w", name, err)
		}
	}
	return nil
}

func (s *Store) GetKV(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetKV(ctx context.Context, key, val string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, val)
	return err
}

// SetKVs 在一个事务里写入 set 中的键并删除 del 中的键，要么全成功要么全不变。
func (s *Store) SetKVs(ctx context.Context, set map[string]string, del []string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		for k, v := range set {
			if _, err := tx.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return err
			}
		}
		for _, k := range del {
			if _, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, k); err != nil {
				return err
			}
		}
		return nil
	})
}

// Seen 表示这条微信消息已经处理过（条目或指令）。
func (s *Store) Seen(ctx context.Context, msgID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM seen_msgs WHERE msg_id = ?`, msgID).Scan(&n)
	return n > 0, err
}

// MarkSeen 在消息处理完之后调用；重复调用无副作用。
func (s *Store) MarkSeen(ctx context.Context, msgID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO seen_msgs (msg_id, seen_at) VALUES (?, ?) ON CONFLICT(msg_id) DO NOTHING`, msgID, at.Unix())
	return err
}

// Snapshot 在服务运行时导出一致的数据库副本。dst 必须不存在。
func (s *Store) Snapshot(ctx context.Context, dst string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dst)
	return err
}

func fromUnix(v int64) time.Time { return time.Unix(v, 0).In(clock.Zone) }

func unixOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}
