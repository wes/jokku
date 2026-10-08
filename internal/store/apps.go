package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/wes/jokku/internal/types"
)

func (s *Store) CreateApp(ctx context.Context, name string) (*types.App, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "INSERT INTO apps (name, created_at) VALUES (?, ?)", name, unix(s.now()))
		if isUniqueViolation(err) {
			return &ExistsError{What: "App " + name}
		}
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		return newAppDefaults(ctx, tx, id)
	})
	if err != nil {
		return nil, err
	}
	return s.App(ctx, name)
}

// newAppDefaults records the settings a new app starts with where they
// differ from the plugin defaults, which apps created before keep. Let's
// Encrypt starts off, as in Dokku (letsencrypt:enable turns it on), unless
// it was turned on with --global or the app already has its own setting.
func newAppDefaults(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO properties (app_id, plugin, key, value)
SELECT ?, 'letsencrypt', 'enabled', 'false'
WHERE NOT EXISTS (SELECT 1 FROM properties WHERE app_id IN (0, ?) AND plugin = 'letsencrypt' AND key = 'enabled')`, id, id)
	return err
}

func (s *Store) App(ctx context.Context, name string) (*types.App, error) {
	row := s.db.QueryRowContext(ctx, appSelect+" WHERE a.name = ?", name)
	app, err := scanApp(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &NotFoundError{What: "App " + name}
	}
	return app, err
}

func (s *Store) Apps(ctx context.Context) ([]types.App, error) {
	rows, err := s.db.QueryContext(ctx, appSelect+" ORDER BY a.name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var apps []types.App
	for rows.Next() {
		app, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, *app)
	}
	return apps, rows.Err()
}

const appSelect = `
SELECT a.name, a.locked, a.stopped, a.created_at, COALESCE(r.version, 0),
	COALESCE((SELECT source FROM deploys d WHERE d.app_id = a.id AND d.status = 'succeeded' ORDER BY d.id DESC LIMIT 1), '')
FROM apps a LEFT JOIN releases r ON r.id = a.current_release_id`

type scanner interface{ Scan(...any) error }

func scanApp(row scanner) (*types.App, error) {
	var app types.App
	var created int64
	if err := row.Scan(&app.Name, &app.Locked, &app.Stopped, &created, &app.CurrentRelease, &app.DeploySource); err != nil {
		return nil, err
	}
	app.CreatedAt = fromUnix(created)
	return &app, nil
}

// DeleteApp removes an app and everything scoped to it.
func (s *Store) DeleteApp(ctx context.Context, name string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		id, err := appID(ctx, tx, name)
		if err != nil {
			return err
		}
		for _, table := range []string{"config_vars", "domains", "properties"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE app_id = ?", id); err != nil {
				return err
			}
		}
		// formations, resources, releases and deploys cascade.
		_, err = tx.ExecContext(ctx, "DELETE FROM apps WHERE id = ?", id)
		return err
	})
}

func (s *Store) RenameApp(ctx context.Context, name, newName string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE apps SET name = ? WHERE name = ?", newName, name)
	if isUniqueViolation(err) {
		return &ExistsError{What: "App " + newName}
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "App " + name}
	}
	return nil
}

// CloneApp creates newName with a copy of name's config vars, properties,
// formation and resources. Domains are not copied: they must be unique.
func (s *Store) CloneApp(ctx context.Context, name, newName string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		src, err := appID(ctx, tx, name)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO apps (name, created_at) VALUES (?, ?)", newName, unix(s.now()))
		if isUniqueViolation(err) {
			return &ExistsError{What: "App " + newName}
		}
		if err != nil {
			return err
		}
		dst, err := res.LastInsertId()
		if err != nil {
			return err
		}
		copies := []string{
			"INSERT INTO config_vars (app_id, key, value) SELECT ?, key, value FROM config_vars WHERE app_id = ?",
			"INSERT INTO properties (app_id, plugin, key, value) SELECT ?, plugin, key, value FROM properties WHERE app_id = ?",
			"INSERT INTO formations (app_id, process_type, quantity) SELECT ?, process_type, quantity FROM formations WHERE app_id = ?",
			"INSERT INTO resources (app_id, process_type, cpus, memory_mb) SELECT ?, process_type, cpus, memory_mb FROM resources WHERE app_id = ?",
		}
		for _, q := range copies {
			if _, err := tx.ExecContext(ctx, q, dst, src); err != nil {
				return err
			}
		}
		return newAppDefaults(ctx, tx, dst)
	})
}

func (s *Store) SetAppLocked(ctx context.Context, name string, locked bool) error {
	res, err := s.db.ExecContext(ctx, "UPDATE apps SET locked = ? WHERE name = ?", locked, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "App " + name}
	}
	return nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
