package cli

import (
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wes/jokku/internal/database"
	"github.com/wes/jokku/internal/types"
)

// The db: commands: db:list, and for each engine db:<engine>:create, link,
// connect, export and the rest. A database runs as the app
// <engine>-<name>, which these commands find from its engine and name.

func databaseCommands() []*Command {
	cmds := []*Command{{Name: "db:list", Help: "List the databases: Postgres, MySQL and Redis", Run: dbList("")}}
	for _, name := range database.Names() {
		e, _ := database.Lookup(name)
		p := "db:" + name + ":"
		title := map[string]string{"postgres": "Postgres", "mysql": "MySQL", "redis": "Redis"}[name]
		cmds = append(cmds,
			&Command{Name: p + "create", Help: fmt.Sprintf("Create a %s database (%s, %s of memory)", title, e.ImageRef("", ""), formatMemory(e.MemoryMB)),
				Args: "<name>", MinArgs: 1, MaxArgs: 1, Flags: []Flag{
					{Name: "image-version", Value: "tag", Help: "The image's tag (default " + e.Version + ")"},
					{Name: "image", Value: "image", Help: "Another image (default " + e.Image + ")"},
					{Name: "size", Value: "size", Help: "Its volume's size (default 10g)"},
					{Name: "memory", Value: "size", Help: "Its memory (default " + formatMemory(e.MemoryMB) + ")"},
				}, Run: dbCreate(e)},
			&Command{Name: p + "list", Help: "List the " + title + " databases", Run: dbList(name)},
			&Command{Name: p + "info", Help: "Display a " + title + " database's status, address, links and backups (--dsn prints its URL)",
				Args: "<name>", MinArgs: 1, MaxArgs: 1, AnyFlags: true, Run: dbInfo(e)},
			&Command{Name: p + "link", Help: fmt.Sprintf("Link a %s database to an app: sets %s on it, and restarts it", title, e.EnvVar),
				Args: "<name> <app>", MinArgs: 2, MaxArgs: 2, Flags: []Flag{
					{Name: "alias", Value: "NAME", Help: "Set <NAME>_URL instead of " + e.EnvVar},
					{Name: "no-restart", Help: "Don't restart the app"},
				}, Run: dbLink(e)},
			&Command{Name: p + "unlink", Help: "Unlink a " + title + " database from an app, and restart it",
				Args: "<name> <app>", MinArgs: 2, MaxArgs: 2, Flags: []Flag{{Name: "no-restart", Help: "Don't restart the app"}}, Run: dbUnlink(e)},
			&Command{Name: p + "connect", Help: "Open a " + map[string]string{"postgres": "psql", "mysql": "mysql", "redis": "redis-cli"}[name] + " shell in a " + title + " database",
				Args: "<name>", MinArgs: 1, MaxArgs: 1, Run: dbConnect(e)},
			&Command{Name: p + "export", Help: "Write a " + title + " database's contents to stdout, as " + e.Dump,
				Args: "<name>", MinArgs: 1, MaxArgs: 1, Run: dbExport(e)},
			&Command{Name: p + "logs", Help: "Display a " + title + " database's log output",
				Args: "<name>", MinArgs: 1, MaxArgs: 1, Flags: []Flag{
					{Name: "tail", Short: "t", Help: "Keep following new output"},
					{Name: "num", Short: "n", Value: "N", Help: "Number of lines to show (default 100)"},
				}, Run: dbApp(e, logs)},
			&Command{Name: p + "restart", Help: "Restart a " + title + " database", Args: "<name>", MinArgs: 1, MaxArgs: 1,
				Run: dbApp(e, func(c *Context) error { return c.ps("restart") })},
			&Command{Name: p + "stop", Help: "Stop a " + title + " database", Args: "<name>", MinArgs: 1, MaxArgs: 1,
				Run: dbApp(e, func(c *Context) error { return c.ps("stop") })},
			&Command{Name: p + "start", Help: "Start a stopped " + title + " database", Args: "<name>", MinArgs: 1, MaxArgs: 1,
				Run: dbApp(e, func(c *Context) error { return c.ps("start") })},
			&Command{Name: p + "destroy", Help: "Delete a " + title + " database and its data (its backups stay in their bucket)",
				Args: "<name>", MinArgs: 1, MaxArgs: 1, Flags: []Flag{forceFlag}, Run: dbDestroy(e)},
		)
		if _, _, ok := e.ImportCommand("", nil); ok {
			cmds = append(cmds, &Command{Name: p + "import", Help: "Load an export (" + e.Dump + ") into a " + title + " database, from stdin",
				Args: "<name>", MinArgs: 1, MaxArgs: 1, Run: dbImport(e)})
		}
	}
	return cmds
}

// dbApp runs an app command (logs, ps) on a database's app.
func dbApp(e database.Engine, run func(*Context) error) func(*Context) error {
	return func(c *Context) error {
		if _, err := c.API.Database(c, e.Name, c.Args[0]); err != nil {
			return err
		}
		c.App = e.AppName(c.Args[0])
		return run(c)
	}
}

func dbCreate(e database.Engine) func(*Context) error {
	return func(c *Context) error {
		req := types.CreateDatabaseRequest{Name: c.Args[0], Image: c.String("image"), ImageVersion: c.String("image-version")}
		for flag, to := range map[string]*int{"size": &req.SizeMB, "memory": &req.MemoryMB} {
			if s := c.String(flag); s != "" {
				mb, err := parseSize(s)
				if err != nil {
					return err
				}
				*to = mb
			}
		}
		return c.API.CreateDatabase(c, e.Name, req, func(ev types.Event) { fmt.Fprintln(c.Stdout, ev.Message) })
	}
}

