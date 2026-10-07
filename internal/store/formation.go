package store

import (
	"context"
	"database/sql"

	"github.com/wes/jokku/internal/types"
)

// Default instance size when neither the process type nor the app overrides
// it.
const (
	DefaultCPUs     = 1
	DefaultMemoryMB = 256
)

// Formation returns the explicitly scaled process types with their effective
// sizes. Process types that were never scaled are not listed; as in Dokku,
// the deployer runs one "web" and zero of everything else for them.
func (s *Store) Formation(ctx context.Context, app string) ([]types.Process, error) {
	id, err := appID(ctx, s.db, app)
	if err != nil {
		return nil, err
	}
	res, err := resources(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT process_type, quantity FROM formations WHERE app_id = ? ORDER BY process_type", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	procs := []types.Process{}
	for rows.Next() {
		var p types.Process
		if err := rows.Scan(&p.Type, &p.Quantity); err != nil {
			return nil, err
		}
		size := res.Effective(p.Type)
		p.CPUs, p.MemoryMB = size.CPUs, size.MemoryMB
		procs = append(procs, p)
	}
	return procs, rows.Err()
}

func (s *Store) Scale(ctx context.Context, app string, quantities map[string]int) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		id, err := appID(ctx, tx, app)
		if err != nil {
			return err
		}
		for proc, n := range quantities {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO formations (app_id, process_type, quantity) VALUES (?, ?, ?) ON CONFLICT (app_id, process_type) DO UPDATE SET quantity = excluded.quantity",
				id, proc, n); err != nil {
				return err
			}
		}
		return nil
	})
}

// AppResources holds the size overrides for an app.
type AppResources struct {
	Default types.ResourceSize
	Process map[string]types.ResourceSize
}

// Effective resolves the size for a process type: process override, then app
// default, then the built-in default.
func (r AppResources) Effective(proc string) types.ResourceSize {
	size := types.ResourceSize{CPUs: DefaultCPUs, MemoryMB: DefaultMemoryMB}
	for _, o := range []types.ResourceSize{r.Default, r.Process[proc]} {
		if o.CPUs > 0 {
			size.CPUs = o.CPUs
		}
		if o.MemoryMB > 0 {
			size.MemoryMB = o.MemoryMB
		}
	}
	return size
}

func (s *Store) Resources(ctx context.Context, app string) (AppResources, error) {
	id, err := appID(ctx, s.db, app)
	if err != nil {
		return AppResources{}, err
	}
	return resources(ctx, s.db, id)
}

func resources(ctx context.Context, q querier, id int64) (AppResources, error) {
	out := AppResources{Process: map[string]types.ResourceSize{}}
	rows, err := q.QueryContext(ctx, "SELECT process_type, cpus, memory_mb FROM resources WHERE app_id = ?", id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var proc string
		var size types.ResourceSize
		if err := rows.Scan(&proc, &size.CPUs, &size.MemoryMB); err != nil {
			return out, err
		}
		if proc == "" {
			out.Default = size
		} else {
			out.Process[proc] = size
		}
	}
	return out, rows.Err()
}

// SetResources applies a limits change. Nil fields are left alone; zero
// clears the override.
func (s *Store) SetResources(ctx context.Context, app string, l types.ResourceLimits) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		id, err := appID(ctx, tx, app)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO resources (app_id, process_type) VALUES (?, ?) ON CONFLICT DO NOTHING", id, l.ProcessType); err != nil {
			return err
		}
		if l.CPUs != nil {
			if _, err := tx.ExecContext(ctx, "UPDATE resources SET cpus = ? WHERE app_id = ? AND process_type = ?", *l.CPUs, id, l.ProcessType); err != nil {
				return err
			}
		}
		if l.MemoryMB != nil {
			if _, err := tx.ExecContext(ctx, "UPDATE resources SET memory_mb = ? WHERE app_id = ? AND process_type = ?", *l.MemoryMB, id, l.ProcessType); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM resources WHERE app_id = ? AND cpus = 0 AND memory_mb = 0", id)
		return err
	})
}
