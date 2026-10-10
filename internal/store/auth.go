package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AuthUser is someone who can log in to apps with a login in front of them
// (http-auth). Passwords are stored as bcrypt hashes.
type AuthUser struct {
	Name         string
	PasswordHash string
	TOTPSecret   string // base32; empty without a second factor
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (s *Store) AuthUsers(ctx context.Context) ([]AuthUser, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, password_hash, totp_secret, created_at, updated_at FROM auth_users ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuthUser
	for rows.Next() {
		var u AuthUser
		var created, updated int64
		if err := rows.Scan(&u.Name, &u.PasswordHash, &u.TOTPSecret, &created, &updated); err != nil {
			return nil, err
		}
		u.CreatedAt, u.UpdatedAt = fromUnix(created), fromUnix(updated)
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) AuthUser(ctx context.Context, name string) (*AuthUser, error) {
	var u AuthUser
	var created, updated int64
	err := s.db.QueryRowContext(ctx, "SELECT name, password_hash, totp_secret, created_at, updated_at FROM auth_users WHERE name = ?", name).
		Scan(&u.Name, &u.PasswordHash, &u.TOTPSecret, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &NotFoundError{What: "User " + name}
	}
	u.CreatedAt, u.UpdatedAt = fromUnix(created), fromUnix(updated)
	return &u, err
}

// CreateAuthUser adds a user; it fails if the name is taken.
func (s *Store) CreateAuthUser(ctx context.Context, name, passwordHash string) error {
	now := unix(s.now())
	_, err := s.db.ExecContext(ctx, "INSERT INTO auth_users (name, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)",
		name, passwordHash, now, now)
	if isUniqueViolation(err) {
		return &ExistsError{What: "User " + name}
	}
	return err
}

// SetAuthUserPassword changes a user's password, which ends their sessions.
func (s *Store) SetAuthUserPassword(ctx context.Context, name, passwordHash string) error {
	return s.updateAuthUser(ctx, name, "password_hash", passwordHash)
}

// SetAuthUserTOTP sets (or, with "", removes) a user's second factor.
func (s *Store) SetAuthUserTOTP(ctx context.Context, name, secret string) error {
	return s.updateAuthUser(ctx, name, "totp_secret", secret)
}

func (s *Store) updateAuthUser(ctx context.Context, name, column, value string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE auth_users SET "+column+" = ?, updated_at = ? WHERE name = ?", value, unix(s.now()), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "User " + name}
	}
	return nil
}

func (s *Store) DeleteAuthUser(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM auth_users WHERE name = ?", name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "User " + name}
	}
	return nil
}

// AuthShare is a link into one app for anyone holding it, until it expires.
// Only the token's hash is kept.
type AuthShare struct {
	ID        string
	App       string
	TokenHash string
	Note      string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// CreateAuthShare adds a link, and forgets those that expired.
func (s *Store) CreateAuthShare(ctx context.Context, sh *AuthShare) error {
	id, err := appID(ctx, s.db, sh.App)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM auth_shares WHERE expires_at <= ?", unix(s.now())); err != nil {
		return err
	}
	sh.CreatedAt = s.now().UTC().Truncate(time.Second)
	_, err = s.db.ExecContext(ctx, "INSERT INTO auth_shares (id, app_id, token_hash, note, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		sh.ID, id, sh.TokenHash, sh.Note, unix(sh.ExpiresAt), unix(sh.CreatedAt))
	if isUniqueViolation(err) {
		return &ExistsError{What: "Share " + sh.ID}
	}
	return err
}

// AuthShares lists the links into app ("" for every app's) that haven't
// expired.
func (s *Store) AuthShares(ctx context.Context, app string) ([]AuthShare, error) {
	q := "SELECT s.id, a.name, s.token_hash, s.note, s.expires_at, s.created_at FROM auth_shares s JOIN apps a ON a.id = s.app_id WHERE s.expires_at > ?"
	args := []any{unix(s.now())}
	if app != "" {
		q += " AND a.name = ?"
		args = append(args, app)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY s.created_at, s.id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuthShare
	for rows.Next() {
		var sh AuthShare
		var expires, created int64
		if err := rows.Scan(&sh.ID, &sh.App, &sh.TokenHash, &sh.Note, &expires, &created); err != nil {
			return nil, err
		}
		sh.ExpiresAt, sh.CreatedAt = fromUnix(expires), fromUnix(created)
		out = append(out, sh)
	}
	return out, rows.Err()
}

// DeleteAuthShare revokes one of app's links.
func (s *Store) DeleteAuthShare(ctx context.Context, app, id string) error {
	appID, err := appID(ctx, s.db, app)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM auth_shares WHERE app_id = ? AND id = ?", appID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "Share " + id + " of " + app}
	}
	return nil
}
