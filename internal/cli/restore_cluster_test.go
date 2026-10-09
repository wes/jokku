package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wes/jokku/internal/backup"
	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// A cluster backup brings a lost control node back on a new server: its
// database, its identity, and the releases its apps run.
func TestRestoreCluster(t *testing.T) {
	ctx := context.Background()
	bucket := t.TempDir()
	open := backup.Open
	backup.Open = func(types.BackupDestination) (backup.Store, error) { return backup.Dir(bucket), nil }
	t.Cleanup(func() { backup.Open = open })

	// The old control node.
	old := t.TempDir()
	st, err := store.Open(filepath.Join(old, "jokku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.RegisterControlNode(ctx, store.Node{Name: "jokku1", Address: "10.0.0.1"})
	os.MkdirAll(filepath.Join(old, "tls"), 0o700)
	for name, content := range map[string]string{"tls/control.key": "KEY", "tls/control.crt": "CERT", "wireguard.key": "WG", "agent-token": "AT"} {
		os.WriteFile(filepath.Join(old, name), []byte(content), 0o600)
	}
	st.CreateApp(ctx, "shop")
	artifact := filepath.Join(old, "artifacts", "abc123.ext4")
	os.MkdirAll(filepath.Dir(artifact), 0o755)
	rootfs := bytes.Repeat([]byte("rootfs!"), 300000)
	os.WriteFile(artifact, rootfs, 0o644)
	rel := &store.Release{App: "shop", Artifact: artifact, Processes: map[string][]string{"web": {"/bin/shop"}}, ConfigVars: map[string]string{}}
	if err := st.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	st.SetCurrentRelease(ctx, "shop", rel.ID)
	key := backup.NewKey()
	st.CreateBackupKey(ctx, key.String())
	st.SetBackupKeySaved(ctx)
	st.CreateBackupDestination(ctx, types.BackupDestination{Name: "s3", Endpoint: "https://s3.example.test", Bucket: "b", Encrypt: true})
	st.SetClusterBackup(ctx, store.VolumeBackup{Destination: "s3", Path: "jokku/cluster", Every: 0, KeepRecent: store.DefaultKeepRecent, KeepDaily: 30})
	ctl := cluster.New(&cluster.Controller{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Self: "jokku1",
		ClusterCIDR: netip.MustParsePrefix("10.210.0.0/16"), DataDir: old})
	if _, err := ctl.BackupCluster(ctx, "tester", func(string) {}); err != nil {
		t.Fatal(err)
	}
	// A second backup finds the release already there.
	var lines []string
	if _, err := ctl.BackupCluster(ctx, "tester", func(l string) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(lines, "\n"), "root filesystems") {
		t.Errorf("the release was backed up twice: %v", lines)
	}

	// The new server.
	fresh := t.TempDir()
	stopped, started := false, false
	o := restoreClusterOptions{
		Dest: types.BackupDestination{Bucket: "b"}, Key: key, DataDir: fresh, NodeName: "jokku1",
		Stop: func() error { stopped = true; return nil }, Start: func() error { started = true; return nil },
		Log: func(s string) { t.Log(s) },
	}
	if err := restoreCluster(ctx, restoreClusterOptions{Dest: o.Dest, Key: backup.NewKey(), DataDir: fresh, NodeName: "jokku1",
		Stop: o.Stop, Start: o.Start, Log: o.Log}); err == nil || !strings.Contains(err.Error(), "another key") {
		t.Fatalf("restoring with another key: %v", err)
	}
	other := o
	other.NodeName = "jokku9"
	if err := restoreCluster(ctx, other); err == nil || !strings.Contains(err.Error(), "hostnamectl set-hostname jokku1") {
		t.Fatalf("restoring onto a server with another name: %v", err)
	}
	if err := restoreCluster(ctx, o); err != nil {
		t.Fatal(err)
	}
	if !stopped || !started {
		t.Error("jokku was not stopped and started around the swap")
	}
	restored, err := store.Open(filepath.Join(fresh, "jokku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if apps, _ := restored.Apps(ctx); len(apps) != 1 || apps[0].Name != "shop" {
		t.Errorf("restored apps: %+v", apps)
	}
	if b, _ := os.ReadFile(filepath.Join(fresh, "tls/control.key")); string(b) != "KEY" {
		t.Errorf("the TLS key was not restored: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(fresh, "artifacts", "abc123.ext4")); !bytes.Equal(b, rootfs) {
		t.Error("the release's root filesystem was not restored")
	}
	// Once there is a cluster, restoring over it takes --force.
	if err := restoreCluster(ctx, o); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("restoring over a cluster: %v", err)
	}
}
