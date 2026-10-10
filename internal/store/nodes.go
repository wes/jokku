package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/wes/jokku/internal/types"
)

// Node is the stored form of a cluster member. The API turns it into
// types.Node, adding addresses derived from the cluster network.
type Node struct {
	Name        string
	Role        string
	SubnetIndex int
	Address     string
	Arch        string
	CPUs        int
	MemoryMB    int
	Schedulable bool
	Ingress     bool
	Draining    bool
	LastSeen    time.Time
	CreatedAt   time.Time

	Version     string
	WGPublicKey string
	WGEndpoint  string   // host:port
	TokenHash   string   // hash of the agent's credential
	AgentToken  string   // the control node's credential for the agent's API
	CanRun      string   // why microVMs cannot run there; empty if they can
	Features    []string // what its agent supports, e.g. types.FeatureVolumes
	Metrics     types.NodeMetrics
	// RemovedAt is when an edge was removed. Its credentials are revoked,
	// but it stays a peer for a moment, so its next poll tells it to stop
	// routing; then it is deleted.
	RemovedAt time.Time
}

// Removed reports whether the node is an edge on its way out.
func (n Node) Removed() bool { return !n.RemovedAt.IsZero() }

const (
	RoleControl = "control"
	RoleWorker  = "worker"
	RoleEdge    = types.RoleEdge
)

// Edge reports whether the node is an edge: it receives traffic and runs
// nothing.
func (n Node) Edge() bool { return n.Role == RoleEdge }

// NodeDownAfter is how long a node may go without reporting before it is
// considered down. (A variable so tests can shorten it.)
var NodeDownAfter = 30 * time.Second

func (n Node) Ready(now time.Time) bool { return now.Sub(n.LastSeen) <= NodeDownAfter }

// Has reports whether the node's agent supports feature.
func (n Node) Has(feature string) bool { return slices.Contains(n.Features, feature) }

func (n Node) Status(now time.Time) string {
	switch {
	case !n.Ready(now):
		return "down"
	case n.Draining:
		return "draining"
	default:
		return "ready"
	}
}

