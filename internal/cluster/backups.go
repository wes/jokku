package cluster

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/wes/jokku/internal/backup"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// Backups: the node holding a volume's disk backs it up straight to the
// bucket when asked (BackupVolume); the control node keeps the settings,
// the key and a record of each backup.
//
// A restore makes a new copy of the disk from a backup, much as a move
// does from the owner:
//
//   - On the disk's own node, while its node is up: the backup downloads
//     while the app runs; then the instances using the disk are stopped and
//     replaced, the restored copy takes the disk's place, and the
//     replacements start with it.
//   - Onto another node, when the disk's node is down or gone: the
//     instances using the disk are replaced there at once, and wait for the
//     download, which is then committed like a move. The old node's disk is
//     never deleted; it is set aside when the node comes back.

// restoreState is a restore in progress, as kept in the volume's record
// (credentials stay with the destination).
type restoreState struct {
	Destination string `json:"destination"`
	Path        string `json:"path"`
	Backup      string `json:"backup"`
	Swap        bool   `json:"swap,omitempty"`
}

func parseRestore(s string) restoreState {
	var r restoreState
	json.Unmarshal([]byte(s), &r)
	return r
}

func (r restoreState) String() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// restoreSpec is what the node making the disk needs to download it.
func (c *Controller) restoreSpec(ctx context.Context, v store.Volume) (*types.RestoreSpec, error) {
	r := parseRestore(v.Restore)
	dest, err := c.Store.BackupDestination(ctx, r.Destination)
	if err != nil {
		return nil, err
	}
	key, err := c.backupKey(ctx, dest)
	if err != nil {
		return nil, err
	}
	return &types.RestoreSpec{Destination: *dest, Path: r.Path, Backup: r.Backup, Key: key, Swap: r.Swap}, nil
}

// backupKey is the key a destination's backups are encrypted with, or ""
// for one that doesn't encrypt.
func (c *Controller) backupKey(ctx context.Context, dest *types.BackupDestination) (string, error) {
	if !dest.Encrypt {
		return "", nil
	}
	key, saved, err := c.Store.BackupKey(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", fmt.Errorf("%s encrypts backups, but this cluster has no backup key yet: run jokku backups:key", dest.Name)
	case err != nil:
		return "", err
	case !saved:
		return "", errors.New("encrypted backups wait until you have saved the backup key: run jokku backups:key")
	}
	return key, nil
}

