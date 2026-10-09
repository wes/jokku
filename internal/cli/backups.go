package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/wes/jokku/internal/types"
)

var backupsCommands = []*Command{
	{Name: "backups:destination-add", Help: "Add an S3-compatible bucket to back volumes up to (the secret key is asked for, or read from stdin)",
		Args: "<name>", MinArgs: 1, MaxArgs: 1, Flags: []Flag{
			{Name: "endpoint", Value: "url", Help: "The S3 endpoint, e.g. https://fly.storage.tigris.dev"},
			{Name: "bucket", Value: "bucket", Help: "The bucket"},
			{Name: "region", Value: "region", Help: "The bucket's region, if the endpoint needs one"},
			{Name: "access-key-id", Value: "id", Help: "The access key ID"},
			{Name: "secret-access-key", Value: "secret", Help: "The secret access key (better left out, to be asked for)"},
			{Name: "no-encrypt", Help: "Store backups unencrypted"},
			{Name: "force", Help: "Don't ask to confirm the backup key is saved"},
		}, Run: backupsDestinationAdd},
	{Name: "backups:destinations", Help: "List backup destinations", Run: backupsDestinations},
	{Name: "backups:destination-remove", Help: "Remove a backup destination (its backups stay in the bucket)",
		Args: "<name>", MinArgs: 1, MaxArgs: 1, Run: backupsDestinationRemove},
	{Name: "backups:key", Help: "Show the key that encrypts backups; without it they can't be restored",
		Flags: []Flag{{Name: "force", Help: "Don't ask to confirm the key is saved"}}, Run: backupsKey},
	{Name: "backups:set", Help: "Back a volume up to a destination, under a path (default: jokku/<app>/<volume>), on a schedule",
		App: NeedsApp, Args: "<volume> <destination> [<path>]", MinArgs: 2, MaxArgs: 3, Flags: []Flag{
			{Name: "every", Value: "duration", Help: "How often: 15m (the default), 1h, 1d, or off for only when asked"},
			{Name: "keep-recent", Value: "duration", Help: "Keep every backup this recent (default 24h)"},
			{Name: "keep-daily", Value: "days", Help: "And the last backup of each of this many days (default 30)"},
			{Name: "auto-restore", Value: "on|off", Help: "Restore it onto another node when its node is down for long (default on)"},
		}, Run: backupsSet},
	{Name: "backups:unset", Help: "Stop backing a volume up (its backups stay in the bucket)",
		App: NeedsApp, Args: "<volume>", MinArgs: 1, MaxArgs: 1, Run: backupsUnset},
	{Name: "backups:run", Help: "Back a volume up now", App: NeedsApp, Args: "<volume>", MinArgs: 1, MaxArgs: 1, Run: backupsRun},
	{Name: "backups:list", Help: "List a volume's backups", App: NeedsApp, Args: "<volume>", MinArgs: 1, MaxArgs: 1, Run: backupsList},
	{Name: "backups:restore", Help: "Restore a volume from a backup (the latest without one); the app restarts with it",
		App: NeedsApp, Args: "<volume> [<backup>]", MinArgs: 1, MaxArgs: 2, Flags: []Flag{
			{Name: "node", Value: "node", Help: "Restore onto this node (when the volume's node is down)"},
			{Name: "skip-backup", Help: "Don't back the current data up first"},
			{Name: "force", Help: "Don't ask for confirmation"},
		}, Run: backupsRestore},
	{Name: "backups:report", Help: "Display where volumes (and the cluster) are backed up, and how their backups went",
		App: OptionalApp, AnyFlags: true, Run: backupsReport},
	{Name: "backups:cluster", Help: "Back the cluster itself up to a destination: the control node's database and identity, and recent releases",
		Args: "<destination> [<path>]", MinArgs: 1, MaxArgs: 2, Flags: []Flag{
			{Name: "every", Value: "duration", Help: "How often: 1h (the default), 6h, 1d, or off for only when asked"},
			{Name: "keep-recent", Value: "duration", Help: "Keep every backup this recent (default 24h)"},
			{Name: "keep-daily", Value: "days", Help: "And the last backup of each of this many days (default 30)"},
		}, Run: backupsCluster},
	{Name: "backups:cluster-unset", Help: "Stop backing the cluster up (its backups stay in the bucket)", Run: backupsClusterUnset},
	{Name: "backups:cluster-run", Help: "Back the cluster up now", Run: backupsClusterRun},
	{Name: "backups:cluster-list", Help: "List the cluster's backups", Run: backupsClusterList},
}