func dbList(engine string) func(*Context) error {
	return func(c *Context) error {
		dbs, err := c.API.Databases(c, engine)
		if err != nil {
			return err
		}
		if len(dbs) == 0 {
			what := "databases"
			if engine != "" {
				what = engine + " databases"
			}
			c.Header("No %s yet", what)
			c.Info("Create one with: jokku db:%s:create <name>", map[bool]string{true: "postgres", false: engine}[engine == ""])
			return nil
		}
		tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tENGINE\tIMAGE\tSTATUS\tNODE\tLINKED TO\tBACKUPS")
		for _, db := range dbs {
			links := "-"
			if len(db.Links) > 0 {
				var apps []string
				for _, l := range db.Links {
					apps = append(apps, l.App)
				}
				links = strings.Join(apps, ",")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", db.Name, db.Engine, db.Image, db.Status, orDash(db.Node), links, backupSummary(db.Backups))
		}
		return tw.Flush()
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// backupSummary says in a few words how a database is backed up.
func backupSummary(b *types.VolumeBackups) string {
	switch {
	case b == nil:
		return "not backed up"
	case b.Last != nil && b.Last.Status == types.StatusFailed:
		return "failed " + ago(b.Last.StartedAt)
	case b.Succeeded != nil:
		return "every " + shortDuration(time.Duration(b.EverySeconds)*time.Second) + ", last " + ago(b.Succeeded.FinishedAt)
	}
	return "every " + shortDuration(time.Duration(b.EverySeconds)*time.Second) + ", none yet"
}

func dbInfo(e database.Engine) func(*Context) error {
	return func(c *Context) error {
		db, err := c.API.Database(c, e.Name, c.Args[0])
		if err != nil {
			return err
		}
		status := db.Status
		if db.Node != "" {
			status += " on " + db.Node
		}
		var links []string
		for _, l := range db.Links {
			links = append(links, l.App+" ("+l.Var+")")
		}
		volume := ""
		if v := db.Volume; v != nil {
			volume = fmt.Sprintf("%s used of %s", formatMemory(v.UsedMB), formatMemory(v.SizeMB))
		}
		dsn := db.URL
		if len(c.extra) == 0 {
			dsn = maskPassword(db.URL) // the full URL with --dsn
		}
		return c.report(fmt.Sprintf("%s %s information", db.Name, db.Engine), []row{
			{"status", "Status", status},
			{"version", "Image", db.Image},
			{"host", "Host", db.Host},
			{"dsn", "Dsn", dsn},
			{"links", "Links", strings.Join(links, ", ")},
			{"memory", "Memory", formatMemory(db.MemoryMB)},
			{"volume", "Volume", volume},
			{"backups", "Backups", backupSummary(db.Backups)},
			{"app", "App", db.App},
		})
	}
}

// maskPassword hides a URL's password.
func maskPassword(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return strings.Replace(u.String(), "xxxxx", "•••••", 1)
}

func dbLink(e database.Engine) func(*Context) error {
	return func(c *Context) error {
		req := types.LinkDatabaseRequest{App: c.Args[1], Alias: c.String("alias"), NoRestart: c.Bool("no-restart")}
		return c.API.LinkDatabase(c, e.Name, c.Args[0], req, func(ev types.Event) { fmt.Fprintln(c.Stdout, ev.Message) })
	}
}

func dbUnlink(e database.Engine) func(*Context) error {
	return func(c *Context) error {
		return c.API.UnlinkDatabase(c, e.Name, c.Args[0], c.Args[1], c.Bool("no-restart"), func(ev types.Event) { fmt.Fprintln(c.Stdout, ev.Message) })
	}
}

func dbConnect(e database.Engine) func(*Context) error {
	return func(c *Context) error {
		db, err := c.API.Database(c, e.Name, c.Args[0])
		if err != nil {
			return err
		}
		argv, env := e.ConnectCommand(db.Name, db.Vars)
		return runSession(c, db.App, e.Process(), argv, env, false)
	}
}

func dbExport(e database.Engine) func(*Context) error {
	return func(c *Context) error {
		if isTerminal(c.Stdout) {
			return fmt.Errorf("db:%s:export writes %s to stdout; send it to a file: jokku db:%s:export %s > %s.dump (over ssh, without -t)",
				e.Name, e.Dump, e.Name, c.Args[0], c.Args[0])
		}
		db, err := c.API.Database(c, e.Name, c.Args[0])
		if err != nil {
			return err
		}
		argv, env := e.ExportCommand(db.Name, db.Vars)
		return runSession(c, db.App, e.Process(), argv, env, false)
	}
}

func dbImport(e database.Engine) func(*Context) error {
	return func(c *Context) error {
		if isTerminal(c.Stdin) {
			return fmt.Errorf("db:%s:import reads %s from stdin: jokku db:%s:import %s < %s.dump (over ssh, without -t)",
				e.Name, e.Dump, e.Name, c.Args[0], c.Args[0])
		}
		db, err := c.API.Database(c, e.Name, c.Args[0])
		if err != nil {
			return err
		}
		argv, env, _ := e.ImportCommand(db.Name, db.Vars)
		return runSession(c, db.App, e.Process(), argv, env, false)
	}
}

func dbDestroy(e database.Engine) func(*Context) error {
	return func(c *Context) error {
		name := c.Args[0]
		if _, err := c.API.Database(c, e.Name, name); err != nil {
			return err
		}
		if !c.Bool("force") {
			if err := c.confirm(fmt.Sprintf("delete %s database %s and all its data (its backups stay in their bucket)", e.Name, name), name); err != nil {
				return err
			}
		}
		if err := c.API.DestroyDatabase(c, e.Name, name); err != nil {
			return err
		}
		c.Step("Destroyed %s database %s", e.Name, name)
		return nil
	}
}
