package cluster_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/backup"
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
	if _, err := c.SetVolumeBackup(ctx, "db", "data", types.SetVolumeBackupRequest{Destination: "tigris"}); err != nil {
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

	// w1 comes back: its copy of the app stops, and its disk is kept.
	h.revive("w1")
	eventually(t, 10*time.Second, "the old web.1 stopped on w1", func() error {
		if n := h.nodes["w1"].rt.apps()["db"]; n != 0 {
			return errFmt("db still runs on w1")
		}
		return nil
	})
	if _, err := os.Stat(h.disk("w1", v)); err != nil {
		t.Errorf("w1's disk is gone: %v", err)
	}
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
