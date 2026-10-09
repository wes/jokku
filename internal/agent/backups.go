package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wes/jokku/internal/backup"
	"github.com/wes/jokku/internal/session"
	"github.com/wes/jokku/internal/types"
)

// serveBackup backs up a volume whose disk is on this node, as the control
// node asks: blocks go straight from here to the bucket. Progress streams
// back as events; the last says what was stored.
func (a *Agent) serveBackup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var job types.BackupJob
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&job); err != nil || job.Volume != id {
		http.Error(w, "invalid backup job", http.StatusBadRequest)
		return
	}
	path := a.volumePath(id, "")
	if !exists(path) {
		http.Error(w, "the disk of volume "+job.Name+" is not on this node", http.StatusNotFound)
		return
	}
	if _, busy := a.backingUp.LoadOrStore(id, struct{}{}); busy {
		http.Error(w, "a backup of volume "+job.Name+" is already running", http.StatusConflict)
		return
	}
	// The locks go before the last event: once the control node hears the
	// backup is done, it may start the next.
	release := func() { a.backingUp.Delete(id) }
	defer func() { release() }()
	if job.Instance == "" {
		// Nothing should write to the disk: keep it that way (instances
		// wait to start) while it is read.
		if _, busy := a.exporting.LoadOrStore(id, struct{}{}); busy {
			http.Error(w, "volume "+job.Name+" is being handed to another node", http.StatusConflict)
			return
		}
		release = func() { a.exporting.Delete(id); a.backingUp.Delete(id) }
		a.mu.Lock()
		attached, err := a.Runtime.Attached(r.Context())
		a.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if attached[path] {
			http.Error(w, "volume "+job.Name+" is attached to a running instance", http.StatusConflict)
			return
		}
	}

	var mu sync.Mutex
	enc := json.NewEncoder(w)
	rc := http.NewResponseController(w)
	send := func(e types.Event) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(e)
		rc.Flush()
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc.Flush() // the first progress line may be a while
	res, err := a.backUp(r.Context(), job, path, func(line string) { send(types.Event{Type: types.EventLog, Message: line}) })
	release()
	release = func() {}
	if err != nil {
		a.Log.Error("backup failed", "app", job.App, "volume", job.Name, "err", err)
		send(types.Event{Type: types.EventDone, Status: types.StatusFailed, Error: err.Error()})
		return
	}
	a.Log.Info("backed up volume", "app", job.App, "volume", job.Name, "backup", res.Name, "new_bytes", res.NewBytes)
	send(types.Event{Type: types.EventDone, Status: types.StatusSucceeded, Backup: &types.BackupResult{
		Name: res.Name, Size: res.Size, Blocks: res.Blocks, NewBlocks: res.NewBlocks, NewBytes: res.NewBytes,
	}})
}

func (a *Agent) backUp(ctx context.Context, job types.BackupJob, path string, log func(string)) (*backup.Result, error) {
	var key *backup.Key
	if job.Key != "" {
		k, err := backup.ParseKey(job.Key)
		if err != nil {
			return nil, err
		}
		key = k
	}
	st, err := backup.Open(job.Destination)
	if err != nil {
		return nil, err
	}
	o := backup.Options{
		Store: st, Key: key, Path: job.Path, Volume: job.Volume, App: job.App, VolumeName: job.Name,
		Disk: path, Name: job.Backup, Staging: filepath.Dir(path), Log: log,
	}
	if job.Instance != "" {
		o.Freeze = func(ctx context.Context) (func() error, error) { return a.freeze(ctx, job.Instance, job.MountPath) }
	}
	return backup.Run(ctx, o)
}

// freeze pauses writes to a volume inside a running VM, through its guest
// agent, until the returned thaw is called.
func (a *Agent) freeze(ctx context.Context, instance, mountPath string) (func() error, error) {
	conn, token, err := a.Runtime.Session(ctx, instance)
	if err != nil {
		return nil, err
	}
	s := session.New(conn)
	if err := s.SendRequest(session.Request{Op: session.OpFreeze, Path: mountPath, Token: token}); err != nil {
		conn.Close()
		return nil, err
	}
	for frozen := false; !frozen; {
		typ, p, err := s.Read()
		switch {
		case err != nil:
			conn.Close()
			return nil, err
		case typ == session.FrameExit:
			conn.Close()
			_, msg := session.ParseExit(p)
			return nil, errors.New(msg)
		case typ == session.FrameStdout:
			frozen = strings.TrimSpace(string(p)) == "frozen"
		}
	}
	return func() error {
		defer conn.Close()
		if err := s.Write(session.FrameEOF, nil); err != nil {
			return err
		}
		for {
			typ, p, err := s.Read()
			if err != nil {
				return err
			}
			if typ == session.FrameExit {
				if code, msg := session.ParseExit(p); code != 0 {
					return errors.New(msg)
				}
				return nil
			}
		}
	}, nil
}