// RegisterControlNode records the control node itself (subnet 1) or updates
// its facts on restart.
func (s *Store) RegisterControlNode(ctx context.Context, n Node) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO nodes (name, role, subnet_index, address, arch, cpus, memory_mb, version, last_seen, created_at)
VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (subnet_index) DO UPDATE SET
	name = excluded.name, address = excluded.address, arch = excluded.arch,
	cpus = excluded.cpus, memory_mb = excluded.memory_mb, version = excluded.version, last_seen = excluded.last_seen`,
		n.Name, RoleControl, n.Address, n.Arch, n.CPUs, n.MemoryMB, n.Version, unix(s.now()), unix(s.now()))
	return err
}

// CreateNode adds a worker (or, with Role set to RoleEdge, an edge) with the
// lowest free subnet index (2 to 254) and fills in SubnetIndex. Edges are
// never schedulable.
func (s *Store) CreateNode(ctx context.Context, n *Node) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes WHERE name = ?", n.Name).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return &ExistsError{What: "Node " + n.Name}
		}
		used := map[int]bool{}
		rows, err := tx.QueryContext(ctx, "SELECT subnet_index FROM nodes")
		if err != nil {
			return err
		}
		for rows.Next() {
			var i int
			if err := rows.Scan(&i); err != nil {
				rows.Close()
				return err
			}
			used[i] = true
		}
		rows.Close()
		n.SubnetIndex = 0
		for i := 2; i <= 254; i++ {
			if !used[i] {
				n.SubnetIndex = i
				break
			}
		}
		if n.SubnetIndex == 0 {
			return errors.New("the cluster is full (254 nodes)")
		}
		if n.Role != RoleEdge {
			n.Role = RoleWorker
		}
		n.Schedulable, n.Ingress = n.Role != RoleEdge, true
		// An edge has not reported yet: it is down until its agent does.
		seen := unix(s.now())
		if n.Role == RoleEdge {
			seen = 0
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO nodes (name, role, subnet_index, address, arch, cpus, memory_mb, wg_public_key, wg_endpoint,
	token_hash, agent_token, version, schedulable, ingress, last_seen, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			n.Name, n.Role, n.SubnetIndex, n.Address, n.Arch, n.CPUs, n.MemoryMB, n.WGPublicKey, n.WGEndpoint,
			n.TokenHash, n.AgentToken, n.Version, n.Schedulable, n.Ingress, seen, unix(s.now()))
		return err
	})
}

func (s *Store) Nodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, nodeSelect+" ORDER BY subnet_index")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, *n)
	}
	return nodes, rows.Err()
}

func (s *Store) Node(ctx context.Context, name string) (*Node, error) {
	n, err := scanNode(s.db.QueryRowContext(ctx, nodeSelect+" WHERE name = ?", name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &NotFoundError{What: "Node " + name}
	}
	return n, err
}

// NodeByTokenHash finds the node an agent credential belongs to.
func (s *Store) NodeByTokenHash(ctx context.Context, hash string) (*Node, error) {
	if hash == "" {
		return nil, &NotFoundError{What: "Node"}
	}
	n, err := scanNode(s.db.QueryRowContext(ctx, nodeSelect+" WHERE token_hash = ?", hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &NotFoundError{What: "Node"}
	}
	return n, err
}

const nodeSelect = `SELECT name, role, subnet_index, address, arch, cpus, memory_mb, schedulable, ingress, draining,
	last_seen, created_at, version, wg_public_key, wg_endpoint, token_hash, agent_token, can_run, metrics, features, removed_at FROM nodes`

func scanNode(row scanner) (*Node, error) {
	var n Node
	var seen, created, removed int64
	var metrics []byte
	var features string
	err := row.Scan(&n.Name, &n.Role, &n.SubnetIndex, &n.Address, &n.Arch, &n.CPUs, &n.MemoryMB,
		&n.Schedulable, &n.Ingress, &n.Draining, &seen, &created,
		&n.Version, &n.WGPublicKey, &n.WGEndpoint, &n.TokenHash, &n.AgentToken, &n.CanRun, &metrics, &features, &removed)
	if err != nil {
		return nil, err
	}
	if features != "" {
		n.Features = strings.Split(features, ",")
	}
	n.LastSeen, n.CreatedAt, n.RemovedAt = fromUnix(seen), fromUnix(created), fromUnix(removed)
	json.Unmarshal(metrics, &n.Metrics)
	return &n, nil
}

// NodeReported records an agent's report: it is alive, and these are its
// facts.
func (s *Store) NodeReported(ctx context.Context, name, version, canRun string, features []string, m types.NodeMetrics) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	q := "UPDATE nodes SET last_seen = ?, version = ?, can_run = ?, features = ?, metrics = ?"
	args := []any{unix(s.now()), version, canRun, strings.Join(features, ","), b}
	if m.CPUs > 0 {
		q += ", cpus = ?, memory_mb = ?"
		args = append(args, m.CPUs, m.MemoryMB)
	}
	_, err = s.db.ExecContext(ctx, q+" WHERE name = ?", append(args, name)...)
	return err
}

// SetNodeWireGuard records a node's WireGuard public key and endpoint.
func (s *Store) SetNodeWireGuard(ctx context.Context, name, publicKey, endpoint string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE nodes SET wg_public_key = ?, wg_endpoint = ? WHERE name = ?", publicKey, endpoint, name)
	return err
}

// SetNodeFlag updates one of the boolean node settings.
func (s *Store) SetNodeFlag(ctx context.Context, name, flag string, value bool) error {
	switch flag {
	case "schedulable", "ingress", "draining":
	default:
		return errors.New("unknown node setting " + flag)
	}
	res, err := s.db.ExecContext(ctx, "UPDATE nodes SET "+flag+" = ? WHERE name = ?", value, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "Node " + name}
	}
	return nil
}

// DeleteNode removes a worker. Its instances are rescheduled by the
// controller, which notices their node is gone.
func (s *Store) DeleteNode(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM nodes WHERE name = ? AND role != ?", name, RoleControl)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.Node(ctx, name); err != nil {
			return err
		}
		return errors.New("the control node cannot be removed")
	}
	return nil
}

// RemoveEdge takes an edge out of the cluster. One that never connected is
// deleted; otherwise its credentials are revoked at once, and DeleteRemovedEdges
// deletes it later.
func (s *Store) RemoveEdge(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM nodes WHERE name = ? AND role = ? AND last_seen = 0", name, RoleEdge)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	res, err = s.db.ExecContext(ctx, "UPDATE nodes SET token_hash = '', removed_at = ? WHERE name = ? AND role = ? AND removed_at = 0",
		unix(s.now()), name, RoleEdge)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: "Edge " + name}
	}
	return nil
}

// DeleteRemovedEdges deletes the edges removed before t, and says how many.
func (s *Store) DeleteRemovedEdges(ctx context.Context, t time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM nodes WHERE role = ? AND removed_at > 0 AND removed_at < ?", RoleEdge, unix(t))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Join tokens are stored hashed; a non-reusable token works once.

func (s *Store) CreateJoinToken(ctx context.Context, hash string, expires time.Time, reusable bool) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO join_tokens (hash, reusable, expires_at, created_at) VALUES (?, ?, ?, ?)",
		hash, reusable, unix(expires), unix(s.now()))
	return err
}

// UseJoinToken checks a token and consumes it unless it is reusable.
func (s *Store) UseJoinToken(ctx context.Context, hash string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		tx.ExecContext(ctx, "DELETE FROM join_tokens WHERE expires_at < ?", unix(s.now()))
		var reusable bool
		err := tx.QueryRowContext(ctx, "SELECT reusable FROM join_tokens WHERE hash = ?", hash).Scan(&reusable)
		if errors.Is(err, sql.ErrNoRows) {
			return &NotFoundError{What: "Join token (or it expired or was already used; create another with jokku cluster:join-command)"}
		}
		if err != nil {
			return err
		}
		if !reusable {
			_, err = tx.ExecContext(ctx, "DELETE FROM join_tokens WHERE hash = ?", hash)
		}
		return err
	})
}

// SetNodeAgentToken sets the control node's credential for a node's agent
// API.
func (s *Store) SetNodeAgentToken(ctx context.Context, name, token string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE nodes SET agent_token = ? WHERE name = ?", token, name)
	return err
}

// SetNodeToken sets the hash of a node's agent credential.
func (s *Store) SetNodeToken(ctx context.Context, name, hash string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE nodes SET token_hash = ? WHERE name = ?", hash, name)
	return err
}
