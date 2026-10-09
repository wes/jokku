package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wes/jokku/internal/types"
)

// The control node's own backups (its database and identity) have one set
// of settings, kept in the shape of a volume's: VolumeID, App and Volume
// are empty.

// DefaultClusterBackupEvery is how often the cluster is backed up by default.
const DefaultClusterBackupEvery = time.Hour

const clusterBackupSelect = `
SELECT '', '', '', destination, path, every_s, keep_recent_s, keep_daily, 0, stored_bytes, stored_backups, collected_at
FROM cluster_backup WHERE id = 1`

// ClusterBackup is where the cluster is backed up; NotFound if nowhere.
func (s *Store) ClusterBackup(ctx context.Context) (*VolumeBackup, error) {
	b, err := scanVolumeBackup(s.db.QueryRowContext(ctx, clusterBackupSelect))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &NotFoundError{What: "Cluster backups"}
	}
	return b, err
}

func (s *Store) SetClusterBackup(ctx context.Context, b VolumeBackup) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO cluster_backup (id, destination, path, every_s, keep_recent_s, keep_daily) VALUES (1, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET destination = excluded.destination, path = excluded.path,
	every_s = excluded.every_s, keep_recent_s = excluded.keep_recent_s, keep_daily = excluded.keep_daily`,
		b.Destination, b.Path, int64(b.Every/time.Second), int64(b.KeepRecent/time.Second), b.KeepDaily)
	return err
}

func (s *Store) DeleteClusterBackup(ctx context.Context) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM cluster_backup WHERE id = 1")
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "Cluster backups"}
	}
	return nil
}

func (s *Store) AddClusterBackupUsage(ctx context.Context, bytes int64, backups int) error {
	_, err := s.db.ExecContext(ctx, "UPDATE cluster_backup SET stored_bytes = stored_bytes + ?, stored_backups = ? WHERE id = 1", bytes, backups)
	return err
}

func (s *Store) SetClusterBackupUsage(ctx context.Context, bytes int64, backups int) error {
	_, err := s.db.ExecContext(ctx, "UPDATE cluster_backup SET stored_bytes = ?, stored_backups = ?, collected_at = ? WHERE id = 1",
		bytes, backups, unix(s.now()))
	return err
}

func (s *Store) StartClusterBackupRun(ctx context.Context, name, actor string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO cluster_backup_runs (name, status, actor, started_at) VALUES (?, ?, ?, ?)",
		name, types.StatusRunning, actor, unix(s.now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishClusterBackupRun(ctx context.Context, r BackupRun) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE cluster_backup_runs SET status = ?, error = ?, size_bytes = ?, blocks = ?, new_blocks = ?, new_bytes = ?, finished_at = ?
WHERE id = ?`, r.Status, r.Error, r.SizeBytes, r.Blocks, r.NewBlocks, r.NewBytes, unix(s.now()), r.ID)
	return err
}

// ClusterBackupRuns lists the latest cluster backups, newest first.
func (s *Store) ClusterBackupRuns(ctx context.Context, limit int) ([]BackupRun, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, '', name, status, error, size_bytes, blocks, new_blocks, new_bytes, actor, started_at, COALESCE(finished_at, 0)
FROM cluster_backup_runs ORDER BY id DESC LIMIT ?`, limit)
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

// Snapshot writes a consistent copy of the database to path, while it is in
// use.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path)
	return err
}
