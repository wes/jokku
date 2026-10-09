package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

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
	{Name: "backups:set", Help: "Back a volume up to a destination, under a path (default: jokku/<app>/<volume>)",
		App: NeedsApp, Args: "<volume> <destination> [<path>]", MinArgs: 2, MaxArgs: 3, Run: backupsSet},
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
	{Name: "backups:report", Help: "Display where volumes are backed up, and how their backups went",
		App: OptionalApp, AnyFlags: true, Run: backupsReport},
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
	req := types.SetVolumeBackupRequest{Destination: c.Args[1]}
	if len(c.Args) == 3 {
		req.Path = c.Args[2]
	}
	out, err := c.API.SetVolumeBackup(c, c.App, c.Args[0], req)
	if err != nil {
		return err
	}
	c.Step("Backing up volume %s of %s to %s, under %s", out.Volume, out.App, out.Destination, out.Path)
	c.Info("Back it up now with: jokku backups:run %s %s", c.App, out.Volume)
	return nil
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
	if out.Last != nil && out.Last.Status == types.StatusFailed {
		c.Warn("The latest backup, %s, failed: %s", out.Last.Name, out.Last.Error)
	}
	if len(out.Backups) == 0 {
		c.Info("none yet (back it up now with jokku backups:run %s %s)", c.App, out.Volume)
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
		last, lastErr := "never", ""
		if b.Succeeded != nil {
			last = fmt.Sprintf("%s (%s)", b.Succeeded.Name, ago(b.Succeeded.FinishedAt))
		}
		if b.Last != nil && b.Last.Status == types.StatusFailed {
			lastErr = fmt.Sprintf("%s: %s", b.Last.Name, b.Last.Error)
		}
		if err := c.report(fmt.Sprintf("%s %s backups", b.App, b.Volume), []row{
			{"backups-destination", "Backups destination", b.Destination},
			{"backups-path", "Backups path", b.Path},
			{"backups-last", "Backups last", last},
			{"backups-last-failure", "Backups last failure", lastErr},
		}); err != nil {
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
