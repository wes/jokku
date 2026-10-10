package store

import (
	"context"
	"database/sql"
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

// SetProperties stores several of a plugin's values at once; empty values
// delete their keys.
func (s *Store) SetProperties(ctx context.Context, app, plugin string, values map[string]string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		id, err := appID(ctx, tx, app)
		if err != nil {
			return err
		}
		for key, value := range values {
			if value == "" {
				_, err = tx.ExecContext(ctx, "DELETE FROM properties WHERE app_id = ? AND plugin = ? AND key = ?", id, plugin, key)
			} else {
				_, err = tx.ExecContext(ctx,
					"INSERT INTO properties (app_id, plugin, key, value) VALUES (?, ?, ?, ?) ON CONFLICT (app_id, plugin, key) DO UPDATE SET value = excluded.value",
					id, plugin, key, value)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// EnsureProperty stores value unless the key already has one, and returns
// the value it has now: concurrent callers all get the first one stored.
func (s *Store) EnsureProperty(ctx context.Context, app, plugin, key, value string) (string, error) {
	id, err := appID(ctx, s.db, app)
	if err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO properties (app_id, plugin, key, value) VALUES (?, ?, ?, ?) ON CONFLICT (app_id, plugin, key) DO NOTHING",
		id, plugin, key, value); err != nil {
		return "", err
	}
	var v string
	err = s.db.QueryRowContext(ctx, "SELECT value FROM properties WHERE app_id = ? AND plugin = ? AND key = ?", id, plugin, key).Scan(&v)
	return v, err
}
