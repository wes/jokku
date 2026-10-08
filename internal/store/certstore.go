package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// The certificate store backs every ingress node's Caddy, so certificates,
// ACME accounts and HTTP-01 challenge tokens are shared cluster-wide: any
// node can answer a challenge for a certificate another node requested.

func (s *Store) CertPut(ctx context.Context, key string, value []byte) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO certstore (key, value, modified) VALUES (?, ?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value, modified = excluded.modified",
		key, value, unix(s.now()))
	return err
}

// CertGet returns ErrNotFound for a missing key.
func (s *Store) CertGet(ctx context.Context, key string) ([]byte, time.Time, error) {
	var value []byte
	var modified int64
	err := s.db.QueryRowContext(ctx, "SELECT value, modified FROM certstore WHERE key = ?", key).Scan(&value, &modified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, ErrNotFound
	}
	return value, fromUnix(modified), err
}

// CertDelete removes a key and everything under it (key + "/").
func (s *Store) CertDelete(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM certstore WHERE key = ? OR key LIKE ? ESCAPE '\\'", key, likePrefix(key+"/"))
	return err
}

// CertList lists keys under prefix: all of them when recursive, otherwise
// the direct children (files and directories).
func (s *Store) CertList(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	p := strings.TrimSuffix(prefix, "/")
	if p != "" {
		p += "/"
	}
	rows, err := s.db.QueryContext(ctx, "SELECT key FROM certstore WHERE key LIKE ? ESCAPE '\\' ORDER BY key", likePrefix(p))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		if !recursive {
			rest := strings.TrimPrefix(k, p)
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				k = p + rest[:i]
			}
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out, rows.Err()
}

// CertLock takes a named lock for owner, or reports false while someone else
// holds an unexpired one.
func (s *Store) CertLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx, `
INSERT INTO certlocks (key, owner, expires_at) VALUES (?, ?, ?)
ON CONFLICT (key) DO UPDATE SET owner = excluded.owner, expires_at = excluded.expires_at
WHERE certlocks.expires_at < ? OR certlocks.owner = excluded.owner`,
		key, owner, unix(now.Add(ttl)), unix(now))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) CertUnlock(ctx context.Context, key, owner string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM certlocks WHERE key = ? AND owner = ?", key, owner)
	return err
}

func likePrefix(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(p) + "%"
}
