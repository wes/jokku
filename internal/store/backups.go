package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wes/jokku/internal/types"
)

// CreateBackupDestination stores a bucket to back up to.
func (s *Store) CreateBackupDestination(ctx context.Context, d types.BackupDestination) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO backup_destinations (name, endpoint, region, bucket, access_key_id, secret_access_key, encrypt, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Name, d.Endpoint, d.Region, d.Bucket, d.AccessKeyID, d.SecretAccessKey, d.Encrypt, unix(s.now()))
	if isUniqueViolation(err) {
		return &ExistsError{What: "Backup destination " + d.Name}
	}
	return err
}

const destinationSelect = "SELECT name, endpoint, region, bucket, access_key_id, secret_access_key, encrypt, created_at FROM backup_destinations"

// BackupDestinations lists the destinations, secrets included.
func (s *Store) BackupDestinations(ctx context.Context) ([]types.BackupDestination, error) {
	rows, err := s.db.QueryContext(ctx, destinationSelect+" ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.BackupDestination{}
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

func (s *Store) BackupDestination(ctx context.Context, name string) (*types.BackupDestination, error) {
	d, err := scanDestination(s.db.QueryRowContext(ctx, destinationSelect+" WHERE name = ?", name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &NotFoundError{What: "Backup destination " + name}
	}
	return d, err
}

func scanDestination(row interface{ Scan(...any) error }) (*types.BackupDestination, error) {
	var d types.BackupDestination
	var created int64
	if err := row.Scan(&d.Name, &d.Endpoint, &d.Region, &d.Bucket, &d.AccessKeyID, &d.SecretAccessKey, &d.Encrypt, &created); err != nil {
		return nil, err
	}
	d.CreatedAt = fromUnix(created)
	return &d, nil
}

func (s *Store) DeleteBackupDestination(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM backup_destinations WHERE name = ?", name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "Backup destination " + name}
	}
	return nil
}

// BackupKey returns the key that encrypts backups, and whether the user said
// they saved it. ErrNotFound until one is made.
func (s *Store) BackupKey(ctx context.Context) (string, bool, error) {
	var key string
	var saved bool
	err := s.db.QueryRowContext(ctx, "SELECT key, saved FROM backup_key WHERE id = 1").Scan(&key, &saved)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, &NotFoundError{What: "The backup key"}
	}
	return key, saved, err
}

// CreateBackupKey stores key unless there is a key already; either way it
// returns the cluster's key.
func (s *Store) CreateBackupKey(ctx context.Context, key string) (string, bool, error) {
	if _, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO backup_key (id, key, created_at) VALUES (1, ?, ?)", key, unix(s.now())); err != nil {
		return "", false, err
	}
	return s.BackupKey(ctx)
}

// SetBackupKeySaved records that the user saved the key somewhere safe.
func (s *Store) SetBackupKeySaved(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "UPDATE backup_key SET saved = 1 WHERE id = 1")
	return err
}

// VolumeBackup is where a volume is backed up: a path in a destination.
type VolumeBackup struct {
	VolumeID    string
	App         string
	Volume      string
	Destination string
	Path        string
}

