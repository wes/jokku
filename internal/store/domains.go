package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/wes/jokku/internal/types"
)

// Domains returns an app's domains, or the global domains for app "".
func (s *Store) Domains(ctx context.Context, app string) ([]string, error) {
	id, err := appID(ctx, s.db, app)
	if err != nil {
		return nil, err
	}
	return domains(ctx, s.db, id)
}

func domains(ctx context.Context, q querier, id int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT domain FROM domains WHERE app_id = ? ORDER BY domain", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) UpdateDomains(ctx context.Context, app string, p types.DomainsPatch) ([]string, error) {
	var out []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		id, err := appID(ctx, tx, app)
		if err != nil {
			return err
		}
		if p.Clear || p.Set != nil {
			if _, err := tx.ExecContext(ctx, "DELETE FROM domains WHERE app_id = ?", id); err != nil {
				return err
			}
		}
		for _, d := range p.Remove {
			if _, err := tx.ExecContext(ctx, "DELETE FROM domains WHERE app_id = ? AND domain = ?", id, d); err != nil {
				return err
			}
		}
		for _, d := range append(p.Add, p.Set...) {
			_, err := tx.ExecContext(ctx, "INSERT INTO domains (app_id, domain) VALUES (?, ?) ON CONFLICT (app_id, domain) DO NOTHING", id, d)
			if isUniqueViolation(err) {
				owner, _ := domainOwner(ctx, tx, d)
				return &ExistsError{What: fmt.Sprintf("Domain %s (on app %s)", d, owner)}
			}
			if err != nil {
				return err
			}
		}
		out, err = domains(ctx, tx, id)
		return err
	})
	return out, err
}

func domainOwner(ctx context.Context, q querier, domain string) (string, error) {
	var name string
	err := q.QueryRowContext(ctx, "SELECT a.name FROM domains d JOIN apps a ON a.id = d.app_id WHERE d.domain = ?", domain).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}
