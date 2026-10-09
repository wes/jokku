package cluster_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/agent"
	"github.com/wes/jokku/internal/backup"
	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// backupsTo points every backup destination at a directory, as a bucket.
func (h *harness) backupsTo(dir string) {
	os.MkdirAll(dir, 0o700)
	open := backup.Open
	backup.Open = func(d types.BackupDestination) (backup.Store, error) { return backup.Dir(dir), nil }
	h.t.Cleanup(func() { backup.Open = open })
}

func (h *harness) logged(what string) func(types.Event) {
	return func(e types.Event) { h.t.Logf("%s: %s", what, e.Message) }
}

func TestBackupAndRestore(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.join("w2")
	h.waitReady(3)
	bucket := filepath.Join(t.TempDir(), "bucket")
	h.backupsTo(bucket)
	undo := h.onlyOn("w1")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()
	v := h.volume("db", "data")
	ctx, c := h.ctx, h.client

	// Encrypted destinations wait for a key the user has saved.
	dest := types.CreateBackupDestinationRequest{Name: "tigris", Endpoint: "fly.storage.tigris.dev", Bucket: "b", AccessKeyID: "id", SecretAccessKey: "secret"}
	if _, err := c.CreateBackupDestination(ctx, dest); err == nil || !strings.Contains(err.Error(), "jokku backups:key") {
		t.Fatalf("an encrypted destination without a saved key: %v", err)
	}
	key, err := c.CreateBackupKey(ctx)
	if err != nil || key.Saved {
		t.Fatalf("key: %+v, %v", key, err)
	}
	if again, _ := c.CreateBackupKey(ctx); again.Key != key.Key {
		t.Fatal("asking for the key again made another one")
	}
	if err := c.SaveBackupKey(ctx); err != nil {
		t.Fatal(err)
	}
	d, err := c.CreateBackupDestination(ctx, dest)
	if err != nil || d.Endpoint != "https://fly.storage.tigris.dev" || !d.Encrypt || d.SecretAccessKey != "" {
		t.Fatalf("destination: %+v, %v", d, err)
	}
	if list, _ := c.BackupDestinations(ctx); len(list) != 1 || list[0].SecretAccessKey != "" {
		t.Fatalf("destinations: %+v", list)
	}
	if _, err := c.SetVolumeBackup(ctx, "db", "data", types.SetVolumeBackupRequest{Destination: "tigris", Every: "off"}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteBackupDestination(ctx, "tigris"); err == nil || !strings.Contains(err.Error(), "db data") {
		t.Errorf("removing a destination in use: %v", err)
	}

	// A backup of the running app pauses its writes for a moment.
	first := writeData(t, h.disk("w1", v))
	if err := c.RunBackup(ctx, "db", "data", h.logged("backup")); err != nil {
		t.Fatal(err)
	}
	in := h.web1("db")
	h.nodes["w1"].rt.mu.Lock()
	freezes := h.nodes["w1"].rt.freezes[in.ID]
	h.nodes["w1"].rt.mu.Unlock()
	if freezes != 1 {
		t.Errorf("writes were paused %d times, want 1", freezes)
	}
	if _, err := os.Stat(filepath.Join(bucket, "jokku/db/data/jokku-backup.json")); err != nil {
		t.Fatalf("nothing in the bucket: %v", err)
	}

	// The app changes the data; the next backup adds only that.
	f, _ := os.OpenFile(h.disk("w1", v), os.O_RDWR, 0)
	f.WriteAt(bytes.Repeat([]byte{9}, 4096), 20<<20)
	f.Close()
	if err := c.RunBackup(ctx, "db", "data", h.logged("backup")); err != nil {
		t.Fatal(err)
	}
	list, err := c.VolumeBackups(ctx, "db", "data")
	if err != nil || len(list.Backups) != 2 {
		t.Fatalf("backups: %+v, %v", list, err)
	}
	if list.Backups[0].NewBlocks != 1 {
		t.Errorf("the second backup added %d blocks, want 1", list.Backups[0].NewBlocks)
	}
	oldest := list.Backups[1].Name

	// jokku top's view of it all.
	ov, err := c.BackupsOverview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Volumes) != 1 || ov.Volumes[0].Backups == nil || len(ov.Volumes[0].Recent) != 2 || ov.Volumes[0].Node != "w1" {
		t.Errorf("overview volumes: %+v", ov.Volumes)
	}
	if ov.Cluster == nil || ov.Key == nil || !ov.Key.Saved || ov.Key.ID != key.ID || len(ov.Destinations) != 1 || ov.Destinations[0].SecretAccessKey != "" {
		t.Errorf("overview: cluster %+v, key %+v, destinations %+v", ov.Cluster, ov.Key, ov.Destinations)
	}

	// Restore the first backup where the volume is: the current data is
	// backed up first, then the app restarts with the restored disk.
	second, _ := os.ReadFile(h.disk("w1", v))
	if err := c.RestoreVolume(ctx, "db", "data", types.RestoreVolumeRequest{Backup: oldest}, h.logged("restore")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(h.disk("w1", v)); !bytes.Equal(got, first) {
		t.Error("the restored disk is not the first backup's")
	}
	if list, _ := c.VolumeBackups(ctx, "db", "data"); len(list.Backups) != 3 {
		t.Errorf("%d backups after the restore, want 3 (the data before it)", len(list.Backups))
	}
	eventually(t, 10*time.Second, "web.1 back with the restored disk", func() error {
		return h.healthy("db", "w1", in.ID)
	})
	h.noDoubleAttach()

	// Its node dies: restore it onto another, where the app starts again.
	h.kill("w1")
	eventually(t, 10*time.Second, "w1 down", func() error {
		if n, _ := h.st.Node(ctx, "w1"); n.Ready(time.Now()) {
			return errFmt("w1 still up")
		}
		return nil
	})
	if err := c.RestoreVolume(ctx, "db", "data", types.RestoreVolumeRequest{Node: "w2"}, h.logged("restore")); err != nil {
		t.Fatal(err)
	}
	if v := h.volume("db", "data"); v.Node != "w2" || v.Restore != "" {
		t.Fatalf("volume after the restore: node %s, restore %q", v.Node, v.Restore)
	}
	// The latest backup is the one taken just before the first restore.
	if got, _ := os.ReadFile(h.disk("w2", v)); !bytes.Equal(got, second) {
		t.Error("the disk restored onto w2 is not the latest backup's")
	}
	eventually(t, 10*time.Second, "web.1 running on w2", func() error { return h.healthy("db", "w2", "") })

	// w1 comes back: its copy of the app stops, and its disk is kept aside.
	h.revive("w1")
	eventually(t, 10*time.Second, "the old web.1 stopped on w1, its disk kept aside", func() error {
		if n := h.nodes["w1"].rt.apps()["db"]; n != 0 {
			return errFmt("db still runs on w1")
		}
		_, err := os.Stat(h.disk("w1", v) + ".stale")
		return err
	})
	if err := h.healthy("db", "w2", ""); err != nil {
		t.Error(err)
	}

	events, _ := h.st.Events(ctx, "db", 50)
	var all []string
	for _, e := range events {
		all = append(all, e.Message)
	}
	for _, want := range []string{"tester backed up volume data", "restored from backup " + oldest, "onto w2"} {
		if !strings.Contains(strings.Join(all, "\n"), want) {
			t.Errorf("no event %q in:\n%s", want, strings.Join(all, "\n"))
		}
	}
}

// healthy checks the app's web instance is healthy on node (and isn't
// instance not, when given).
func (h *harness) healthy(app, node, not string) error {
	in := h.web1(app)
	switch {
	case in == nil:
		return errFmt("no web instance")
	case in.Node != node || in.ID == not:
		return errFmt("web is " + in.ID + " on " + in.Node)
	case in.State != store.StateHealthy:
		return errFmt("web is " + in.State)
	}
	return nil
}

type errFmt string

func (e errFmt) Error() string { return string(e) }

// Backups run on their schedule, and retention prunes them and deletes the
// blocks no backup uses.
func TestScheduledBackups(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.waitReady(2)
	bucket := filepath.Join(t.TempDir(), "bucket")
	h.backupsTo(bucket)
	collect := cluster.CollectEvery
	cluster.CollectEvery = 0
	t.Cleanup(func() { cluster.CollectEvery = collect })
	undo := h.onlyOn("w1")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()
	v := h.volume("db", "data")
	ctx := h.ctx
	if _, _, err := h.st.CreateBackupKey(ctx, backup.NewKey().String()); err != nil {
		t.Fatal(err)
	}
	h.st.SetBackupKeySaved(ctx)
	if err := h.st.CreateBackupDestination(ctx, types.BackupDestination{Name: "s3", Endpoint: "https://s3.example.test", Bucket: "b", Encrypt: true}); err != nil {
		t.Fatal(err)
	}
	// Every second (the API allows no less than 5 minutes), keeping only
	// the newest.
	if err := h.st.SetVolumeBackup(ctx, store.VolumeBackup{VolumeID: v.ID, Destination: "s3", Path: "jokku/db/data", Every: time.Second}); err != nil {
		t.Fatal(err)
	}
	writeData(t, h.disk("w1", v))
	eventually(t, 20*time.Second, "three scheduled backups", func() error {
		runs, _ := h.st.BackupRuns(ctx, v.ID, 10)
		ok := 0
		for _, r := range runs {
			switch {
			case r.Actor != cluster.ScheduleActor:
				return errFmt("a backup by " + r.Actor)
			case r.Status == types.StatusFailed:
				return errFmt("a scheduled backup failed: " + r.Error)
			case r.Status == types.StatusSucceeded:
				ok++
			}
		}
		if ok < 3 {
			return errFmt("not yet")
		}
		return nil
	})
	// Changing the data between backups leaves blocks only old backups used.
	f, _ := os.OpenFile(h.disk("w1", v), os.O_RDWR, 0)
	f.WriteAt(bytes.Repeat([]byte{7}, 4096), 0)
	f.Close()
	eventually(t, 20*time.Second, "pruned to the newest backup, without its old blocks", func() error {
		set, _ := h.st.VolumeBackupFor(ctx, v.ID)
		names, _ := backup.List(ctx, backup.Dir(bucket), "jokku/db/data")
		blocks, _ := backup.Dir(bucket).List(ctx, "jokku/db/data/blocks/")
		if len(names) != 1 || set.StoredBackups != 1 || len(blocks) != 3 {
			return errFmt(fmt.Sprintf("%d backups (%d recorded), %d blocks", len(names), set.StoredBackups, len(blocks)))
		}
		return nil
	})
	events, _ := h.st.Events(ctx, "db", 100)
	for _, e := range events {
		if strings.Contains(e.Message, "backed up") {
			t.Errorf("a scheduled backup made an event: %s", e.Message)
		}
	}
}

// backedUp sets a volume up to back up to bucket (every: 0 for only when
// asked), with an encrypted destination and a saved key, and backs it up
// once.
func (h *harness) backedUp(bucket string, v *store.Volume, every time.Duration) {
	h.t.Helper()
	ctx := h.ctx
	h.backupsTo(bucket)
	if _, _, err := h.st.CreateBackupKey(ctx, backup.NewKey().String()); err != nil {
		h.t.Fatal(err)
	}
	h.st.SetBackupKeySaved(ctx)
	if err := h.st.CreateBackupDestination(ctx, types.BackupDestination{Name: "s3", Endpoint: "https://s3.example.test", Bucket: "b", Encrypt: true}); err != nil {
		h.t.Fatal(err)
	}
	set := store.VolumeBackup{VolumeID: v.ID, Destination: "s3", Path: "jokku/" + v.App + "/" + v.Name, Every: every,
		KeepRecent: store.DefaultKeepRecent, KeepDaily: store.DefaultKeepDaily, AutoRestore: true}
	if err := h.st.SetVolumeBackup(ctx, set); err != nil {
		h.t.Fatal(err)
	}
	if every == 0 {
		if err := h.client.RunBackup(ctx, v.App, v.Name, func(types.Event) {}); err != nil {
			h.t.Fatal(err)
		}
	}
}

// A node cut off from the control node stops the app using an
// auto-restored volume; the control node restores the volume onto another
// node, where the app starts again. When the node is back, it keeps its
// copy aside until it is discarded.
func TestAutoRestoreWhenANodeIsCutOff(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.join("w2")
	h.waitReady(3)
	undo := h.onlyOn("w1")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()
	v := h.volume("db", "data")
	backedUp := writeData(t, h.disk("w1", v))
	h.backedUp(filepath.Join(t.TempDir(), "bucket"), v, 0)
	// Written after the backup: only w1 has it.
	f, _ := os.OpenFile(h.disk("w1", v), os.O_RDWR, 0)
	f.WriteAt(bytes.Repeat([]byte{8}, 4096), 30<<20)
	f.Close()

	h.nodes["w1"].cut.Store(true)
	eventually(t, 10*time.Second, "w1 stops db while cut off", func() error {
		if h.nodes["w1"].rt.apps()["db"] != 0 {
			return errFmt("db still runs on w1")
		}
		return nil
	})
	var target string
	eventually(t, 20*time.Second, "db restored and running on another node", func() error {
		in := h.web1("db")
		if in == nil || in.Node == "w1" {
			return errFmt("not moved")
		}
		target = in.Node
		return h.healthy("db", target, "")
	})
	h.noDoubleAttach()
	if got, _ := os.ReadFile(h.disk(target, v)); !bytes.Equal(got, backedUp) {
		t.Errorf("%s's disk is not the backup's", target)
	}
	events, _ := h.st.Events(h.ctx, "db", 50)
	var all []string
	for _, e := range events {
		all = append(all, e.Message)
	}
	if !strings.Contains(strings.Join(all, "\n"), "w1 is down: restoring volume data onto "+target+" from its latest backup") {
		t.Errorf("no event about the restore in:\n%s", strings.Join(all, "\n"))
	}

	// w1 is back: its disk, with the write the backup missed, is kept aside.
	h.nodes["w1"].cut.Store(false)
	stale := h.disk("w1", v) + ".stale"
	eventually(t, 10*time.Second, "w1 keeps its copy aside", func() error {
		if _, err := os.Stat(stale); err != nil {
			return err
		}
		if _, err := os.Stat(h.disk("w1", v)); err == nil {
			return errFmt("the disk is still in place on w1")
		}
		return nil
	})
	if got, _ := os.ReadFile(stale); !bytes.Equal(got[30<<20:30<<20+4096], bytes.Repeat([]byte{8}, 4096)) {
		t.Error("the copy kept aside lost the write made after the backup")
	}
	vol, err := h.client.Volume(h.ctx, "db", "data")
	if err != nil || vol.OldCopy == nil || vol.OldCopy.Node != "w1" {
		t.Fatalf("the volume doesn't show its old copy: %+v, %v", vol, err)
	}
	if h.nodes["w1"].rt.apps()["db"] != 0 {
		t.Error("db started again on w1")
	}
	if err := h.client.DiscardOldCopy(h.ctx, "db", "data"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the old copy deleted", func() error {
		if _, err := os.Stat(stale); err == nil {
			return errFmt("still there")
		}
		if v := h.volume("db", "data"); v.StaleNode != "" {
			return errFmt("still recorded on " + v.StaleNode)
		}
		return nil
	})
	h.noDoubleAttach()
}