// restoreTries is how often a restore's download is tried before the node
// reports it failed.
const restoreTries = 5

// restoreVolume downloads a backup into a new copy of a volume's disk
// (.incoming, then .received when complete). The control node then makes
// it the disk: on another node by committing the restore, as for a move;
// on this one by letting it replace the disk once nothing uses it.
func (a *Agent) restoreVolume(ctx context.Context, spec types.VolumeSpec) {
	part := a.volumePath(spec.ID, sufIncoming)
	r := spec.Restore
	what := "restoring volume " + spec.ID
	report := func(state, msg string) {
		a.setVol(spec.ID, func(v *vol) {
			v.restore, v.err = state, msg
			if v.role == types.VolumeIncoming {
				v.state = state
			}
		})
		a.reportSoon()
	}
	backoff := time.Second
	for try := 1; ctx.Err() == nil; try++ {
		err := func() error {
			var key *backup.Key
			if r.Key != "" {
				k, err := backup.ParseKey(r.Key)
				if err != nil {
					return err
				}
				key = k
			}
			st, err := backup.Open(r.Destination)
			if err != nil {
				return err
			}
			_, err = backup.Restore(ctx, st, key, r.Path, r.Backup, part, func(done, _ int64) {
				a.setVol(spec.ID, func(v *vol) { v.copied = done })
			})
			if err != nil {
				return err
			}
			return os.Rename(part, a.volumePath(spec.ID, sufReceived))
		}()
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			a.clearErr(what)
			a.Log.Info("restored volume from a backup", "app", spec.App, "volume", spec.Name, "backup", r.Backup)
			report(types.VolumeReceived, "")
			a.Kick()
			return
		case try >= restoreTries:
			a.logOnce(what, fmt.Errorf("restoring volume %s of %s from backup %s: %w", spec.Name, spec.App, r.Backup, err))
			os.Remove(part)
			report(types.VolumeFailed, err.Error())
			return
		}
		a.setVol(spec.ID, func(v *vol) { v.err = err.Error() })
		sleep(ctx, backoff)
		backoff = min(backoff*2, 30*time.Second)
	}
}

// startRestore downloads a restore's backup in the background, once per
// restore (its token).
func (a *Agent) startRestore(spec types.VolumeSpec) {
	a.volMu.Lock()
	defer a.volMu.Unlock()
	v := a.vols[spec.ID]
	if v == nil || (v.token == spec.Token && (v.cancel != nil || v.restore != "")) {
		return // running, or done
	}
	if v.cancel != nil {
		v.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	v.cancel, v.token, v.restore, v.copied, v.err = cancel, spec.Token, types.VolumeCopying, 0, ""
	if v.role == types.VolumeIncoming {
		v.state = types.VolumeCopying
	}
	go a.restoreVolume(ctx, spec)
}

// restoreHere advances a restore on the disk's own node: download the
// backup while the app still runs; once the control node has stopped the
// instances using the disk (Swap) and their VMs let go of it, put the
// restored copy in its place.
func (a *Agent) restoreHere(spec types.VolumeSpec, attached map[string]bool) error {
	a.volMu.Lock()
	v := a.vols[spec.ID]
	state, token := v.restore, v.token
	a.volMu.Unlock()
	if token == spec.Token && (state == types.VolumeRestored || state == types.VolumeFailed) {
		return nil
	}
	received := a.volumePath(spec.ID, sufReceived)
	if !exists(received) {
		a.startRestore(spec)
		return nil
	}
	a.setVol(spec.ID, func(v *vol) {
		if v.token != spec.Token {
			v.token, v.cancel = spec.Token, nil
		}
		v.restore = types.VolumeReceived
	})
	path := a.volumePath(spec.ID, "")
	if !spec.Restore.Swap || attached[path] {
		return nil
	}
	if _, busy := a.backingUp.Load(spec.ID); busy {
		return nil
	}
	if err := os.Rename(received, path); err != nil {
		return err
	}
	a.Log.Info("replaced a volume's disk with its restored copy", "app", spec.App, "volume", spec.Name, "backup", spec.Restore.Backup)
	a.setVol(spec.ID, func(v *vol) { v.restore = types.VolumeRestored })
	a.reportSoon()
	return nil
}
