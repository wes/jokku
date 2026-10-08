package agent

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/volume"
)

// A local volume's disk is <data>/volumes/<id>.ext4 on the node that owns
// it. A move leaves other files on the way:
//
//	<id>.ext4.incoming  the receiving node's copy while it is being made
//	<id>.ext4.received  its finished copy, until the control node commits the move
//	<id>.ext4.moved     the old owner's disk after the final pass handed it over
//
// Those are deleted once the control node stops listing the volume for this
// node. The disk itself is deleted only on an explicit destroy.
const (
	sufIncoming = ".incoming"
	sufReceived = ".received"
	sufMoved    = ".moved"
	sufNew      = ".new" // being created
)

// finalPassAfter is how little a copy pass (made while the app runs) must
// send before the app is stopped for the final pass: the less, the shorter
// the downtime.
const finalPassAfter = 64 << 20

// vol is what this node knows about one of its volumes.
type vol struct {
	role   string
	state  string // a types.Volume* state; "" reports nothing
	usedMB int
	copied int64 // bytes, in the current copy pass
	err    string
	// A copy from another node in progress, and the move it belongs to.
	cancel context.CancelFunc
	token  string
}

func (a *Agent) volumePath(id, suffix string) string {
	return filepath.Join(a.DataDir, "volumes", id+".ext4"+suffix)
}

func (a *Agent) vol(id string) *vol {
	a.volMu.Lock()
	defer a.volMu.Unlock()
	v := a.vols[id]
	if v == nil {
		v = &vol{}
		a.vols[id] = v
	}
	return v
}

func (a *Agent) setVol(id string, fn func(*vol)) {
	a.volMu.Lock()
	defer a.volMu.Unlock()
	if v := a.vols[id]; v != nil {
		fn(v)
	}
}

// syncVolumes makes this node's disks match the volumes it is told about:
// it creates new ones, finishes or calls off moves, copies incoming ones,
// deletes destroyed ones and cleans up what moves left behind.
func (a *Agent) syncVolumes(ctx context.Context, st *types.NodeState, attached map[string]bool) {
	if len(st.Volumes) > 0 {
		os.MkdirAll(filepath.Join(a.DataDir, "volumes"), 0o700)
	}
	listed := map[string]bool{}
	for _, spec := range st.Volumes {
		listed[spec.ID] = true
		v := a.vol(spec.ID)
		a.setVol(spec.ID, func(v *vol) { v.role = spec.Role })
		var err error
		switch spec.Role {
		case types.VolumeOwner:
			a.stopCopy(spec.ID)
			err = a.own(ctx, spec, attached)
		case types.VolumeIncoming:
			a.receive(spec, v)
		case types.VolumePrevious:
			a.stopCopy(spec.ID)
			a.setVol(spec.ID, func(v *vol) { v.state = "" })
		case types.VolumeDestroy:
			a.stopCopy(spec.ID)
			err = a.destroyVolume(spec, attached)
		}
		if err != nil {
			a.logOnce("volume "+spec.ID, err)
			a.setVol(spec.ID, func(v *vol) { v.err = err.Error() })
		} else if spec.Role != types.VolumeIncoming {
			a.clearErr("volume " + spec.ID)
			a.setVol(spec.ID, func(v *vol) { v.err = "" })
		}
	}

	// What moves left behind for volumes this node no longer holds. A disk
	// is kept even then: only an explicit destroy deletes data.
	entries, _ := os.ReadDir(filepath.Join(a.DataDir, "volumes"))
	for _, e := range entries {
		id, suffix, ok := strings.Cut(e.Name(), ".ext4")
		if !ok || listed[id] {
			continue
		}
		switch suffix {
		case sufIncoming, sufReceived, sufMoved, sufNew:
			os.Remove(filepath.Join(a.DataDir, "volumes", e.Name()))
		case "":
			a.logOnce("volume "+id, fmt.Errorf("keeping %s: the control node no longer knows the volume", e.Name()))
		}
	}
	a.volMu.Lock()
	for id, v := range a.vols {
		if !listed[id] {
			if v.cancel != nil {
				v.cancel()
			}
			delete(a.vols, id)
		}
	}
	a.volMu.Unlock()
}