func backupsDestinationAdd(c *Context) error {
	req := types.CreateBackupDestinationRequest{
		Name: c.Args[0], Endpoint: c.String("endpoint"), Region: c.String("region"), Bucket: c.String("bucket"),
		AccessKeyID: c.String("access-key-id"), SecretAccessKey: c.String("secret-access-key"), NoEncrypt: c.Bool("no-encrypt"),
	}
	switch {
	case req.Endpoint == "":
		return usageErr("Which S3 endpoint? Pass --endpoint, e.g. https://fly.storage.tigris.dev")
	case req.Bucket == "":
		return usageErr("Which bucket? Pass --bucket")
	case req.AccessKeyID == "":
		return usageErr("Pass the access key ID with --access-key-id")
	}
	if req.SecretAccessKey == "" {
		secret, err := readSecret(c, "Secret access key: ")
		if err != nil {
			return err
		}
		req.SecretAccessKey = secret
	}
	if req.SecretAccessKey == "" {
		return usageErr("No secret access key given: type it when asked, or pipe it to stdin")
	}
	if !req.NoEncrypt {
		if err := ensureKeySaved(c); err != nil {
			return err
		}
	}
	d, err := c.API.CreateBackupDestination(c, req)
	if err != nil {
		return err
	}
	how := "encrypted"
	if !d.Encrypt {
		how = "unencrypted"
	}
	c.Step("Added %s: bucket %s at %s, %s", d.Name, d.Bucket, d.Endpoint, how)
	if d.BacksUpCluster {
		c.Info("The cluster itself is backed up there every hour, under %s (see jokku backups:cluster)", "jokku/cluster")
	}
	c.Info("Back a volume up there with: jokku backups:set <app> <volume> %s", d.Name)
	return nil
}

// readSecret asks for a secret without echoing it, or reads it from stdin
// when that isn't a terminal.
func readSecret(c *Context, prompt string) (string, error) {
	if f, ok := c.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(c.Stdout, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(c.Stdout)
		return strings.TrimSpace(string(b)), err
	}
	b, err := io.ReadAll(io.LimitReader(c.Stdin, 64<<10))
	return strings.TrimSpace(string(b)), err
}

// ensureKeySaved makes the backup key if there is none, and until the user
// has confirmed saving it, shows it and asks them to.
func ensureKeySaved(c *Context) error {
	k, err := c.API.CreateBackupKey(c)
	if err != nil {
		return err
	}
	if k.Saved {
		return nil
	}
	showKey(c, k)
	return confirmKeySaved(c, k)
}

func showKey(c *Context, k *types.BackupKey) {
	c.Header("The backup key")
	fmt.Fprintln(c.Stdout)
	fmt.Fprintf(c.Stdout, "    %s\n\n", k.Key)
	c.Info("Key ID %s", k.ID)
	c.Warn("Save this key somewhere safe, away from this cluster, such as a password manager.")
	c.Warn("Backups are encrypted with it, and nothing can restore them without it: not Jokku,")
	c.Warn("and not anyone else. Jokku keeps a copy, but if this cluster is lost, so is that.")
}

func confirmKeySaved(c *Context, k *types.BackupKey) error {
	if !c.Bool("force") {
		f, ok := c.Stdin.(*os.File)
		if !ok || !term.IsTerminal(int(f.Fd())) {
			return fmt.Errorf("confirm you saved the backup key above: run jokku backups:key in a terminal, or pass --force once it is saved")
		}
		want := k.Key[len(k.Key)-6:]
		fmt.Fprintf(c.Stdout, "\nTo confirm you saved it, type the key's last 6 characters: ")
		line, _ := bufio.NewReader(f).ReadString('\n')
		if strings.TrimSpace(line) != want {
			return fmt.Errorf("that doesn't match the key's last 6 characters; nothing was changed")
		}
	}
	if err := c.API.SaveBackupKey(c); err != nil {
		return err
	}
	c.Step("Backup key saved")
	return nil
}

func backupsKey(c *Context) error {
	k, err := c.API.CreateBackupKey(c)
	if err != nil {
		return err
	}
	showKey(c, k)
	if k.Saved {
		return nil
	}
	return confirmKeySaved(c, k)
}

