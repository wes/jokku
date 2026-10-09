package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// DatabaseLink is a database linked to an app: Var on the app holds its URL.
type DatabaseLink struct {
	Database  string // the database's app
	App       string
	Var       string
	CreatedAt time.Time
}

// LinkDatabase records that app reaches database through Var.
func (s *Store) LinkDatabase(ctx context.Context, database, app, varName string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		dbID, err := appID(ctx, tx, database)
		if err != nil {
			return err
		}
		appID, err := appID(ctx, tx, app)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO database_links (database_id, app_id, var, created_at) VALUES (?, ?, ?, ?)",
			dbID, appID, varName, unix(s.now()))
		if err != nil && (isUniqueViolation(err) || errors.Is(err, sql.ErrNoRows)) {
			return &ExistsError{What: "A link from " + app}
		}
		return err
	})
}

// UnlinkDatabase forgets a link, returning the var it set.
func (s *Store) UnlinkDatabase(ctx context.Context, database, app string) (string, error) {
	links, err := s.DatabaseLinks(ctx, database)
	if err != nil {
		return "", err
	}
	for _, l := range links {
		if l.App != app {
			continue
		}
		_, err := s.db.ExecContext(ctx, `
DELETE FROM database_links WHERE database_id = (SELECT id FROM apps WHERE name = ?) AND app_id = (SELECT id FROM apps WHERE name = ?)`,
			database, app)
		return l.Var, err
	}
	return "", &NotFoundError{What: "A link to " + app}
}

// DatabaseLinks lists a database's links, or every link for database "".
func (s *Store) DatabaseLinks(ctx context.Context, database string) ([]DatabaseLink, error) {
	q, args := `
SELECT d.name, a.name, l.var, l.created_at FROM database_links l
JOIN apps d ON d.id = l.database_id JOIN apps a ON a.id = l.app_id`, []any{}
	if database != "" {
		q, args = q+" WHERE d.name = ?", append(args, database)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY d.name, a.name", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DatabaseLink
	for rows.Next() {
		var l DatabaseLink
		var created int64
		if err := rows.Scan(&l.Database, &l.App, &l.Var, &created); err != nil {
			return nil, err
		}
		l.CreatedAt = fromUnix(created)
		out = append(out, l)
	}
	return out, rows.Err()
}