// own keeps the disk of a volume this node owns.
func (a *Agent) own(ctx context.Context, spec types.VolumeSpec, attached map[string]bool) error {
	path := a.volumePath(spec.ID, "")
	if !exists(path) {
		switch {
		case exists(a.volumePath(spec.ID, sufReceived)):
			// A move to this node is done.
			if err := os.Rename(a.volumePath(spec.ID, sufReceived), path); err != nil {
				return err
			}
			a.Log.Info("volume moved here", "app", spec.App, "volume", spec.Name)
		case exists(a.volumePath(spec.ID, sufMoved)) && spec.Token != "":
			// Handed to the receiving node; waiting for the control node to
			// commit the move.
			a.setVol(spec.ID, func(v *vol) { v.state = types.VolumeMoved })
			return nil
		case exists(a.volumePath(spec.ID, sufMoved)):
			// The move was called off after the hand-over: take it back.
			if err := os.Rename(a.volumePath(spec.ID, sufMoved), path); err != nil {
				return err
			}
			a.Log.Info("volume move called off; keeping the disk", "app", spec.App, "volume", spec.Name)
		case spec.Create:
			if err := a.Runtime.CreateVolume(ctx, path, spec.SizeMB); err != nil {
				return fmt.Errorf("creating volume %s of %s: %w", spec.Name, spec.App, err)
			}
			a.Log.Info("created volume", "app", spec.App, "volume", spec.Name, "size_mb", spec.SizeMB)
		default:
			a.setVol(spec.ID, func(v *vol) { v.state = types.VolumeMissing })
			return fmt.Errorf("the disk of volume %s of %s is missing from %s", spec.Name, spec.App, path)
		}
	}
	if spec.Token == "" {
		// Not moving: older copies from earlier moves are stale.
		for _, s := range []string{sufIncoming, sufReceived, sufMoved} {
			os.Remove(a.volumePath(spec.ID, s))
		}
	}
	if info, err := os.Stat(path); err == nil && info.Size() < int64(spec.SizeMB)<<20 && !attached[path] {
		if _, busy := a.exporting.Load(spec.ID); !busy {
			if err := a.Runtime.GrowVolume(ctx, path, spec.SizeMB); err != nil {
				return fmt.Errorf("growing volume %s of %s: %w", spec.Name, spec.App, err)
			}
			a.Log.Info("grew volume", "app", spec.App, "volume", spec.Name, "size_mb", spec.SizeMB)
		}
	}
	used := allocatedMB(path)
	a.setVol(spec.ID, func(v *vol) { v.state, v.usedMB = types.VolumeReady, used })
	return nil
}

// receive copies a volume that is moving to this node, in the background.
func (a *Agent) receive(spec types.VolumeSpec, v *vol) {
	if exists(a.volumePath(spec.ID, sufReceived)) {
		a.setVol(spec.ID, func(v *vol) { v.state = types.VolumeReceived })
		return
	}
	a.volMu.Lock()
	defer a.volMu.Unlock()
	if v.cancel != nil && v.token == spec.Token {
		return // already copying
	}
	if v.cancel != nil {
		v.cancel() // a different move
	}
	ctx, cancel := context.WithCancel(context.Background())
	v.cancel, v.token, v.state, v.copied, v.err = cancel, spec.Token, types.VolumeCopying, 0, ""
	go a.copyVolume(ctx, spec)
}

func (a *Agent) stopCopy(id string) {
	a.setVol(id, func(v *vol) {
		if v.cancel != nil {
			v.cancel()
			v.cancel, v.token = nil, ""
		}
	})
}

