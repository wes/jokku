package cluster

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// ScheduleActor is who scheduled backups are made by, in their records.
const ScheduleActor = "schedule"

// Scheduled backups: the controller loop starts each volume's backup when
// it is due, a few at a time, since each reads its whole disk. A failed one
// is tried again sooner than its schedule, but not more often than
// RetryBackupAfter.
var (
	MaxScheduledBackups = 2
	RetryBackupAfter    = 5 * time.Minute
)

type backupScheduler struct {
	running atomic.Int32
	mu      sync.Mutex
	tried   map[string]time.Time // volume ID -> last start that didn't get as far as a recorded backup
}

// NextBackup is when a volume's next scheduled backup is due, given its
// latest backup (nil if none): now if it has none; zero when it isn't
// scheduled.
func NextBackup(b store.VolumeBackup, last *store.BackupRun, now time.Time) time.Time {
	switch {
	case b.Every <= 0:
		return time.Time{}
	case last == nil:
		return now
	case last.Status == types.StatusFailed:
		return last.StartedAt.Add(min(b.Every, RetryBackupAfter))
	}
	return last.StartedAt.Add(b.Every)
}

// tickBackups starts the scheduled backups that are due: the cluster's,
// then volumes'.
func (c *Controller) tickBackups(ctx context.Context) error {
	sets, err := c.Store.VolumeBackups(ctx, "", "")
	if err != nil {
		return err
	}
	now := time.Now()
	if err := c.tickClusterBackup(ctx, now); err != nil {
		return err
	}
	for _, b := range sets {
		if b.Every <= 0 || int(c.scheduler.running.Load()) >= MaxScheduledBackups {
			continue
		}
		if _, busy := c.backups.Load(b.VolumeID); busy {
			continue
		}
		runs, err := c.Store.BackupRuns(ctx, b.VolumeID, 1)
		if err != nil {
			return err
		}
		var last *store.BackupRun
		if len(runs) > 0 {
			last = &runs[0]
		}
		next := NextBackup(b, last, now)
		c.scheduler.mu.Lock()
		if tried, ok := c.scheduler.tried[b.VolumeID]; ok && tried.Add(min(b.Every, RetryBackupAfter)).After(next) {
			next = tried.Add(min(b.Every, RetryBackupAfter))
		}
		c.scheduler.mu.Unlock()
		if now.Before(next) || !c.backupReady(ctx, b, now) {
			continue
		}
		c.scheduler.mu.Lock()
		if c.scheduler.tried == nil {
			c.scheduler.tried = map[string]time.Time{}
		}
		c.scheduler.tried[b.VolumeID] = now
		c.scheduler.mu.Unlock()
		c.scheduler.running.Add(1)
		go func(b store.VolumeBackup) {
			defer c.scheduler.running.Add(-1)
			_, err := c.BackupVolume(context.WithoutCancel(ctx), b.App, b.Volume, ScheduleActor, "", func(string) {})
			if err != nil {
				c.Log.Warn("scheduled backup", "app", b.App, "volume", b.Volume, "err", err)
			}
		}(b)
	}
	return nil
}

