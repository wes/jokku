package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wes/jokku/internal/backup"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// The cluster's own backups: the control node's database (a consistent
// snapshot) and identity, under <path>/state, and the root filesystems of
// recent releases, under <path>/artifacts, so a lost control node can be
// rebuilt from the bucket with its apps ready to start (jokku
// restore-cluster). They run on the control node, which has all of it.

// clusterLock is the backup lock key for the cluster's own backups.
const clusterLock = "cluster"

// artifactsKept is how many recent releases per app have their root
// filesystem backed up, as many as the control node keeps on disk.
const artifactsKept = 3

// DefaultClusterBackupPath is where in its bucket the cluster is backed up
// unless told otherwise.
const DefaultClusterBackupPath = "jokku/cluster"

// BackupCluster backs up the control node now, and records the backup.
func (c *Controller) BackupCluster(ctx context.Context, actor string, log func(string)) (*types.BackupResult, error) {
	set, err := c.Store.ClusterBackup(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errors.New("the cluster isn't backed up anywhere; set it up with: jokku backups:cluster <destination>")
	}
	if err != nil {
		return nil, err
	}
	dest, err := c.Store.BackupDestination(ctx, set.Destination)
	if err != nil {
		return nil, err
	}
	key, err := c.backupKey(ctx, dest)
	if err != nil {
		return nil, err
	}
	if c.DataDir == "" {
		return nil, errors.New("this control node doesn't know its data directory")
	}
	if _, busy := c.backups.LoadOrStore(clusterLock, true); busy {
		return nil, errors.New("a backup of the cluster is already running")
	}
	defer c.backups.Delete(clusterLock)

	prev, _ := c.Store.ClusterBackupRuns(ctx, 1)
	wasFailing := len(prev) > 0 && prev[0].Status == types.StatusFailed
	name := time.Now().UTC().Format(backup.NameFormat)
	if len(prev) > 0 && prev[0].Name >= name {
		last, _ := time.Parse(backup.NameFormat, prev[0].Name)
		name = last.Add(time.Second).Format(backup.NameFormat)
	}
	id, err := c.Store.StartClusterBackupRun(ctx, name, actor)
	if err != nil {
		return nil, err
	}
	res, err := c.backUpCluster(ctx, set, dest, key, name, log)
	run := store.BackupRun{ID: id, Status: types.StatusSucceeded}
	if err != nil {
		run.Status, run.Error = types.StatusFailed, err.Error()
		if actor != ScheduleActor || !wasFailing {
			c.Store.AddEvent(context.WithoutCancel(ctx), "backups", "", c.Self, "backing up the cluster failed: %v", err)
		}
	} else {
		run.SizeBytes, run.Blocks, run.NewBlocks, run.NewBytes = res.Size, res.Blocks, res.NewBlocks, res.NewBytes
		switch {
		case actor != ScheduleActor:
			c.Store.AddEvent(ctx, "backups", "", c.Self, "%s backed up the cluster as %s (%s new)", actor, res.Name, humanBytes(res.NewBytes))
		case wasFailing:
			c.Store.AddEvent(ctx, "backups", "", c.Self, "backing up the cluster works again")
		}
	}
	if ferr := c.Store.FinishClusterBackupRun(context.WithoutCancel(ctx), run); ferr != nil && err == nil {
		err = ferr
	}
	if err == nil {
		c.retainCluster(context.WithoutCancel(ctx), set, dest, key, res, log)
	}
	return res, err
}