// SetVolumeBackup sets (or changes) where a volume is backed up.
func (s *Store) SetVolumeBackup(ctx context.Context, b VolumeBackup) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO volume_backups (volume_id, destination, path) VALUES (?, ?, ?)
ON CONFLICT (volume_id) DO UPDATE SET destination = excluded.destination, path = excluded.path`,
		b.VolumeID, b.Destination, b.Path)
	if isUniqueViolation(err) {
		return &ExistsError{What: "Backups to " + b.Destination + " " + b.Path + " for another volume"}
	}
	return err
}

func (s *Store) DeleteVolumeBackup(ctx context.Context, volumeID string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM volume_backups WHERE volume_id = ?", volumeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "Backups for this volume"}
	}
	return nil
}

const volumeBackupSelect = `
SELECT b.volume_id, COALESCE(a.name, ''), v.name, b.destination, b.path
FROM volume_backups b JOIN volumes v ON v.id = b.volume_id LEFT JOIN apps a ON a.id = v.app_id`

// VolumeBackups lists where volumes are backed up: an app's, every app's
// for app "", or those going to a destination.
func (s *Store) VolumeBackups(ctx context.Context, app, destination string) ([]VolumeBackup, error) {
	q, args := volumeBackupSelect+" WHERE v.state != ?", []any{VolumeDestroying}
	if app != "" {
		q, args = q+" AND a.name = ?", append(args, app)
	}
	if destination != "" {
		q, args = q+" AND b.destination = ?", append(args, destination)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY a.name, v.name", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VolumeBackup
	for rows.Next() {
		var b VolumeBackup
		if err := rows.Scan(&b.VolumeID, &b.App, &b.Volume, &b.Destination, &b.Path); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// VolumeBackupFor says where one volume is backed up.
func (s *Store) VolumeBackupFor(ctx context.Context, volumeID string) (*VolumeBackup, error) {
	var b VolumeBackup
	err := s.db.QueryRowContext(ctx, volumeBackupSelect+" WHERE b.volume_id = ?", volumeID).Scan(&b.VolumeID, &b.App, &b.Volume, &b.Destination, &b.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &NotFoundError{What: "Backups for this volume"}
	}
	return &b, err
}

// BackupRun is one backup made (or tried) of a volume.
type BackupRun struct {
	ID         int64
	VolumeID   string
	Name       string
	Status     string // running, succeeded or failed
	Error      string
	SizeBytes  int64
	Blocks     int
	NewBlocks  int
	NewBytes   int64
	Actor      string
	StartedAt  time.Time
	FinishedAt time.Time
}

func (s *Store) StartBackupRun(ctx context.Context, volumeID, name, actor string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO backup_runs (volume_id, name, status, actor, started_at) VALUES (?, ?, ?, ?, ?)",
		volumeID, name, types.StatusRunning, actor, unix(s.now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishBackupRun records how a backup went.
func (s *Store) FinishBackupRun(ctx context.Context, r BackupRun) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE backup_runs SET status = ?, error = ?, size_bytes = ?, blocks = ?, new_blocks = ?, new_bytes = ?, finished_at = ?
WHERE id = ?`, r.Status, r.Error, r.SizeBytes, r.Blocks, r.NewBlocks, r.NewBytes, unix(s.now()), r.ID)
	return err
}

// BackupRuns lists a volume's latest backups, newest first.
func (s *Store) BackupRuns(ctx context.Context, volumeID string, limit int) ([]BackupRun, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, volume_id, name, status, error, size_bytes, blocks, new_blocks, new_bytes, actor, started_at, COALESCE(finished_at, 0)
FROM backup_runs WHERE volume_id = ? ORDER BY id DESC LIMIT ?`, volumeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackupRun
	for rows.Next() {
		var r BackupRun
		var started, finished int64
		if err := rows.Scan(&r.ID, &r.VolumeID, &r.Name, &r.Status, &r.Error, &r.SizeBytes, &r.Blocks, &r.NewBlocks, &r.NewBytes,
			&r.Actor, &started, &finished); err != nil {
			return nil, err
		}
		r.StartedAt, r.FinishedAt = fromUnix(started), fromUnix(finished)
		out = append(out, r)
	}
	return out, rows.Err()
}

// StartVolumeRestore begins restoring a volume from a backup (restore, a
// types.RestoreSpec as JSON, without credentials): on its own node when to
// is "", or onto node to.
func (s *Store) StartVolumeRestore(ctx context.Context, id, to, token, restore string) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE volumes SET restore = ?, restore_result = '', moving_to = ?, move_token = ?, transfer = '', copied_mb = 0, move_error = ''
WHERE id = ? AND moving_to = '' AND restore = '' AND state != ?`, restore, to, token, id, VolumeDestroying)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("the volume is moving, being restored already, or being destroyed")
	}
	return nil
}

// SetVolumeRestore updates a restore in progress.
func (s *Store) SetVolumeRestore(ctx context.Context, id, restore string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE volumes SET restore = ? WHERE id = ? AND restore != ''", restore, id)
	return err
}

// EndVolumeRestore finishes a restore on the volume's own node, with result
// "restored" or why it failed.
func (s *Store) EndVolumeRestore(ctx context.Context, id, result string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE volumes SET restore = '', restore_result = ?, move_token = '', transfer = '', copied_mb = 0, move_error = ''
WHERE id = ? AND moving_to = ''`, result, id)
	return err
}