// volumeBackup gathers what a backup or restore of an app's volume needs.
func (c *Controller) volumeBackup(ctx context.Context, app, name string) (*store.Volume, *store.VolumeBackup, *types.BackupDestination, error) {
	v, err := c.Store.Volume(ctx, app, name)
	if err != nil {
		return nil, nil, nil, err
	}
	if v.Type != types.VolumeLocal {
		return nil, nil, nil, fmt.Errorf("only local volumes are backed up")
	}
	set, err := c.Store.VolumeBackupFor(ctx, v.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, nil, fmt.Errorf("volume %s is not backed up anywhere; set it up with: jokku backups:set %s %s <destination>", name, app, name)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	dest, err := c.Store.BackupDestination(ctx, set.Destination)
	if err != nil {
		return nil, nil, nil, err
	}
	return v, set, dest, nil
}

// BackupVolume backs up an app's volume now, on the node that holds its
// disk, and records the backup. log is told the node's progress.
func (c *Controller) BackupVolume(ctx context.Context, app, name, actor string, log func(string)) (*types.BackupResult, error) {
	v, set, dest, err := c.volumeBackup(ctx, app, name)
	if err != nil {
		return nil, err
	}
	key, err := c.backupKey(ctx, dest)
	if err != nil {
		return nil, err
	}
	switch {
	case v.Node == "":
		return nil, fmt.Errorf("volume %s holds nothing yet: its disk is made when an instance first mounts it", name)
	case v.Restore != "":
		return nil, fmt.Errorf("volume %s is being restored; back it up once that is done", name)
	case v.Moving():
		return nil, fmt.Errorf("volume %s is moving to %s; back it up once it is there", name, v.MovingTo)
	}
	node, err := c.Store.Node(ctx, v.Node)
	if err != nil {
		return nil, err
	}
	if !node.Ready(time.Now()) {
		return nil, fmt.Errorf("volume %s is on %s, which is down", name, v.Node)
	}
	if _, busy := c.backups.LoadOrStore(v.ID, true); busy {
		return nil, fmt.Errorf("a backup of volume %s is already running", name)
	}
	defer c.backups.Delete(v.ID)

	job := types.BackupJob{Volume: v.ID, App: app, Name: name, Destination: *dest, Path: set.Path, Key: key}
	insts, err := c.Store.Instances(ctx, app)
	if err != nil {
		return nil, err
	}
	for _, in := range insts {
		if in.Node != v.Node || in.Desired != store.DesiredRunning || (in.State != store.StateHealthy && in.State != store.StateStarting) {
			continue
		}
		for _, m := range in.Volumes {
			if m.ID == v.ID {
				job.Instance, job.MountPath = in.ID, m.Path
			}
		}
	}
	job.Backup = time.Now().UTC().Format(backup.NameFormat)
	if runs, _ := c.Store.BackupRuns(ctx, v.ID, 1); len(runs) > 0 && runs[0].Name >= job.Backup {
		last, _ := time.Parse(backup.NameFormat, runs[0].Name)
		job.Backup = last.Add(time.Second).Format(backup.NameFormat)
	}

	id, err := c.Store.StartBackupRun(ctx, v.ID, job.Backup, actor)
	if err != nil {
		return nil, err
	}
	res, err := c.runBackup(ctx, *node, job, log)
	run := store.BackupRun{ID: id, Status: types.StatusSucceeded}
	if err != nil {
		run.Status, run.Error = types.StatusFailed, err.Error()
		c.Store.AddEvent(context.WithoutCancel(ctx), "volume", app, v.Node, "backing up volume %s failed: %v", name, err)
	} else {
		run.SizeBytes, run.Blocks, run.NewBlocks, run.NewBytes = res.Size, res.Blocks, res.NewBlocks, res.NewBytes
		c.Store.AddEvent(ctx, "volume", app, v.Node, "%s backed up volume %s as %s (%s new)", actor, name, res.Name, humanBytes(res.NewBytes))
	}
	if ferr := c.Store.FinishBackupRun(context.WithoutCancel(ctx), run); ferr != nil && err == nil {
		err = ferr
	}
	return res, err
}

// runBackup asks a node to run a backup job and follows its progress.
func (c *Controller) runBackup(ctx context.Context, node store.Node, job types.BackupJob, log func(string)) (*types.BackupResult, error) {
	body, _ := json.Marshal(job)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.agentAddr(node)+"/v1/volumes/"+job.Volume+"/backup", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+node.AgentToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := agentClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching %s: %w", node.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("%s: %s", node.Name, strings.TrimSpace(string(b)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e types.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("%s sent a malformed event: %w", node.Name, err)
		}
		switch {
		case e.Type == types.EventLog:
			log(e.Message)
		case e.Type == types.EventDone && e.Status == types.StatusSucceeded && e.Backup != nil:
			return e.Backup, nil
		case e.Type == types.EventDone:
			return nil, errors.New(e.Error)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%s stopped answering before the backup finished", node.Name)
}

// ListBackups lists the backups of an app's volume, newest first, from its
// bucket, with what this cluster recorded about them.
func (c *Controller) ListBackups(ctx context.Context, app, name string) (*types.VolumeBackups, error) {
	v, set, dest, err := c.volumeBackup(ctx, app, name)
	if err != nil {
		return nil, err
	}
	out := &types.VolumeBackups{App: app, Volume: name, Destination: set.Destination, Path: set.Path, Backups: []types.BackupInfo{}}
	runs, err := c.Store.BackupRuns(ctx, v.ID, 1000)
	if err != nil {
		return nil, err
	}
	if len(runs) > 0 {
		r := runs[0]
		out.Last = &types.BackupRun{Name: r.Name, Status: r.Status, Error: r.Error, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
	}
	made := map[string]store.BackupRun{}
	for _, r := range runs {
		if r.Status == types.StatusSucceeded {
			made[r.Name] = r
		}
	}
	st, err := backup.Open(*dest)
	if err != nil {
		return nil, err
	}
	names, err := backup.List(ctx, st, set.Path)
	if err != nil {
		return nil, fmt.Errorf("listing %s in %s: %w", set.Path, dest.Name, err)
	}
	for i := len(names) - 1; i >= 0; i-- {
		b := types.BackupInfo{Name: names[i]}
		b.Time, _ = time.Parse(backup.NameFormat, names[i])
		if r, ok := made[names[i]]; ok {
			b.Size, b.NewBytes, b.NewBlocks = r.SizeBytes, r.NewBytes, r.NewBlocks
		}
		out.Backups = append(out.Backups, b)
	}
	return out, nil
}

// RestoreVolume starts restoring an app's volume from one of its backups
// (the latest for name ""): on the disk's own node, or onto node when the
// disk's node is down or gone. It returns the backup and the node.
func (c *Controller) RestoreVolume(ctx context.Context, app, name, backupName, node, actor string) (string, string, error) {
	c.volMu.Lock()
	defer c.volMu.Unlock()
	v, set, dest, err := c.volumeBackup(ctx, app, name)
	if err != nil {
		return "", "", err
	}
	switch {
	case v.Restore != "":
		return "", "", fmt.Errorf("volume %s is already being restored", name)
	case v.Moving():
		return "", "", fmt.Errorf("volume %s is moving to %s; restore it once it is there", name, v.MovingTo)
	}
	key, err := c.backupKey(ctx, dest)
	if err != nil {
		return "", "", err
	}
	st, err := backup.Open(*dest)
	if err != nil {
		return "", "", err
	}
	names, err := backup.List(ctx, st, set.Path)
	if err != nil {
		return "", "", fmt.Errorf("listing %s in %s: %w", set.Path, dest.Name, err)
	}
	switch {
	case len(names) == 0:
		return "", "", fmt.Errorf("volume %s has no backups in %s %s yet", name, dest.Name, set.Path)
	case backupName == "":
		backupName = names[len(names)-1]
	}
	var k *backup.Key
	if key != "" {
		if k, err = backup.ParseKey(key); err != nil {
			return "", "", err
		}
	}
	if _, err := backup.ReadManifest(ctx, st, k, set.Path, backupName); err != nil {
		return "", "", err
	}
	state := restoreState{Destination: dest.Name, Path: set.Path, Backup: backupName}
	b := make([]byte, 24)
	rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)

	now := time.Now()
	var src *store.Node
	if v.Node != "" {
		src, _ = c.Store.Node(ctx, v.Node)
	}
	srcUp := src != nil && src.Ready(now)
	if node == "" && srcUp {
		node = v.Node
	}
	if node == v.Node && node != "" {
		if !srcUp {
			return "", "", fmt.Errorf("volume %s is on %s, which is down: restore it onto another node with --node", name, v.Node)
		}
		if err := c.Store.StartVolumeRestore(ctx, v.ID, "", token, state.String()); err != nil {
			return "", "", err
		}
		c.Store.AddEvent(ctx, "volume", app, v.Node, "%s is restoring volume %s from backup %s", actor, name, backupName)
		c.Changed()
		return backupName, node, nil
	}
	if srcUp {
		return "", "", fmt.Errorf("volume %s is on %s, which is up: restore it there (without --node), then move it with storage:move", name, v.Node)
	}

	// Onto another node: the instances using the volume go there with it.
	insts, err := c.Store.Instances(ctx, app)
	if err != nil {
		return "", "", err
	}
	vols, err := c.Store.Volumes(ctx, app)
	if err != nil {
		return "", "", err
	}
	var using []store.Instance
	cpus, memory := 0, 0
	for _, in := range insts {
		if in.Desired != store.DesiredRunning || !usesVolume(in, v.ID) {
			continue
		}
		for _, o := range pinnedVolumes(in, vols) {
			if o.ID != v.ID {
				return "", "", fmt.Errorf("%s also mounts volume %s, which is on %s too: restore that one first", in.Name(), o.Name, o.Node)
			}
		}
		using = append(using, in)
		cpus, memory = max(cpus, in.CPUs), max(memory, in.MemoryMB)
	}
	p := Placement{App: app, Exclude: v.Node, Volumes: true, DiskMB: v.UsedMB, CPUs: cpus, MemoryMB: memory}
	if len(using) > 0 {
		p.ProcessType = using[0].ProcessType
	}
	if node != "" {
		p.Pin, p.PinReason = node, "the restore goes to "+node
	}
	dest2, subnet, err := c.Place(ctx, p)
	if err != nil {
		return "", "", err
	}
	if err := c.Store.StartVolumeRestore(ctx, v.ID, dest2, token, state.String()); err != nil {
		return "", "", err
	}
	for _, in := range using {
		if err := c.replaceOn(ctx, in, dest2, subnet); err != nil {
			return "", "", err
		}
	}
	from := "nowhere yet"
	if v.Node != "" {
		from = v.Node + ", which is down"
	}
	c.Store.AddEvent(ctx, "volume", app, dest2, "%s is restoring volume %s from backup %s onto %s (it was on %s)", actor, name, backupName, dest2, from)
	c.Changed()
	return backupName, dest2, nil
}

// replaceOn stops an instance and starts a replacement for it on node, which
// waits there until its volumes are ready.
func (c *Controller) replaceOn(ctx context.Context, in store.Instance, node string, subnet netip.Prefix) error {
	if err := c.Store.StopInstances(ctx, []string{in.ID}, nil); err != nil {
		return err
	}
	repl := store.Instance{
		App: in.App, ReleaseID: in.ReleaseID, ProcessType: in.ProcessType, Index: in.Index, Node: node,
		Port: in.Port, CPUs: in.CPUs, MemoryMB: in.MemoryMB, Desired: store.DesiredRunning, Replaces: in.ID,
		Volumes: in.Volumes,
	}
	return c.Store.CreateInstance(ctx, &repl, subnet)
}

// tickRestore advances a restore on what the nodes reported.
func (c *Controller) tickRestore(ctx context.Context, v store.Volume, insts []store.Instance, byID map[string]store.Instance,
	byName map[string]store.Node, handled map[string]bool, now time.Time) (bool, error) {
	r := parseRestore(v.Restore)
	src, srcOK := byName[v.Node]
	if v.Moving() {
		dest, destOK := byName[v.MovingTo]
		why := ""
		switch {
		case !destOK:
			why = v.MovingTo + " was removed"
		case !dest.Ready(now):
			why = v.MovingTo + " is down"
		case v.Transfer == types.VolumeFailed:
			why = "the backup could not be downloaded: " + v.MoveError
		case v.Transfer == types.VolumeReceived:
			if err := c.Store.CommitVolumeMove(ctx, v.ID); err != nil {
				return false, err
			}
			c.Store.AddEvent(ctx, "volume", v.App, v.MovingTo, "volume %s restored from backup %s onto %s", v.Name, r.Backup, v.MovingTo)
			return true, nil
		default:
			return false, nil
		}
		return true, c.abortMove(ctx, v, why, insts, byID, byName, handled)
	}

	switch {
	case !srcOK || v.Transfer == types.VolumeFailed:
		why := v.Node + " was removed"
		if srcOK {
			why = "the backup could not be downloaded: " + v.MoveError
		}
		if err := c.Store.EndVolumeRestore(ctx, v.ID, why); err != nil {
			return false, err
		}
		c.Store.AddEvent(ctx, "volume", v.App, v.Node, "restoring volume %s from backup %s failed, so it keeps its data: %s", v.Name, r.Backup, why)
		return true, nil
	case v.Transfer == types.VolumeRestored:
		if err := c.Store.EndVolumeRestore(ctx, v.ID, "restored"); err != nil {
			return false, err
		}
		c.Store.AddEvent(ctx, "volume", v.App, v.Node, "volume %s restored from backup %s", v.Name, r.Backup)
		return true, nil
	case v.Transfer == types.VolumeReceived && !r.Swap:
		// Downloaded: replace the instances using the disk, so the restored
		// copy can take its place. Their replacements wait for it.
		subnet, _ := NodeSubnet(c.ClusterCIDR, src.SubnetIndex)
		for _, in := range insts {
			if in.Node != v.Node || in.Desired != store.DesiredRunning || !usesVolume(in, v.ID) || handled[in.ID] {
				continue
			}
			handled[in.ID] = true
			if err := c.replaceOn(ctx, in, v.Node, subnet); err != nil {
				return false, err
			}
			c.Store.AddEvent(ctx, "instance", in.App, in.Node, "restarting %s with volume %s restored from backup %s", in.Name(), v.Name, r.Backup)
		}
		r.Swap = true
		return true, c.Store.SetVolumeRestore(ctx, v.ID, r.String())
	}
	return false, nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
