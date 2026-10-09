package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/wes/jokku/internal/backup"
	"github.com/wes/jokku/internal/daemon"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

var restoreClusterCommand = &Command{
	Name: "restore-cluster", Local: true, serverOnly: true,
	Help: "Make this server the control node of a cluster from its backup: its database, identity and recent releases (as root; asks for the keys)",
	Flags: []Flag{
		{Name: "endpoint", Value: "url", Help: "The S3 endpoint the cluster was backed up to"},
		{Name: "bucket", Value: "bucket", Help: "The bucket"},
		{Name: "region", Value: "region", Help: "The bucket's region, if the endpoint needs one"},
		{Name: "access-key-id", Value: "id", Help: "The access key ID"},
		{Name: "path", Value: "path", Help: "Where in the bucket (default " + "jokku/cluster" + ")"},
		{Name: "backup", Value: "name", Help: "Which backup (default the latest)"},
		{Name: "data-dir", Value: "DIR", Help: "State directory (default /var/lib/jokku)"},
		{Name: "force", Help: "Replace a cluster already on this server, or restore onto a server with another name"},
	},
	Run: runRestoreCluster,
}

func runRestoreCluster(c *Context) error {
	if os.Geteuid() != 0 {
		return errors.New("restore-cluster replaces this server's state: run it as root (sudo jokku restore-cluster ...)")
	}
	dest := types.BackupDestination{
		Name: "restore", Endpoint: c.String("endpoint"), Region: c.String("region"), Bucket: c.String("bucket"),
		AccessKeyID: c.String("access-key-id"),
	}
	if dest.Endpoint == "" || dest.Bucket == "" || dest.AccessKeyID == "" {
		return usageErr("Say where the cluster was backed up: --endpoint, --bucket and --access-key-id")
	}
	in := bufio.NewReader(c.Stdin)
	secret, err := askLine(c, in, "Secret access key: ")
	if err != nil {
		return err
	}
	dest.SecretAccessKey = secret
	keyText, err := askLine(c, in, "Backup key (jbk1_..., or empty if the backups aren't encrypted): ")
	if err != nil {
		return err
	}
	var key *backup.Key
	if keyText != "" {
		if key, err = backup.ParseKey(keyText); err != nil {
			return err
		}
	}
	host, _ := os.Hostname()
	o := restoreClusterOptions{
		Dest: dest, Path: c.String("path"), Backup: c.String("backup"), Key: key, KeyText: keyText,
		DataDir: c.String("data-dir"), NodeName: host, Force: c.Bool("force"),
		Stop:  func() error { return exec.Command("systemctl", "stop", "jokku").Run() },
		Start: func() error { return exec.Command("systemctl", "start", "jokku").Run() },
		Log:   func(s string) { fmt.Fprintln(c.Stdout, s) },
	}
	if o.DataDir == "" {
		o.DataDir = daemon.DefaultDataDir
	}
	return restoreCluster(c, o)
}

// askLine asks for a secret: hidden at a terminal, else a line of stdin.
func askLine(c *Context, in *bufio.Reader, prompt string) (string, error) {
	if f, ok := c.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(c.Stdout, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(c.Stdout)
		return strings.TrimSpace(string(b)), err
	}
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line), nil
}

type restoreClusterOptions struct {
	Dest     types.BackupDestination
	Path     string // in the bucket; default jokku/cluster
	Backup   string // default the latest
	Key      *backup.Key
	KeyText  string
	DataDir  string
	NodeName string // this server's
	Force    bool

	Stop, Start func() error // the jokku service
	Log         func(string)
}

