package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/wes/jokku/internal/types"
)

func (s *Store) CreateDeploy(ctx context.Context, app, source, ref, actor string) (*types.Deploy, error) {
	var d *types.Deploy
	err := s.tx(ctx, func(tx *sql.Tx) error {
		id, err := appID(ctx, tx, app)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			"INSERT INTO deploys (app_id, source, source_ref, status, actor, created_at) VALUES (?, ?, ?, 'pending', ?, ?)",
			id, source, ref, actor, unix(s.now()))
		if err != nil {
			return err
		}
		deployID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		d, err = deploy(ctx, tx, deployID)
		return err
	})
	return d, err
}

// SetDeployStatus moves a deploy along. Terminal statuses also set
// finished_at.
func (s *Store) SetDeployStatus(ctx context.Context, id int64, status, errMsg string) error {
	var finished any
	if status == types.StatusSucceeded || status == types.StatusFailed {
		finished = unix(s.now())
	}
	_, err := s.db.ExecContext(ctx, "UPDATE deploys SET status = ?, error = ?, finished_at = COALESCE(?, finished_at) WHERE id = ?",
		status, errMsg, finished, id)
	return err
}

func (s *Store) Deploys(ctx context.Context, app string, limit int) ([]types.Deploy, error) {
	id, err := appID(ctx, s.db, app)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, deploySelect+" WHERE d.app_id = ? ORDER BY d.id DESC LIMIT ?", id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Deploy{}
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

const deploySelect = `
SELECT d.id, a.name, d.source, d.source_ref, d.status, d.error, d.actor, COALESCE(r.version, 0), d.created_at, d.finished_at
FROM deploys d JOIN apps a ON a.id = d.app_id LEFT JOIN releases r ON r.id = d.release_id`

func deploy(ctx context.Context, q querier, id int64) (*types.Deploy, error) {
	d, err := scanDeploy(q.QueryRowContext(ctx, deploySelect+" WHERE d.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &NotFoundError{What: "Deploy"}
	}
	return d, err
}

func scanDeploy(row scanner) (*types.Deploy, error) {
	var d types.Deploy
	var created int64
	var finished sql.NullInt64
	if err := row.Scan(&d.ID, &d.App, &d.Source, &d.SourceRef, &d.Status, &d.Error, &d.Actor, &d.Release, &created, &finished); err != nil {
		return nil, err
	}
	d.CreatedAt = fromUnix(created)
	if finished.Valid {
		t := fromUnix(finished.Int64)
		d.FinishedAt = &t
	}
	return &d, nil
}