func backupsDestinations(c *Context) error {
	dests, err := c.API.BackupDestinations(c)
	if err != nil {
		return err
	}
	c.Header("Backup destinations")
	if len(dests) == 0 {
		c.Info("none (add one with jokku backups:destination-add <name> --endpoint <url> --bucket <bucket> --access-key-id <id>)")
		return nil
	}
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tENDPOINT\tBUCKET\tREGION\tENCRYPTED")
	for _, d := range dests {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Name, d.Endpoint, d.Bucket, d.Region, yesNo(d.Encrypt))
	}
	return tw.Flush()
}

func backupsDestinationRemove(c *Context) error {
	if err := c.API.DeleteBackupDestination(c, c.Args[0]); err != nil {
		return err
	}
	c.Step("Removed %s; the backups in it are still there", c.Args[0])
	return nil
}

func backupsSet(c *Context) error {
	req := types.SetVolumeBackupRequest{Destination: c.Args[1], Every: c.String("every"), KeepRecent: c.String("keep-recent"),
		AutoRestore: c.String("auto-restore")}
	if len(c.Args) == 3 {
		req.Path = c.Args[2]
	}
	if s := c.String("keep-daily"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return usageErr("--keep-daily is a number of days, not %q", s)
		}
		req.KeepDaily = &n
	}
	out, err := c.API.SetVolumeBackup(c, c.App, c.Args[0], req)
	if err != nil {
		return err
	}
	c.Step("Backing up volume %s of %s to %s, under %s, %s", out.Volume, out.App, out.Destination, out.Path, schedule(out))
	c.Info("Keeping %s", retention(out))
	c.Info("If its node is down, %s", autoRestore(out))
	if out.EverySeconds > 0 {
		c.Info("The first backup starts within a minute; or now, with: jokku backups:run %s %s", c.App, out.Volume)
	}
	return nil
}

func backupsCluster(c *Context) error {
	req := types.SetVolumeBackupRequest{Destination: c.Args[0], Every: c.String("every"), KeepRecent: c.String("keep-recent")}
	if len(c.Args) == 2 {
		req.Path = c.Args[1]
	}
	if s := c.String("keep-daily"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return usageErr("--keep-daily is a number of days, not %q", s)
		}
		req.KeepDaily = &n
	}
	out, err := c.API.SetClusterBackup(c, req)
	if err != nil {
		return err
	}
	c.Step("Backing up the cluster to %s, under %s, %s", out.Destination, out.Path, schedule(out))
	c.Info("Keeping %s", retention(out))
	c.Info("To rebuild the control node from it on a new server: sudo jokku restore-cluster (see jokku help restore-cluster)")
	return nil
}

func backupsClusterUnset(c *Context) error {
	if err := c.API.UnsetClusterBackup(c); err != nil {
		return err
	}
	c.Step("No longer backing up the cluster; its backups are still in the bucket")
	return nil
}

func backupsClusterRun(c *Context) error {
	return c.API.RunClusterBackup(c, func(e types.Event) { fmt.Fprintln(c.Stdout, e.Message) })
}

func backupsClusterList(c *Context) error {
	out, err := c.API.ClusterBackups(c)
	if err != nil {
		return err
	}
	c.Header("Backups of the cluster, in %s %s", out.Destination, out.Path)
	return listBackups(c, out, "jokku backups:cluster-run")
}

func schedule(b *types.VolumeBackups) string {
	if b.EverySeconds == 0 {
		return "when asked (jokku backups:run)"
	}
	return "every " + shortDuration(time.Duration(b.EverySeconds)*time.Second)
}

func autoRestore(b *types.VolumeBackups) string {
	if !b.AutoRestore {
		return "it waits for that node (--auto-restore off)"
	}
	return fmt.Sprintf("it is restored onto another node from its latest backup after %s", shortDuration(time.Duration(b.FailoverSeconds)*time.Second))
}

func retention(b *types.VolumeBackups) string {
	var parts []string
	if b.KeepRecentSeconds > 0 {
		parts = append(parts, "every backup from the last "+shortDuration(time.Duration(b.KeepRecentSeconds)*time.Second))
	}
	switch {
	case b.KeepDaily == 1:
		parts = append(parts, "the last one of today")
	case b.KeepDaily > 1:
		parts = append(parts, fmt.Sprintf("the last one of each day for %d days", b.KeepDaily))
	}
	if len(parts) == 0 {
		return "only the latest backup"
	}
	return strings.Join(parts, ", and ")
}