// copyVolume pulls a moving volume's disk from its owner: passes while the
// app still runs there until little changes between them, then, once the
// control node has stopped the app (the owner answers busy until then), the
// final pass. The finished copy waits as .received for the move to be
// committed.
func (a *Agent) copyVolume(ctx context.Context, spec types.VolumeSpec) {
	part := a.volumePath(spec.ID, sufIncoming)
	p := &volume.Puller{
		Client: a.volumeClient, Token: spec.Token, Path: part,
		URL:      "http://" + spec.From + "/v1/volumes/" + spec.ID + "/copy",
		Progress: func(n int64) { a.setVol(spec.ID, func(v *vol) { v.copied = n }) },
	}
	what := "copying volume " + spec.ID
	synced, passes, backoff := false, 0, time.Second
	for ctx.Err() == nil {
		st, err := p.Pull(ctx, synced)
		switch {
		case errors.Is(err, volume.ErrBusy):
			sleep(ctx, time.Second)
			continue
		case err != nil:
			if ctx.Err() == nil {
				a.logOnce(what, fmt.Errorf("copying volume %s of %s from %s: %w", spec.Name, spec.App, spec.From, err))
				a.setVol(spec.ID, func(v *vol) { v.err = err.Error() })
				a.reportSoon()
			}
			sleep(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		a.clearErr(what)
		backoff = time.Second
		if !synced {
			passes++
			if st.Bytes > finalPassAfter && passes < 5 {
				continue
			}
			synced = true
			a.setVol(spec.ID, func(v *vol) { v.state, v.err = types.VolumeSynced, "" })
			a.reportSoon()
			continue
		}
		if err := os.Rename(part, a.volumePath(spec.ID, sufReceived)); err != nil {
			a.logOnce(what, err)
			sleep(ctx, backoff)
			continue
		}
		a.Log.Info("received volume", "app", spec.App, "volume", spec.Name, "from", spec.From)
		a.setVol(spec.ID, func(v *vol) { v.state, v.err = types.VolumeReceived, "" })
		a.reportSoon()
		a.Kick()
		return
	}
}

// destroyVolume deletes a destroyed volume's disk, once no VM has it.
func (a *Agent) destroyVolume(spec types.VolumeSpec, attached map[string]bool) error {
	path := a.volumePath(spec.ID, "")
	if attached[path] {
		return nil // its VM is stopping
	}
	for _, s := range []string{"", sufIncoming, sufReceived, sufMoved, sufNew} {
		if err := os.Remove(a.volumePath(spec.ID, s)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	a.setVol(spec.ID, func(v *vol) { v.state = types.VolumeDestroyed })
	return nil
}

// volumeGate says whether an instance's volumes let it start: "" when they
// do, "syncing" while one is still being copied here, "pending" while
// another VM has one attached or it is being handed to another node, or an
// error when one is gone.
func (a *Agent) volumeGate(spec types.InstanceSpec, attached map[string]bool) (string, error) {
	for _, m := range spec.Volumes {
		a.volMu.Lock()
		v, ok := a.vols[m.ID]
		var role, state string
		if ok {
			role, state = v.role, v.state
		}
		a.volMu.Unlock()
		switch {
		case !ok:
			return "", fmt.Errorf("volume %s is not on this node", m.ID)
		case role == types.VolumeIncoming:
			return "syncing", nil
		case state == types.VolumeMissing:
			return "", fmt.Errorf("the disk of volume %s is missing", m.ID)
		case state != types.VolumeReady:
			return "pending", nil
		}
		if _, busy := a.exporting.Load(m.ID); busy || attached[a.volumePath(m.ID, "")] {
			return "pending", nil
		}
	}
	return "", nil
}

// serveVolume sends a volume's disk to the node it is moving to, which
// presents the move's token. A final pass is refused while a VM has the disk
// attached; once sent, the disk is the receiver's (renamed .moved) and no
// instance here can start with it.
func (a *Agent) serveVolume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var spec *types.VolumeSpec
	if st := a.Desired(); st != nil {
		for _, s := range st.Volumes {
			if s.ID == id && s.Role == types.VolumeOwner && s.Token != "" {
				spec = &s
			}
		}
	}
	if spec == nil {
		http.Error(w, "this node is not moving that volume", http.StatusNotFound)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+spec.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	path := a.volumePath(id, "")
	final := r.URL.Query().Get("final") == "1"
	if final {
		if _, busy := a.exporting.LoadOrStore(id, struct{}{}); busy {
			http.Error(w, "a final pass is already running", http.StatusConflict)
			return
		}
		defer a.exporting.Delete(id)
		// Under the reconcile lock: no VM can start with the disk between
		// this check and the end of the copy.
		a.mu.Lock()
		attached, err := a.Runtime.Attached(r.Context())
		a.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if attached[path] {
			http.Error(w, volume.ErrBusy.Error(), http.StatusConflict)
			return
		}
	}
	have, err := volume.ReadSummary(http.MaxBytesReader(w, r.Body, 1<<30))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		f, err = os.Open(a.volumePath(id, sufMoved)) // a pass repeated after the hand-over
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	http.NewResponseController(w).Flush() // reading the disk may take a while before anything differs
	st, err := volume.Send(w, have, f)
	if err != nil {
		a.Log.Error("sending volume", "volume", id, "err", err)
		return
	}
	a.Log.Info("sent volume", "app", spec.App, "volume", spec.Name, "final", final, "blocks", st.Blocks, "bytes", st.Bytes)
	if final && exists(path) {
		if err := os.Rename(path, a.volumePath(id, sufMoved)); err != nil {
			a.Log.Error("handing over volume", "volume", id, "err", err)
		}
		a.Kick()
	}
}

// volumeStatus is what the node reports about its volumes.
func (a *Agent) volumeStatus() []types.VolumeStatus {
	a.volMu.Lock()
	defer a.volMu.Unlock()
	var out []types.VolumeStatus
	for id, v := range a.vols {
		if v.state == "" {
			continue
		}
		out = append(out, types.VolumeStatus{
			ID: id, State: v.state, UsedMB: v.usedMB, CopiedMB: int(v.copied >> 20), Error: v.err,
		})
	}
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
