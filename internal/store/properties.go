package store

import (
	"context"
)

// Properties returns the values set for one plugin, for an app or globally
// (app ""). Unset keys are absent.
func (s *Store) Properties(ctx context.Context, app, plugin string) (map[string]string, error) {
	id, err := appID(ctx, s.db, app)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM properties WHERE app_id = ? AND plugin = ?", id, plugin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetProperty stores a value; an empty value deletes the key.
func (s *Store) SetProperty(ctx context.Context, app, plugin, key, value string) error {
	id, err := appID(ctx, s.db, app)
	if err != nil {
		return err
	}
	if value == "" {
		_, err = s.db.ExecContext(ctx, "DELETE FROM properties WHERE app_id = ? AND plugin = ? AND key = ?", id, plugin, key)
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO properties (app_id, plugin, key, value) VALUES (?, ?, ?, ?) ON CONFLICT (app_id, plugin, key) DO UPDATE SET value = excluded.value",
		id, plugin, key, value)
	return err
}