func (c *Controller) backUpCluster(ctx context.Context, set *store.VolumeBackup, dest *types.BackupDestination, key, name string,
	log func(string)) (*types.BackupResult, error) {
	var k *backup.Key
	if key != "" {
		var err error
		if k, err = backup.ParseKey(key); err != nil {
			return nil, err
		}
	}
	st, err := backup.Open(*dest)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(c.DataDir, "cluster-backup-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	db := filepath.Join(tmp, "jokku.db")
	if err := c.Store.Snapshot(ctx, db); err != nil {
		return nil, fmt.Errorf("copying the database: %w", err)
	}
	tarPath := filepath.Join(tmp, "state.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		return nil, err
	}
	err = backup.PackState(c.DataDir, db, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	r, err := backup.Run(ctx, backup.Options{
		Store: st, Key: k, Path: set.Path + "/" + backup.StateDir, Volume: backup.StateVolume, VolumeName: "cluster",
		Disk: tarPath, Name: name, Staging: tmp, Log: log,
	})
	if err != nil {
		return nil, err
	}
	res := &types.BackupResult{Name: r.Name, Size: r.Size, Blocks: r.Blocks, NewBlocks: r.NewBlocks, NewBytes: r.NewBytes}
	log(fmt.Sprintf("Backed up the database and the control node's identity (%s new)", humanBytes(r.NewBytes)))

	// Each recent release's root filesystem, once.
	kept, err := c.keptArtifacts(ctx)
	if err != nil {
		return nil, err
	}
	apath := set.Path + "/" + backup.ArtifactsDir
	have, err := backup.List(ctx, st, apath)
	if err != nil {
		return nil, err
	}
	added := 0
	for _, a := range kept {
		n := artifactName(a)
		if slices.Contains(have, n) {
			continue
		}
		if _, err := os.Stat(a); err != nil {
			continue // not here (yet)
		}
		ar, err := backup.Run(ctx, backup.Options{
			Store: st, Key: k, Path: apath, Volume: backup.ArtifactsVolume, VolumeName: "artifacts",
			Disk: a, Name: n, Staging: tmp, Log: func(string) {},
		})
		if err != nil {
			return nil, fmt.Errorf("backing up the release %s: %w", n, err)
		}
		res.NewBlocks += ar.NewBlocks
		res.NewBytes += ar.NewBytes
		added++
	}
	if added > 0 {
		log(fmt.Sprintf("Backed up the root filesystems of %d releases", added))
	}
	return res, nil
}

// keptArtifacts lists the root filesystems the cluster backup holds.
func (c *Controller) keptArtifacts(ctx context.Context) ([]string, error) {
	kept, err := c.Store.KeptArtifacts(ctx, artifactsKept)
	if err != nil {
		return nil, err
	}
	var out []string
	for a := range kept {
		if a != "" {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out, nil
}

// artifactName is the name an artifact's backup goes by: its file's.
func artifactName(path string) string { return strings.TrimSuffix(filepath.Base(path), ".ext4") }

// retainCluster prunes the cluster's state backups by its retention, and
// now and then deletes the root filesystems of releases no longer kept and
// the blocks nothing uses.
func (c *Controller) retainCluster(ctx context.Context, set *store.VolumeBackup, dest *types.BackupDestination, key string,
	res *types.BackupResult, log func(string)) {
	warn := func(err error) {
		log("Warning: " + err.Error())
		c.Log.Warn("cluster backup retention", "err", err)
	}
	st, err := backup.Open(*dest)
	if err != nil {
		warn(err)
		return
	}
	statePath, apath := set.Path+"/"+backup.StateDir, set.Path+"/"+backup.ArtifactsDir
	deleted, left, err := backup.Prune(ctx, st, statePath, backup.Retention{Recent: set.KeepRecent, Daily: set.KeepDaily}, time.Now())
	if err != nil {
		warn(fmt.Errorf("removing old backups: %w", err))
		return
	}
	if len(deleted) > 0 {
		log(fmt.Sprintf("Removed %d old backups; %d kept", len(deleted), left))
	}
	c.Store.AddClusterBackupUsage(ctx, res.NewBytes, left)
	if time.Since(set.CollectedAt) < CollectEvery {
		return
	}
	kept, err := c.keptArtifacts(ctx)
	if err != nil {
		warn(err)
		return
	}
	keep := map[string]bool{}
	for _, a := range kept {
		keep[artifactName(a)] = true
	}
	have, err := backup.List(ctx, st, apath)
	if err != nil {
		warn(err)
		return
	}
	for _, n := range have {
		if !keep[n] {
			if err := backup.Delete(ctx, st, apath, n); err != nil {
				warn(err)
				return
			}
		}
	}
	var k *backup.Key
	if key != "" {
		if k, err = backup.ParseKey(key); err != nil {
			warn(err)
			return
		}
	}
	var total int64
	for _, p := range []string{statePath, apath} {
		u, n, freed, err := backup.Collect(ctx, st, k, p)
		if err != nil {
			warn(fmt.Errorf("deleting unused blocks: %w", err))
			return
		}
		if n > 0 {
			log(fmt.Sprintf("Deleted %d blocks nothing uses, %s", n, humanBytes(freed)))
		}
		total += u.Bytes
	}
	c.Store.SetClusterBackupUsage(ctx, total, left)
}

// ListClusterBackups lists the cluster's backups, newest first.
func (c *Controller) ListClusterBackups(ctx context.Context) (*types.VolumeBackups, error) {
	set, err := c.Store.ClusterBackup(ctx)
	if err != nil {
		return nil, err
	}
	dest, err := c.Store.BackupDestination(ctx, set.Destination)
	if err != nil {
		return nil, err
	}
	runs, err := c.Store.ClusterBackupRuns(ctx, 1000)
	if err != nil {
		return nil, err
	}
	status := BackupStatus(*set, runs)
	status.Cluster = true
	out := &status
	out.Backups = []types.BackupInfo{}
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
	names, err := backup.List(ctx, st, set.Path+"/"+backup.StateDir)
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
