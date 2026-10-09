package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/wes/jokku/internal/types"
)

// Volume is persistent storage for an app's instances. A local volume is a
// disk on one node; the move fields describe a copy to another node that is
// in progress.
type Volume struct {
	ID     string
	App    string // "" once the app was destroyed (the volume is being deleted)
	Name   string
	Type   string
	SizeMB int
	State  string // VolumeNew, VolumeReady or VolumeDestroying
	Node   string // where a local volume's disk is; "" until first used
	Status string // what Node last reported: ready, missing or moved
	UsedMB int
	// PreviousNode still has the disk's old copy after a move, until the
	// new node reports it has the disk.
	PreviousNode string

	MovingTo  string
	MoveToken string
	Transfer  string // what MovingTo last reported: copying, synced or received
	CopiedMB  int
	MoveError string
	// Restore, while the disk is being restored from a backup, is the
	// types.RestoreSpec (without credentials) as JSON. The disk is made
	// on MovingTo, or on Node when MovingTo is "", and MoveToken, Transfer,
	// CopiedMB and MoveError follow it as they do a move.
	Restore string
	// RestoreResult is how the last restore ended: "" while one runs,
	// "restored", or why it failed.
	RestoreResult string
	// StaleNode keeps the old copy of the disk after the volume was
	// restored onto another node while it was down: it may hold writes
	// newer than the backup. DiscardStale asks it to delete that copy.
	StaleNode    string
	DiscardStale bool

	Mounts    []types.VolumeMount
	CreatedAt time.Time
}

// DefaultVolumeMB is a new volume's size unless one is given: a limit, not
// an allocation, since disks are sparse.
const DefaultVolumeMB = 10 * 1024

const (
	VolumeNew        = "new"        // no disk yet: it is made, empty, where it is first used
	VolumeReady      = "ready"      // its disk exists on Node
	VolumeDestroying = "destroying" // deleted; waiting for Node to delete the disk
)

// Moving reports whether the volume is being copied to another node.
func (v Volume) Moving() bool { return v.MovingTo != "" }

// CreateVolume stores a new volume and gives it an ID.
func (s *Store) CreateVolume(ctx context.Context, v *Volume) error {
	b := make([]byte, 6)
	rand.Read(b)
	v.ID = hex.EncodeToString(b)
	v.State = VolumeNew
	v.CreatedAt = s.now().UTC().Truncate(time.Second)
	id, err := appID(ctx, s.db, v.App)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO volumes (id, app_id, name, type, size_mb, state, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		v.ID, id, v.Name, v.Type, v.SizeMB, v.State, unix(v.CreatedAt))
	if isUniqueViolation(err) {
		return &ExistsError{What: "Volume " + v.Name}
	}
	return err
}

const volumeSelect = `
SELECT v.id, COALESCE(a.name, ''), v.name, v.type, v.size_mb, v.state, v.node, v.status, v.used_mb, v.previous_node,
	v.moving_to, v.move_token, v.transfer, v.copied_mb, v.move_error, v.restore, v.restore_result, v.stale_node, v.discard_stale, v.created_at
FROM volumes v LEFT JOIN apps a ON a.id = v.app_id`

// Volumes lists an app's volumes, or every volume (including those of
// destroyed apps) for app "".
func (s *Store) Volumes(ctx context.Context, app string) ([]Volume, error) {
	q, args := volumeSelect+" ORDER BY a.name, v.name, v.created_at", []any{}
	if app != "" {
		if _, err := appID(ctx, s.db, app); err != nil {
			return nil, err
		}
		q, args = volumeSelect+" WHERE a.name = ? ORDER BY v.name, v.created_at", []any{app}
	}
	return s.queryVolumes(ctx, q, args...)
}

// Volume finds an app's volume by name.
func (s *Store) Volume(ctx context.Context, app, name string) (*Volume, error) {
	if _, err := appID(ctx, s.db, app); err != nil {
		return nil, err
	}
	vols, err := s.queryVolumes(ctx, volumeSelect+" WHERE a.name = ? AND v.name = ? AND v.state != ?", app, name, VolumeDestroying)
	if err != nil {
		return nil, err
	}
	if len(vols) == 0 {
		return nil, &NotFoundError{What: "Volume " + name}
	}
	return &vols[0], nil
}

