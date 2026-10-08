package store

import (
	"context"
	"time"
)

// RegistryLogin is a credential for pulling images from a private registry,
// used by every build (git:from-image and Dockerfile FROM lines alike).
type RegistryLogin struct {
	Server    string
	Username  string
	Password  string
	CreatedAt time.Time
}

// SetRegistryLogin stores (or replaces) the login for a registry server.
func (s *Store) SetRegistryLogin(ctx context.Context, l RegistryLogin) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO registry_logins (server, username, password, created_at) VALUES (?, ?, ?, ?)
ON CONFLICT (server) DO UPDATE SET username = excluded.username, password = excluded.password`,
		l.Server, l.Username, l.Password, unix(s.now()))
	return err
}

func (s *Store) DeleteRegistryLogin(ctx context.Context, server string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM registry_logins WHERE server = ?", server)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "A login for " + server}
	}
	return nil
}

func (s *Store) RegistryLogins(ctx context.Context) ([]RegistryLogin, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT server, username, password, created_at FROM registry_logins ORDER BY server")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegistryLogin
	for rows.Next() {
		var l RegistryLogin
		var created int64
		if err := rows.Scan(&l.Server, &l.Username, &l.Password, &created); err != nil {
			return nil, err
		}
		l.CreatedAt = fromUnix(created)
		out = append(out, l)
	}
	return out, rows.Err()
}
