package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Release is an immutable, numbered snapshot of what an app runs: a rootfs
// artifact, the command for each process type, the image's runtime settings
// and the config vars.
type Release struct {
	ID          int64
	App         string
	Version     int
	Artifact    string              // path of the read-only rootfs image
	Processes   map[string][]string // process type -> argv
	Image       ImageConfig
	ConfigVars  map[string]string // global and app vars, merged
	Description string
	CreatedAt   time.Time
}

// ImageConfig is what the release keeps from the image's config.
type ImageConfig struct {
	Env        []string `json:"env,omitempty"` // KEY=value
	WorkingDir string   `json:"working_dir,omitempty"`
	User       string   `json:"user,omitempty"`
	Port       int      `json:"port"` // $PORT: the single EXPOSEd port, or 5000
}

// CreateRelease stores r as the app's next version and fills in ID, Version
// and CreatedAt.
func (s *Store) CreateRelease(ctx context.Context, r *Release) error {
	procs, err := json.Marshal(r.Processes)
	if err != nil {
		return err
	}
	image, err := json.Marshal(r.Image)
	if err != nil {
		return err
	}
	vars, err := json.Marshal(r.ConfigVars)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		id, err := appID(ctx, tx, r.App)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) + 1 FROM releases WHERE app_id = ?", id).Scan(&r.Version); err != nil {
			return err
		}
		r.CreatedAt = s.now().UTC().Truncate(time.Second)
		res, err := tx.ExecContext(ctx,
			"INSERT INTO releases (app_id, version, artifact, processes, image_config, config_vars, description, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			id, r.Version, r.Artifact, procs, image, vars, r.Description, unix(r.CreatedAt))
		if err != nil {
			return err
		}
		r.ID, err = res.LastInsertId()
		return err
	})
}

const releaseSelect = `
SELECT r.id, a.name, r.version, r.artifact, r.processes, r.image_config, r.config_vars, r.description, r.created_at
FROM releases r JOIN apps a ON a.id = r.app_id`

func (s *Store) Release(ctx context.Context, id int64) (*Release, error) {
	r, err := scanRelease(s.db.QueryRowContext(ctx, releaseSelect+" WHERE r.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &NotFoundError{What: "Release"}
	}
	return r, err
}

// CurrentRelease is the release an app is serving, or nil before its first
// deploy.
func (s *Store) CurrentRelease(ctx context.Context, app string) (*Release, error) {
	r, err := scanRelease(s.db.QueryRowContext(ctx, releaseSelect+" WHERE r.id = a.current_release_id AND a.name = ?", app))
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.App(ctx, app); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return r, err
}

func (s *Store) Releases(ctx context.Context, app string, limit int) ([]Release, error) {
	rows, err := s.db.QueryContext(ctx, releaseSelect+" WHERE a.name = ? ORDER BY r.version DESC LIMIT ?", app, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func scanRelease(row scanner) (*Release, error) {
	var r Release
	var procs, image, vars []byte
	var created int64
	if err := row.Scan(&r.ID, &r.App, &r.Version, &r.Artifact, &procs, &image, &vars, &r.Description, &created); err != nil {
		return nil, err
	}
	r.CreatedAt = fromUnix(created)
	for _, f := range []struct {
		raw []byte
		dst any
	}{{procs, &r.Processes}, {image, &r.Image}, {vars, &r.ConfigVars}} {
		if err := json.Unmarshal(f.raw, f.dst); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

func (s *Store) SetCurrentRelease(ctx context.Context, app string, releaseID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE apps SET current_release_id = ? WHERE name = ?", releaseID, app)
	return err
}

func (s *Store) SetDeployRelease(ctx context.Context, deployID, releaseID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE deploys SET release_id = ? WHERE id = ?", releaseID, deployID)
	return err
}

func (s *Store) SetAppStopped(ctx context.Context, app string, stopped bool) error {
	res, err := s.db.ExecContext(ctx, "UPDATE apps SET stopped = ? WHERE name = ?", stopped, app)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "App " + app}
	}
	return nil
}

// KeptArtifacts lists the rootfs artifacts worth keeping: those of each app's
// most recent releases (for restarts and rollbacks) and of anything still
// running. Other artifact files can be deleted.
func (s *Store) KeptArtifacts(ctx context.Context, perApp int) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT artifact FROM (
	SELECT artifact, ROW_NUMBER() OVER (PARTITION BY app_id ORDER BY version DESC) AS n FROM releases
) WHERE n <= ?
UNION
SELECT r.artifact FROM instances i JOIN releases r ON r.id = i.release_id`, perApp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out[a] = true
	}
	return out, rows.Err()
}
