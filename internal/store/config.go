package store

import (
	"context"
	"database/sql"

	"github.com/wes/jokku/internal/types"
)

// ConfigVars returns an app's config vars, or the global ones for app "".
func (s *Store) ConfigVars(ctx context.Context, app string) (map[string]string, error) {
	id, err := appID(ctx, s.db, app)
	if err != nil {
		return nil, err
	}
	return configVars(ctx, s.db, id)
}

func configVars(ctx context.Context, q querier, id int64) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT key, value FROM config_vars WHERE app_id = ?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	vars := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		vars[k] = v
	}
	return vars, rows.Err()
}

// UpdateConfigVars applies a patch atomically and reports whether anything
// changed, so callers only restart apps when needed.
func (s *Store) UpdateConfigVars(ctx context.Context, app string, p types.ConfigPatch) (vars map[string]string, changed bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		id, err := appID(ctx, tx, app)
		if err != nil {
			return err
		}
		before, err := configVars(ctx, tx, id)
		if err != nil {
			return err
		}
		if p.Clear {
			if _, err := tx.ExecContext(ctx, "DELETE FROM config_vars WHERE app_id = ?", id); err != nil {
				return err
			}
		}
		for _, k := range p.Unset {
			if _, err := tx.ExecContext(ctx, "DELETE FROM config_vars WHERE app_id = ? AND key = ?", id, k); err != nil {
				return err
			}
		}
		for k, v := range p.Set {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO config_vars (app_id, key, value) VALUES (?, ?, ?) ON CONFLICT (app_id, key) DO UPDATE SET value = excluded.value",
				id, k, v); err != nil {
				return err
			}
		}
		vars, err = configVars(ctx, tx, id)
		if err != nil {
			return err
		}
		changed = !equalMaps(before, vars)
		return nil
	})
	return vars, changed, err
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
