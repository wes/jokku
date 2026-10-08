package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/wes/jokku/internal/types"
)

// Instance is one microVM: a process of a release, placed on a node.
type Instance struct {
	ID          string
	App         string
	ReleaseID   int64
	Release     int // version, for display
	ProcessType string
	Index       int
	Node        string
	IP          string
	Port        int
	CPUs        int
	MemoryMB    int
	Desired     string // DesiredRunning or DesiredStopped
	State       string
	HealthyOnce bool
	Restarts    int
	RetireAt    *time.Time
	StartedAt   time.Time
	CreatedAt   time.Time

	Replaces     string // ID of the instance this one is taking over from
	CPUPercent   float64
	MemoryUsedMB int
	ReportedAt   time.Time
}

const (
	DesiredRunning = "running"
	DesiredStopped = "stopped"

	StatePending  = "pending"  // not started yet
	StateStarting = "starting" // booting, not yet passing checks
	StateHealthy  = "healthy"  // passing checks
	StateCrashed  = "crashed"  // exited; will be restarted
	StateFailed   = "failed"   // exited and will not be restarted
	StatePulling  = "pulling"  // its node is downloading the root filesystem
	StateStopped  = "stopped"  // kept off while the app is stopped
)

// Name is how Dokku names processes: web.1, worker.2.
func (i Instance) Name() string { return fmt.Sprintf("%s.%d", i.ProcessType, i.Index) }

// CreateInstance stores a new instance, giving it an ID and the lowest free
// address in subnet (.2 to .254; .1 is the node).
func (s *Store) CreateInstance(ctx context.Context, in *Instance, subnet netip.Prefix) error {
	b := make([]byte, 4)
	rand.Read(b)
	in.ID = hex.EncodeToString(b)
	in.State = StatePending
	in.CreatedAt = s.now().UTC().Truncate(time.Second)
	return s.tx(ctx, func(tx *sql.Tx) error {
		id, err := appID(ctx, tx, in.App)
		if err != nil {
			return err
		}
		used := map[string]bool{}
		rows, err := tx.QueryContext(ctx, "SELECT ip FROM instances")
		if err != nil {
			return err
		}
		for rows.Next() {
			var ip string
			if err := rows.Scan(&ip); err != nil {
				rows.Close()
				return err
			}
			used[ip] = true
		}
		rows.Close()
		in.IP = ""
		for a := subnet.Masked().Addr().Next().Next(); subnet.Contains(a) && a.As4()[3] < 255; a = a.Next() {
			if !used[a.String()] {
				in.IP = a.String()
				break
			}
		}
		if in.IP == "" {
			return fmt.Errorf("no free addresses left in %s", subnet)
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO instances (id, app_id, release_id, process_type, idx, node, ip, port, cpus, memory_mb, desired, state, replaces, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.ID, id, in.ReleaseID, in.ProcessType, in.Index, in.Node, in.IP, in.Port, in.CPUs, in.MemoryMB, in.Desired, in.State, in.Replaces, unix(in.CreatedAt))
		return err
	})
}

const instanceSelect = `
SELECT i.id, a.name, i.release_id, r.version, i.process_type, i.idx, i.node, i.ip, i.port, i.cpus, i.memory_mb,
	i.desired, i.state, i.healthy_once, i.restarts, i.retire_at, COALESCE(i.started_at, 0), i.created_at,
	i.replaces, i.cpu_percent, i.memory_used_mb, i.reported_at
FROM instances i JOIN apps a ON a.id = i.app_id JOIN releases r ON r.id = i.release_id`

// Instances lists an app's instances, or every instance for app "".
func (s *Store) Instances(ctx context.Context, app string) ([]Instance, error) {
	q, args := instanceSelect+" ORDER BY a.name, i.process_type, i.idx, i.created_at", []any{}
	if app != "" {
		if _, err := appID(ctx, s.db, app); err != nil {
			return nil, err
		}
		q, args = instanceSelect+" WHERE a.name = ? ORDER BY i.process_type, i.idx, i.created_at", []any{app}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		var in Instance
		var retire sql.NullInt64
		var started, created, reported int64
		err := rows.Scan(&in.ID, &in.App, &in.ReleaseID, &in.Release, &in.ProcessType, &in.Index, &in.Node, &in.IP, &in.Port,
			&in.CPUs, &in.MemoryMB, &in.Desired, &in.State, &in.HealthyOnce, &in.Restarts, &retire, &started, &created,
			&in.Replaces, &in.CPUPercent, &in.MemoryUsedMB, &reported)
		if err != nil {
			return nil, err
		}
		if retire.Valid {
			t := fromUnix(retire.Int64)
			in.RetireAt = &t
		}
		in.StartedAt, in.CreatedAt, in.ReportedAt = fromUnix(started), fromUnix(created), fromUnix(reported)
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) Instance(ctx context.Context, id string) (*Instance, error) {
	all, err := s.Instances(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, in := range all {
		if in.ID == id {
			return &in, nil
		}
	}
	return nil, &NotFoundError{What: "Instance " + id}
}

// ReportInstance stores what a node's agent observed about one of its
// instances and returns the state it had before ("" if the instance is
// unknown or not that node's).
func (s *Store) ReportInstance(ctx context.Context, node string, st types.InstanceStatus) (before string, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT state FROM instances WHERE id = ? AND node = ?", st.ID, node).Scan(&before)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var started any
	if !st.StartedAt.IsZero() {
		started = unix(st.StartedAt)
	}
	_, err = s.db.ExecContext(ctx, `
UPDATE instances SET state = ?, healthy_once = ?, restarts = ?, started_at = COALESCE(?, started_at),
	cpu_percent = ?, memory_used_mb = ?, reported_at = ?
WHERE id = ? AND node = ?`,
		st.State, st.HealthyOnce, st.Restarts, started, st.CPUPercent, st.MemoryMB, unix(s.now()), st.ID, node)
	return before, err
}

// InstanceStarted records a (re)start: the instance is booting again.
func (s *Store) InstanceStarted(ctx context.Context, id string, restart bool) error {
	n := 0
	if restart {
		n = 1
	}
	_, err := s.db.ExecContext(ctx, "UPDATE instances SET state = ?, started_at = ?, restarts = restarts + ? WHERE id = ?",
		StateStarting, unix(s.now()), n, id)
	return err
}

func (s *Store) SetInstanceState(ctx context.Context, id, state string) error {
	q := "UPDATE instances SET state = ? WHERE id = ?"
	if state == StateHealthy {
		q = "UPDATE instances SET state = ?, healthy_once = 1 WHERE id = ?"
	}
	_, err := s.db.ExecContext(ctx, q, state, id)
	return err
}

// StopInstances marks instances to be stopped, at retireAt or right away when
// it is nil.
func (s *Store) StopInstances(ctx context.Context, ids []string, retireAt *time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	var at any
	if retireAt != nil {
		at = unix(*retireAt)
	}
	args := []any{DesiredStopped, at}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, "UPDATE instances SET desired = ?, retire_at = ? WHERE id IN (?"+strings.Repeat(", ?", len(ids)-1)+")", args...)
	return err
}

func (s *Store) DeleteInstance(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM instances WHERE id = ?", id)
	return err
}