// shortDuration writes 15m, 1h30m, 24h or 7d rather than 15m0s.
func shortDuration(d time.Duration) string {
	if d >= 48*time.Hour && d%(24*time.Hour) == 0 {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func backupsUnset(c *Context) error {
	if err := c.API.UnsetVolumeBackup(c, c.App, c.Args[0]); err != nil {
		return err
	}
	c.Step("No longer backing up volume %s of %s; its backups are still in the bucket", c.Args[0], c.App)
	return nil
}

func backupsRun(c *Context) error {
	return c.API.RunBackup(c, c.App, c.Args[0], func(e types.Event) { fmt.Fprintln(c.Stdout, e.Message) })
}

func backupsList(c *Context) error {
	out, err := c.API.VolumeBackups(c, c.App, c.Args[0])
	if err != nil {
		return err
	}
	c.Header("Backups of volume %s of %s, in %s %s", out.Volume, out.App, out.Destination, out.Path)
	return listBackups(c, out, fmt.Sprintf("jokku backups:run %s %s", c.App, out.Volume))
}

func listBackups(c *Context, out *types.VolumeBackups, runNow string) error {
	c.Info("%d backups, %s stored; %s, keeping %s", out.StoredBackups, humanBytes(out.StoredBytes), schedule(out), retention(out))
	if out.Last != nil && out.Last.Status == types.StatusFailed {
		c.Warn("The latest backup, %s, failed: %s", out.Last.Name, out.Last.Error)
	}
	if len(out.Backups) == 0 {
		c.Info("none yet (back it up now with %s)", runNow)
		return nil
	}
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "BACKUP\tTAKEN\tADDED")
	for _, b := range out.Backups {
		added := "-"
		if b.NewBlocks > 0 || b.NewBytes > 0 {
			added = humanBytes(b.NewBytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Name, ago(b.Time), added)
	}
	return tw.Flush()
}

func backupsRestore(c *Context) error {
	req := types.RestoreVolumeRequest{Node: c.String("node"), SkipBackup: c.Bool("skip-backup")}
	what := "the latest backup"
	if len(c.Args) == 2 {
		req.Backup = c.Args[1]
		what = "backup " + req.Backup
	}
	if !c.Bool("force") {
		if err := c.confirm(fmt.Sprintf("replace the data of volume %s of %s with %s, and restart the app", c.Args[0], c.App, what), c.App); err != nil {
			return err
		}
	}
	return c.API.RestoreVolume(c, c.App, c.Args[0], req, func(e types.Event) { fmt.Fprintln(c.Stdout, e.Message) })
}

func backupsReport(c *Context) error {
	out, err := c.API.BackupReport(c, c.App)
	if err != nil {
		return err
	}
	if len(out) == 0 {
		c.Header("Backups")
		c.Info("No volume is backed up (set one up with jokku backups:set <app> <volume> <destination>)")
		return nil
	}
	for _, b := range out {
		title := fmt.Sprintf("%s %s backups", b.App, b.Volume)
		if b.Cluster {
			title = "Cluster backups (the control node and recent releases)"
		}
		last, lastErr, next := "never", "", ""
		if b.Succeeded != nil {
			last = fmt.Sprintf("%s (%s)", b.Succeeded.Name, ago(b.Succeeded.FinishedAt))
		}
		if b.Last != nil && b.Last.Status == types.StatusFailed {
			lastErr = fmt.Sprintf("%s: %s", b.Last.Name, b.Last.Error)
		}
		if !b.Next.IsZero() {
			next = "within a minute"
			if d := time.Until(b.Next).Round(time.Minute); d >= time.Minute {
				next = "in " + shortDuration(d)
			}
		}
		rows := []row{
			{"backups-destination", "Backups destination", b.Destination},
			{"backups-path", "Backups path", b.Path},
			{"backups-schedule", "Backups schedule", schedule(&b)},
			{"backups-keep", "Backups keep", retention(&b)},
		}
		if !b.Cluster {
			rows = append(rows, row{"backups-auto-restore", "Backups auto restore", autoRestore(&b)})
		}
		if err := c.report(title, append(rows, []row{
			{"backups-last", "Backups last", last},
			{"backups-next", "Backups next", next},
			{"backups-stored", "Backups stored", fmt.Sprintf("%s in %d backups", humanBytes(b.StoredBytes), b.StoredBackups)},
			{"backups-last-failure", "Backups last failure", lastErr},
		}...)); err != nil {
			return err
		}
	}
	return nil
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
