// Package store is the control node's state: a single SQLite database.
//
// Global settings (config vars, domains, properties) are stored with
// app_id = 0, and every method that takes an app name treats "" as global.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

// NotFoundError and ExistsError carry a human message while still matching
// ErrNotFound / ErrExists with errors.Is.
type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string   { return e.What + " does not exist" }
func (e *NotFoundError) Is(t error) bool { return t == ErrNotFound }

type ExistsError struct{ What string }

func (e *ExistsError) Error() string   { return e.What + " already exists" }
func (e *ExistsError) Is(t error) bool { return t == ErrExists }

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) the database at path and applies pending
// migrations. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		dsn = "file::memory:?_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// The control plane's write volume is tiny; a single connection removes
	// every SQLITE_BUSY case and keeps :memory: databases coherent.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	for i := current; i < len(migrations); i++ {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1))
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
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

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// appID resolves an app name to its id; "" is the global scope (0).
func appID(ctx context.Context, q querier, app string) (int64, error) {
	if app == "" {
		return 0, nil
	}
	var id int64
	err := q.QueryRowContext(ctx, "SELECT id FROM apps WHERE name = ?", app).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, &NotFoundError{What: "App " + app}
	}
	return id, err
}

func unix(t time.Time) int64 { return t.Unix() }

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}
