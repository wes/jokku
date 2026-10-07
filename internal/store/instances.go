package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
	"time"
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
}

const (
	DesiredRunning = "running"
	DesiredStopped = "stopped"

	StatePending  = "pending"  // not started yet
	StateStarting = "starting" // booting, not yet passing checks
	StateHealthy  = "healthy"  // passing checks
	StateCrashed  = "crashed"  // exited; will be restarted
	StateFailed   = "failed"   // exited and will not be restarted
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
INSERT INTO instances (id, app_id, release_id, process_type, idx, node, ip, port, cpus, memory_mb, desired, state, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.ID, id, in.ReleaseID, in.ProcessType, in.Index, in.Node, in.IP, in.Port, in.CPUs, in.MemoryMB, in.Desired, in.State, unix(in.CreatedAt))
		return err
	})
}

const instanceSelect = `
SELECT i.id, a.name, i.release_id, r.version, i.process_type, i.idx, i.node, i.ip, i.port, i.cpus, i.memory_mb,
	i.desired, i.state, i.healthy_once, i.restarts, i.retire_at, COALESCE(i.started_at, 0), i.created_at
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
		var started, created int64
		err := rows.Scan(&in.ID, &in.App, &in.ReleaseID, &in.Release, &in.ProcessType, &in.Index, &in.Node, &in.IP, &in.Port,
			&in.CPUs, &in.MemoryMB, &in.Desired, &in.State, &in.HealthyOnce, &in.Restarts, &retire, &started, &created)
		if err != nil {
			return nil, err
		}
		if retire.Valid {
			t := fromUnix(retire.Int64)
			in.RetireAt = &t
		}
		in.StartedAt, in.CreatedAt = fromUnix(started), fromUnix(created)
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
