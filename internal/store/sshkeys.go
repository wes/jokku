package store

import (
	"context"

	"github.com/wes/jokku/internal/types"
)

func (s *Store) AddSSHKey(ctx context.Context, k types.SSHKey) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO ssh_keys (name, fingerprint, public_key, created_at) VALUES (?, ?, ?, ?)",
		k.Name, k.Fingerprint, k.PublicKey, unix(s.now()))
	if isUniqueViolation(err) {
		var existing string
		if s.db.QueryRowContext(ctx, "SELECT name FROM ssh_keys WHERE fingerprint = ?", k.Fingerprint).Scan(&existing) == nil {
			return &ExistsError{What: "SSH key " + k.Fingerprint + " (named " + existing + ")"}
		}
		return &ExistsError{What: "SSH key " + k.Name}
	}
	return err
}

func (s *Store) SSHKeys(ctx context.Context) ([]types.SSHKey, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, fingerprint, public_key, created_at FROM ssh_keys ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []types.SSHKey{}
	for rows.Next() {
		var k types.SSHKey
		var created int64
		if err := rows.Scan(&k.Name, &k.Fingerprint, &k.PublicKey, &created); err != nil {
			return nil, err
		}
		k.CreatedAt = fromUnix(created)
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// RemoveSSHKey deletes by name, or by fingerprint when byFingerprint is set.
func (s *Store) RemoveSSHKey(ctx context.Context, nameOrFingerprint string, byFingerprint bool) error {
	col := "name"
	if byFingerprint {
		col = "fingerprint"
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM ssh_keys WHERE "+col+" = ?", nameOrFingerprint)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "SSH key " + nameOrFingerprint}
	}
	return nil
}