// tickClusterBackup starts the cluster's own backup when it is due.
func (c *Controller) tickClusterBackup(ctx context.Context, now time.Time) error {
	set, err := c.Store.ClusterBackup(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if set.Every <= 0 || c.DataDir == "" || int(c.scheduler.running.Load()) >= MaxScheduledBackups {
		return nil
	}
	if _, busy := c.backups.Load(clusterLock); busy {
		return nil
	}
	runs, err := c.Store.ClusterBackupRuns(ctx, 1)
	if err != nil {
		return err
	}
	var last *store.BackupRun
	if len(runs) > 0 {
		last = &runs[0]
	}
	next := NextBackup(*set, last, now)
	c.scheduler.mu.Lock()
	defer c.scheduler.mu.Unlock()
	if tried, ok := c.scheduler.tried[clusterLock]; ok && tried.Add(min(set.Every, RetryBackupAfter)).After(next) {
		next = tried.Add(min(set.Every, RetryBackupAfter))
	}
	if now.Before(next) {
		return nil
	}
	if c.scheduler.tried == nil {
		c.scheduler.tried = map[string]time.Time{}
	}
	c.scheduler.tried[clusterLock] = now
	c.scheduler.running.Add(1)
	go func() {
		defer c.scheduler.running.Add(-1)
		if _, err := c.BackupCluster(context.WithoutCancel(ctx), ScheduleActor, func(string) {}); err != nil {
			c.Log.Warn("scheduled cluster backup", "err", err)
		}
	}()
	return nil
}

// backupReady says whether a volume can be backed up now. One whose node
// is down, that is moving, being restored or has no disk yet is waited for,
// without counting as a failed backup.
func (c *Controller) backupReady(ctx context.Context, b store.VolumeBackup, now time.Time) bool {
	v, err := c.Store.VolumeByID(ctx, b.VolumeID)
	if err != nil || v.Node == "" || v.Moving() || v.Restore != "" || v.State == store.VolumeDestroying {
		return false
	}
	n, err := c.Store.Node(ctx, v.Node)
	if errors.Is(err, store.ErrNotFound) || err != nil {
		return false
	}
	return n.Ready(now)
}

// BackupStatus describes a volume's backup settings and its latest backups
// (newest first), without the list of backups in the bucket.
func BackupStatus(b store.VolumeBackup, runs []store.BackupRun) types.VolumeBackups {
	out := types.VolumeBackups{
		App: b.App, Volume: b.Volume, Destination: b.Destination, Path: b.Path,
		EverySeconds: int64(b.Every / time.Second), KeepRecentSeconds: int64(b.KeepRecent / time.Second), KeepDaily: b.KeepDaily,
		StoredBytes: b.StoredBytes, StoredBackups: b.StoredBackups, AutoRestore: b.AutoRestore,
	}
	if b.AutoRestore {
		out.FailoverSeconds = int64(FailoverAfter / time.Second)
	}
	var last *store.BackupRun
	for i, r := range runs {
		info := &types.BackupRun{Name: r.Name, Status: r.Status, Error: r.Error, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
		if i == 0 {
			out.Last, last = info, &runs[0]
		}
		if r.Status == types.StatusSucceeded {
			out.Succeeded = info
			break
		}
	}
	out.Next = NextBackup(b, last, time.Now())
	return out
}

// FailoverAfter is how long a volume's node must have been silent before
// the volume is restored onto another node from its latest backup. Nodes
// stop the instances using such volumes after agent.FenceAfter without the
// control node, well before.
var FailoverAfter = 5 * time.Minute

// RetryFailoverAfter spaces out attempts at a restore that can't happen
// yet (no node with room, say).
var RetryFailoverAfter = time.Minute

type failover struct {
	mu      sync.Mutex
	running map[string]bool
	tried   map[string]time.Time
	told    map[string]string // the last reason given for not restoring
}

// autoRestore restores a volume from its latest backup, in the
// background: onto another node when its node is down or gone, or where it
// is when its disk is missing. why says which.
func (c *Controller) autoRestore(v store.Volume, why string) {
	f := &c.failover
	f.mu.Lock()
	if f.running == nil {
		f.running, f.tried, f.told = map[string]bool{}, map[string]time.Time{}, map[string]string{}
	}
	if f.running[v.ID] || time.Since(f.tried[v.ID]) < RetryFailoverAfter {
		f.mu.Unlock()
		return
	}
	f.running[v.ID], f.tried[v.ID] = true, time.Now()
	f.mu.Unlock()
	go func() {
		ctx := context.Background()
		backupName, node, err := c.RestoreVolume(ctx, v.App, v.Name, "", "", "jokku")
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.running, v.ID)
		if err != nil {
			if f.told[v.ID] != err.Error() {
				f.told[v.ID] = err.Error()
				c.Store.AddEvent(ctx, "volume", v.App, v.Node, "volume %s can't be restored from its backups yet (%s): %v", v.Name, why, err)
			}
			return
		}
		delete(f.told, v.ID)
		if node == v.Node {
			c.Store.AddEvent(ctx, "volume", v.App, node, "%s: restoring volume %s from its latest backup, %s", why, v.Name, backupName)
			return
		}
		c.Store.AddEvent(ctx, "volume", v.App, node, "%s: restoring volume %s onto %s from its latest backup, %s. "+
			"Anything written after that backup is only on %s's disk, which it keeps aside if it comes back", why, v.Name, node, backupName, v.Node)
	}()
}
