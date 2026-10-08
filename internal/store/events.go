package store

import (
	"context"
	"fmt"

	"github.com/wes/jokku/internal/types"
)

// keepEvents bounds the events table.
const keepEvents = 2000

// AddEvent records something that happened. Failing to record an event never
// fails the operation that caused it, so errors are dropped.
func (s *Store) AddEvent(ctx context.Context, kind, app, node, format string, args ...any) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO events (at, kind, app, node, message) VALUES (?, ?, ?, ?, ?)",
		unix(s.now()), kind, app, node, fmt.Sprintf(format, args...))
	if err != nil {
		return
	}
	if id, err := res.LastInsertId(); err == nil && id%100 == 0 {
		s.db.ExecContext(ctx, "DELETE FROM events WHERE id <= ?", id-keepEvents)
	}
}

// Events returns up to limit events, newest first, optionally only one app's.
func (s *Store) Events(ctx context.Context, app string, limit int) ([]types.ClusterEvent, error) {
	q, args := "SELECT id, at, kind, app, node, message FROM events", []any{}
	if app != "" {
		q, args = q+" WHERE app = ?", append(args, app)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY id DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.ClusterEvent{}
	for rows.Next() {
		var e types.ClusterEvent
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Kind, &e.App, &e.Node, &e.Message); err != nil {
			return nil, err
		}
		e.At = fromUnix(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