// When the control node is down for everyone, nothing can be restored, so
// nodes keep running apps with auto-restored volumes.
func TestControlOutageKeepsVolumeApps(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.join("w2")
	h.waitReady(3)
	undo := h.onlyOn("w1")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()
	v := h.volume("db", "data")
	h.backedUp(filepath.Join(t.TempDir(), "bucket"), v, 0)
	in := h.web1("db")

	h.down.Store(true)
	time.Sleep(4 * agent.FenceAfter)
	if h.nodes["w1"].rt.apps()["db"] != 1 {
		t.Error("w1 stopped db while the control node was down for every node")
	}
	h.down.Store(false)
	eventually(t, 10*time.Second, "db still on w1", func() error { return h.healthy("db", "w1", "") })
	if got := h.web1("db"); got.ID != in.ID {
		t.Errorf("db was replaced: %s, then %s", in.ID, got.ID)
	}
}

// A volume whose disk goes missing is restored from its latest backup, where
// it is.
func TestMissingDiskIsRestored(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.waitReady(2)
	undo := h.onlyOn("w1")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()
	v := h.volume("db", "data")
	backedUp := writeData(t, h.disk("w1", v))
	h.backedUp(filepath.Join(t.TempDir(), "bucket"), v, 0)
	in := h.web1("db")

	if err := os.Remove(h.disk("w1", v)); err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, "the disk restored, and db running with it", func() error {
		if err := h.healthy("db", "w1", in.ID); err != nil {
			return err
		}
		if v := h.volume("db", "data"); v.Restore != "" {
			return errFmt("still restoring")
		}
		return nil
	})
	if got, _ := os.ReadFile(h.disk("w1", v)); !bytes.Equal(got, backedUp) {
		t.Error("the restored disk is not the backup's")
	}
	h.noDoubleAttach()
}
