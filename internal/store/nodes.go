package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
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
}

const (
	RoleControl = "control"
	RoleWorker  = "worker"
)

// NodeDownAfter is how long a node may go without reporting before it is
// considered down.
const NodeDownAfter = 30 * time.Second

func (n Node) Status(now time.Time) string {
	switch {
	case now.Sub(n.LastSeen) > NodeDownAfter:
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
INSERT INTO nodes (name, role, subnet_index, address, arch, cpus, memory_mb, last_seen, created_at)
VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?)
ON CONFLICT (subnet_index) DO UPDATE SET
	name = excluded.name, address = excluded.address, arch = excluded.arch,
	cpus = excluded.cpus, memory_mb = excluded.memory_mb, last_seen = excluded.last_seen`,
		n.Name, RoleControl, n.Address, n.Arch, n.CPUs, n.MemoryMB, unix(s.now()), unix(s.now()))
	return err
}

func (s *Store) TouchNode(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE nodes SET last_seen = ? WHERE name = ?", unix(s.now()), name)
	return err
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

const nodeSelect = `SELECT name, role, subnet_index, address, arch, cpus, memory_mb, schedulable, ingress, draining, last_seen, created_at FROM nodes`

func scanNode(row scanner) (*Node, error) {
	var n Node
	var seen, created int64
	err := row.Scan(&n.Name, &n.Role, &n.SubnetIndex, &n.Address, &n.Arch, &n.CPUs, &n.MemoryMB,
		&n.Schedulable, &n.Ingress, &n.Draining, &seen, &created)
	if err != nil {
		return nil, err
	}
	n.LastSeen, n.CreatedAt = fromUnix(seen), fromUnix(created)
	return &n, nil
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