func (s *Store) VolumeByID(ctx context.Context, id string) (*Volume, error) {
	vols, err := s.queryVolumes(ctx, volumeSelect+" WHERE v.id = ?", id)
	if err != nil {
		return nil, err
	}
	if len(vols) == 0 {
		return nil, &NotFoundError{What: "Volume " + id}
	}
	return &vols[0], nil
}

func (s *Store) queryVolumes(ctx context.Context, q string, args ...any) ([]Volume, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Volume
	byID := map[string]int{}
	for rows.Next() {
		var v Volume
		var created int64
		err := rows.Scan(&v.ID, &v.App, &v.Name, &v.Type, &v.SizeMB, &v.State, &v.Node, &v.Status, &v.UsedMB, &v.PreviousNode,
			&v.MovingTo, &v.MoveToken, &v.Transfer, &v.CopiedMB, &v.MoveError, &v.Restore, &v.RestoreResult, &v.StaleNode, &v.DiscardStale, &created)
		if err != nil {
			return nil, err
		}
		v.CreatedAt = fromUnix(created)
		v.Mounts = []types.VolumeMount{}
		byID[v.ID] = len(out)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(out) == 0 {
		return out, nil
	}
	mrows, err := s.db.QueryContext(ctx, "SELECT volume_id, process_type, path FROM volume_mounts ORDER BY process_type, path")
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var id string
		var m types.VolumeMount
		if err := mrows.Scan(&id, &m.ProcessType, &m.Path); err != nil {
			return nil, err
		}
		if i, ok := byID[id]; ok {
			out[i].Mounts = append(out[i].Mounts, m)
		}
	}
	return out, mrows.Err()
}

func (s *Store) SetVolumeSize(ctx context.Context, id string, sizeMB int) error {
	_, err := s.db.ExecContext(ctx, "UPDATE volumes SET size_mb = ? WHERE id = ?", sizeMB, id)
	return err
}

// DestroyVolume deletes a volume. One whose disk was never made is gone
// right away; otherwise it is kept, as destroying, until its node reports
// the disk deleted.
func (s *Store) DestroyVolume(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return destroyVolumes(ctx, tx, "id = ?", id)
	})
}

func destroyVolumes(ctx context.Context, tx *sql.Tx, where string, arg any) error {
	stmts := []string{
		"DELETE FROM volume_mounts WHERE volume_id IN (SELECT id FROM volumes WHERE " + where + ")",
		"DELETE FROM volumes WHERE node = '' AND " + where,
		"UPDATE volumes SET state = 'destroying', previous_node = '', moving_to = '', move_token = '', transfer = '', copied_mb = 0, move_error = '', restore = '' WHERE " + where,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q, arg); err != nil {
			return err
		}
	}
	return nil
}

// DeleteVolume removes a volume's record.
func (s *Store) DeleteVolume(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM volumes WHERE id = ?", id)
	return err
}

func (s *Store) AddVolumeMount(ctx context.Context, id string, m types.VolumeMount) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO volume_mounts (volume_id, process_type, path) VALUES (?, ?, ?)", id, m.ProcessType, m.Path)
	if isUniqueViolation(err) {
		return &ExistsError{What: fmt.Sprintf("The mount %s in %s", m.Path, m.ProcessType)}
	}
	return err
}

func (s *Store) RemoveVolumeMount(ctx context.Context, id string, m types.VolumeMount) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM volume_mounts WHERE volume_id = ? AND process_type = ? AND path = ?", id, m.ProcessType, m.Path)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{What: fmt.Sprintf("The mount %s in %s", m.Path, m.ProcessType)}
	}
	return nil
}

// PlaceVolume records the node a new volume's disk is made on, the first
// time an instance uses it.
func (s *Store) PlaceVolume(ctx context.Context, id, node string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE volumes SET node = ? WHERE id = ? AND node = ''", node, id)
	return err
}

