package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// Release is an immutable, numbered snapshot of what an app runs: a rootfs
// artifact, the command for each process type, the image's runtime settings
// and the config vars.
//
// A compose release has a Service per process type instead, each with its
// own image and settings; Artifact and Image are then unused.
type Release struct {
	ID          int64
	App         string
	Version     int
	Artifact    string // path of the read-only rootfs image
	ArtifactSHA string // hex SHA-256, for nodes that download it
	ArtifactLen int64
	Processes   map[string][]string // process type -> argv
	Image       ImageConfig
	ConfigVars  map[string]string // global and app vars, merged
	Services    map[string]Service
	Web         string // the process type that gets HTTP traffic; "" is "web"
	Description string
	CreatedAt   time.Time
}

// Service is one process type of a compose release.
type Service struct {
	Artifact    string      `json:"artifact"`
	ArtifactSHA string      `json:"artifact_sha256"`
	ArtifactLen int64       `json:"artifact_size"`
	Image       ImageConfig `json:"image"` // with the compose file's user, working_dir and port applied
	// Env is the service's environment from the compose file, KEY=value.
	// Config vars only reach it through ${VAR} in the file.
	Env       []string `json:"env,omitempty"`
	Replicas  int      `json:"replicas"`            // unless ps:scale says otherwise
	CPUs      int      `json:"cpus,omitempty"`      // unless resource:limit says otherwise
	MemoryMB  int      `json:"memory_mb,omitempty"` // unless resource:limit says otherwise
	Restart   string   `json:"restart,omitempty"`   // a ps restart-policy; "" is the app's
	Check     string   `json:"check"`               // types.CheckTCP or types.CheckUp
	StopSecs  int      `json:"stop_secs,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
	// BuildKey says how the image was made. A config change that alters it
	// needs a new build (ps:rebuild), not just a restart.
	BuildKey string `json:"build_key"`
	// The image's own settings (Port 0 when it EXPOSEs none), kept so a
	// config change can recompute the service without building again.
	Base       ImageConfig `json:"base"`
	Entrypoint []string    `json:"entrypoint,omitempty"`
	Cmd        []string    `json:"cmd,omitempty"`
}

// Compose reports whether r came from a compose file.
func (r *Release) Compose() bool { return len(r.Services) > 0 }

// WebProcess is the process type that gets HTTP traffic.
func (r *Release) WebProcess() string {
	if r.Web != "" || r.Compose() {
		return r.Web
	}
	return "web"
}

// ImageFor is the rootfs and image settings of a process type.
func (r *Release) ImageFor(proc string) (artifact, sha string, size int64, img ImageConfig) {
	if s, ok := r.Services[proc]; ok {
		return s.Artifact, s.ArtifactSHA, s.ArtifactLen, s.Image
	}
	return r.Artifact, r.ArtifactSHA, r.ArtifactLen, r.Image
}

// Artifacts lists every rootfs the release uses.
func (r *Release) Artifacts() []string {
	if !r.Compose() {
		return []string{r.Artifact}
	}
	var out []string
	for _, s := range r.Services {
		out = append(out, s.Artifact)
	}
	return out
}

// ImageConfig is what the release keeps from the image's config.
type ImageConfig struct {
	Env        []string `json:"env,omitempty"` // KEY=value
	WorkingDir string   `json:"working_dir,omitempty"`
	User       string   `json:"user,omitempty"`
	Port       int      `json:"port"` // $PORT picked from the image's EXPOSE, see build.Port
	StopSignal string   `json:"stop_signal,omitempty"`
}

// Port is $PORT for the release's instances: the PORT config var when it is
// a valid port, otherwise the one picked from the image.
func (r *Release) Port() int {
	if n, err := strconv.Atoi(r.ConfigVars["PORT"]); err == nil && n > 0 && n < 65536 {
		return n
	}
	return r.Image.Port
}

// PortFor is the port a process type listens on: a compose service's own,
// otherwise Port. 0 means it listens on none.
func (r *Release) PortFor(proc string) int {
	if s, ok := r.Services[proc]; ok {
		return s.Image.Port
	}
	return r.Port()
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
	services := []byte("{}")
	if len(r.Services) > 0 {
		if services, err = json.Marshal(r.Services); err != nil {
			return err
		}
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
			"INSERT INTO releases (app_id, version, artifact, artifact_sha256, artifact_size, processes, image_config, config_vars, services, web, description, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			id, r.Version, r.Artifact, r.ArtifactSHA, r.ArtifactLen, procs, image, vars, services, r.Web, r.Description, unix(r.CreatedAt))
		if err != nil {
			return err
		}
		r.ID, err = res.LastInsertId()
		return err
	})
}

const releaseSelect = `
SELECT r.id, a.name, r.version, r.artifact, r.artifact_sha256, r.artifact_size, r.processes, r.image_config, r.config_vars,
	r.services, r.web, r.description, r.created_at
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
	var procs, image, vars, services []byte
	var created int64
	if err := row.Scan(&r.ID, &r.App, &r.Version, &r.Artifact, &r.ArtifactSHA, &r.ArtifactLen, &procs, &image, &vars,
		&services, &r.Web, &r.Description, &created); err != nil {
		return nil, err
	}
	r.CreatedAt = fromUnix(created)
	for _, f := range []struct {
		raw []byte
		dst any
	}{{procs, &r.Processes}, {image, &r.Image}, {vars, &r.ConfigVars}, {services, &r.Services}} {
		if err := json.Unmarshal(f.raw, f.dst); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// SetArtifactSum records a rootfs artifact's checksum and size on every
// release that uses it (releases from before clusters lack them).
func (s *Store) SetArtifactSum(ctx context.Context, artifact, sha string, size int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE releases SET artifact_sha256 = ?, artifact_size = ? WHERE artifact = ?", sha, size, artifact)
	return err
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
SELECT artifact, services FROM (
	SELECT artifact, services, ROW_NUMBER() OVER (PARTITION BY app_id ORDER BY version DESC) AS n FROM releases
) WHERE n <= ?
UNION
SELECT r.artifact, r.services FROM instances i JOIN releases r ON r.id = i.release_id`, perApp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var r Release
		var services []byte
		if err := rows.Scan(&r.Artifact, &services); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(services, &r.Services); err != nil {
			return nil, err
		}
		for _, a := range r.Artifacts() {
			out[a] = true
		}
	}
	return out, rows.Err()
}

// PromoteRelease makes releaseID the app's current release and moves the
// instances a rollout kept (unchanged process types) onto it, in one step so
// the proxy never sees them as an old release's.
func (s *Store) PromoteRelease(ctx context.Context, app string, releaseID int64, kept []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range kept {
			if _, err := tx.ExecContext(ctx, "UPDATE instances SET release_id = ? WHERE id = ?", releaseID, id); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, "UPDATE apps SET current_release_id = ? WHERE name = ?", releaseID, app)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return &NotFoundError{What: "App " + app}
		}
		return nil
	})
}