// restoreCluster makes DataDir the state of the cluster backed up at
// o.Path: the release images first, then, with jokku stopped, the database
// and identity, the current ones kept aside.
func restoreCluster(ctx context.Context, o restoreClusterOptions) error {
	if o.Path == "" {
		o.Path = "jokku/cluster"
	}
	st, err := backup.Open(o.Dest)
	if err != nil {
		return err
	}
	if err := st.Check(ctx); err != nil {
		return fmt.Errorf("cannot use bucket %s: %w", o.Dest.Bucket, err)
	}
	statePath, apath := o.Path+"/"+backup.StateDir, o.Path+"/"+backup.ArtifactsDir
	names, err := backup.List(ctx, st, statePath)
	if err != nil {
		return err
	}
	switch {
	case len(names) == 0:
		return fmt.Errorf("there are no cluster backups under %s in %s", o.Path, o.Dest.Bucket)
	case o.Backup == "":
		o.Backup = names[len(names)-1]
	case !slices.Contains(names, o.Backup):
		return fmt.Errorf("there is no cluster backup %s (there are %d; the latest is %s)", o.Backup, len(names), names[len(names)-1])
	}
	if err := os.MkdirAll(o.DataDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(o.DataDir, "restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	o.Log("-----> Downloading cluster backup " + o.Backup)
	tarPath := filepath.Join(tmp, "state.tar")
	if _, err := backup.Restore(ctx, st, o.Key, statePath, o.Backup, tarPath, nil); err != nil {
		return err
	}
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	stateDir := filepath.Join(tmp, "state")
	files, err := backup.UnpackState(f, stateDir)
	f.Close()
	if err != nil {
		return err
	}

	// What it holds, and whether it belongs here.
	db, err := store.Open(filepath.Join(stateDir, "jokku.db"))
	if err != nil {
		return fmt.Errorf("the backup's database: %w", err)
	}
	nodes, err := db.Nodes(ctx)
	if err == nil {
		var apps []types.App
		if apps, err = db.Apps(ctx); err == nil {
			o.Log(fmt.Sprintf("       %d apps, %d servers", len(apps), len(nodes)))
		}
	}
	kept, kerr := db.KeptArtifacts(ctx, 3)
	db.Close()
	if err != nil {
		return err
	}
	if kerr != nil {
		return kerr
	}
	control, workers := "", []string{}
	for _, n := range nodes {
		if n.Role == store.RoleControl {
			control = n.Name
		} else {
			workers = append(workers, n.Name)
		}
	}
	if control != "" && control != o.NodeName && !o.Force {
		return fmt.Errorf("this cluster's control node was named %s, and this server is %s: name it %s first (hostnamectl set-hostname %s), or pass --force",
			control, o.NodeName, control, control)
	}
	if _, err := os.Stat(filepath.Join(o.DataDir, "jokku.db")); err == nil {
		cur, err := store.Open(filepath.Join(o.DataDir, "jokku.db"))
		if err != nil {
			return err
		}
		apps, _ := cur.Apps(ctx)
		cur.Close()
		if len(apps) > 0 && !o.Force {
			return fmt.Errorf("this server already runs a cluster with %d apps; pass --force to replace it (its state is kept aside)", len(apps))
		}
	}

	// Release images, so apps start without being deployed again.
	have, err := backup.List(ctx, st, apath)
	if err != nil {
		return err
	}
	artifacts := filepath.Join(o.DataDir, "artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		return err
	}
	got, missing := 0, 0
	for a := range kept {
		if a == "" {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(a), ".ext4")
		dst := filepath.Join(artifacts, filepath.Base(a))
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if !slices.Contains(have, name) {
			missing++
			continue
		}
		part := dst + ".part"
		if _, err := backup.Restore(ctx, st, o.Key, apath, name, part, nil); err != nil {
			return fmt.Errorf("downloading the release %s: %w", name, err)
		}
		if err := os.Rename(part, dst); err != nil {
			return err
		}
		got++
	}
	o.Log(fmt.Sprintf("-----> Downloaded the root filesystems of %d releases", got))
	if missing > 0 {
		o.Log(fmt.Sprintf("       %d releases weren't in the backup; deploy those apps again", missing))
	}

	// The swap, with jokku stopped.
	o.Log("-----> Stopping jokku to put the cluster's state in place")
	if err := o.Stop(); err != nil {
		return fmt.Errorf("stopping jokku: %w", err)
	}
	aside := filepath.Join(o.DataDir, "before-restore-"+time.Now().UTC().Format("20060102T150405Z"))
	for _, name := range append(slices.Clone(backup.StateFiles), "jokku.db-wal", "jokku.db-shm") {
		src := filepath.Join(o.DataDir, filepath.FromSlash(name))
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(aside, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	for _, name := range files {
		dst := filepath.Join(o.DataDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(stateDir, filepath.FromSlash(name)), dst); err != nil {
			return err
		}
	}
	o.Log("       This server's previous state is kept in " + aside)
	if err := o.Start(); err != nil {
		return fmt.Errorf("starting jokku: %w", err)
	}
	o.Log("=====> Restored the cluster from " + o.Backup)
	o.Log("       Volumes come back from their own backups: on this server as soon as jokku finds their disks missing,")
	o.Log("       on other servers once they have been down five minutes.")
	if len(workers) > 0 {
		o.Log("       Servers that are gone for good: remove them, so their volumes move here: jokku nodes:remove <name> --force")
		o.Log("       (" + strings.Join(workers, ", ") + "). Servers still running reconnect if this server has the old one's address;")
		o.Log("       otherwise join them again (jokku cluster:join-command).")
	}
	return nil
}