// StartVolumeMove begins copying a volume's disk to another node.
func (s *Store) StartVolumeMove(ctx context.Context, id, to, token string) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE volumes SET moving_to = ?, move_token = ?, transfer = '', copied_mb = 0, move_error = ''
WHERE id = ? AND moving_to = '' AND state != ?`, to, token, id, VolumeDestroying)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("the volume is already moving, or being destroyed")
	}
	return nil
}

// CommitVolumeMove makes the node the disk was copied to its home. The old
// node keeps its copy until the new one reports the disk.
func (s *Store) CommitVolumeMove(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE volumes SET node = moving_to, state = ?, status = '',
	previous_node = CASE WHEN restore != '' THEN '' ELSE node END,
	stale_node = CASE WHEN restore != '' AND node != '' THEN node ELSE stale_node END,
	discard_stale = CASE WHEN restore != '' AND node != '' THEN 0 ELSE discard_stale END,
	moving_to = '', move_token = '', transfer = '', copied_mb = 0, move_error = '', restore = '',
	restore_result = CASE WHEN restore != '' THEN 'restored' ELSE restore_result END
WHERE id = ? AND moving_to != ''`, VolumeReady, id)
	return err
}

// AbortVolumeMove calls off a move, or a restore onto another node (why
// is recorded as its result); the disk stays where it was.
func (s *Store) AbortVolumeMove(ctx context.Context, id, why string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE volumes SET moving_to = '', move_token = '', transfer = '', copied_mb = 0, move_error = '', restore = '',
	restore_result = CASE WHEN restore != '' THEN ? ELSE restore_result END
WHERE id = ?`, why, id)
	return err
}

// ReportVolume stores what a node said about a volume: the owner's disk
// status, the receiving node's copy progress, or that a destroyed volume's
// disk is gone. It returns whether that changes what anyone should do.
func (s *Store) ReportVolume(ctx context.Context, node string, st types.VolumeStatus) (bool, error) {
	v, err := s.VolumeByID(ctx, st.ID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch {
	case v.State == VolumeDestroying:
		// Gone once its disk is, and the old copy kept elsewhere, if any.
		switch {
		case v.Node == node && st.State == types.VolumeDestroyed:
			if v.StaleNode == "" {
				return true, s.DeleteVolume(ctx, v.ID)
			}
			_, err := s.db.ExecContext(ctx, "UPDATE volumes SET node = '' WHERE id = ?", v.ID)
			return true, err
		case v.StaleNode == node && st.State == types.VolumeDiscarded:
			if v.Node == "" {
				return true, s.DeleteVolume(ctx, v.ID)
			}
			return true, s.ForgetStaleCopy(ctx, v.ID)
		}
		return false, nil
	case v.MovingTo == node:
		changed := v.Transfer != st.State || v.MoveError != st.Error
		_, err := s.db.ExecContext(ctx, "UPDATE volumes SET transfer = ?, copied_mb = ?, move_error = ? WHERE id = ?",
			st.State, st.CopiedMB, st.Error, v.ID)
		return changed, err
	case v.StaleNode == node && v.Node != node:
		if st.State == types.VolumeDiscarded {
			return true, s.ForgetStaleCopy(ctx, v.ID)
		}
		return false, nil
	case v.Node == node:
		state, previous := v.State, v.PreviousNode
		if st.State == types.VolumeReady {
			state, previous = VolumeReady, "" // the old copy, if any, can go
		}
		changed := v.Status != st.State || state != v.State || previous != v.PreviousNode
		if _, err := s.db.ExecContext(ctx, "UPDATE volumes SET status = ?, used_mb = ?, state = ?, previous_node = ? WHERE id = ?",
			st.State, st.UsedMB, state, previous, v.ID); err != nil {
			return false, err
		}
		if v.Restore != "" && st.Restore != "" {
			// A restore on the disk's own node.
			changed = changed || v.Transfer != st.Restore || v.MoveError != st.Error
			_, err := s.db.ExecContext(ctx, "UPDATE volumes SET transfer = ?, copied_mb = ?, move_error = ? WHERE id = ? AND moving_to = ''",
				st.Restore, st.CopiedMB, st.Error, v.ID)
			return changed, err
		}
		return changed, nil
	}
	return false, nil
}
