package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/wes/jokku/internal/database"
	"github.com/wes/jokku/internal/types"
)

// dbUserCommands manage a database's logins, for the engines whose users
// Jokku manages (database.Engine.HasUsers).
func dbUserCommands(e database.Engine, p, title string) []*Command {
	if !e.HasUsers() {
		return nil
	}
	return []*Command{
		{Name: p + "users:list", Help: "List a " + title + " database's users, and what each may do (--dsn prints their URLs)",
			Args: "<name>", MinArgs: 1, MaxArgs: 1, Flags: []Flag{{Name: "dsn", Help: "Show the URLs of the users Jokku added, with their passwords"}},
			Run: dbUsersList(e)},
		{Name: p + "users:add", Help: "Add a user to a " + title + " database, who reads and writes its data (not its schema)",
			Args: "<name> <user>", MinArgs: 2, MaxArgs: 2, Flags: []Flag{{Name: "read-only", Help: "Only let it read"}},
			Run: dbUsersAdd(e)},
		{Name: p + "users:passwd", Help: "Give a " + title + " database's user a new password",
			Args: "<name> <user>", MinArgs: 2, MaxArgs: 2, Run: dbUsersPasswd(e)},
		{Name: p + "users:remove", Help: "Remove a user from a " + title + " database, ending its sessions",
			Args: "<name> <user>", MinArgs: 2, MaxArgs: 2, Run: dbUsersRemove(e)},
	}
}

func dbUsersList(e database.Engine) func(*Context) error {
	return func(c *Context) error {
		users, err := c.API.DatabaseUsers(c, e.Name, c.Args[0])
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "USER\tACCESS\tURL")
		for _, u := range users {
			url := u.URL
			switch {
			case url == "":
				url = "-" // not one Jokku added: its password isn't known
			case !c.Bool("dsn"):
				url = maskPassword(url)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", u.Name, u.Access, url)
		}
		return tw.Flush()
	}
}

func dbUsersAdd(e database.Engine) func(*Context) error {
	return func(c *Context) error {
		u, err := c.API.AddDatabaseUser(c, e.Name, c.Args[0], types.AddDatabaseUserRequest{Name: c.Args[1], ReadOnly: c.Bool("read-only")})
		if err != nil {
			return err
		}
		c.Step("Added %s user %s to %s", u.Access, u.Name, c.Args[0])
		printUserURLs(c, e, u)
		return nil
	}
}

func dbUsersPasswd(e database.Engine) func(*Context) error {
	return func(c *Context) error {
		u, err := c.API.DatabaseUserPassword(c, e.Name, c.Args[0], c.Args[1])
		if err != nil {
			return err
		}
		c.Step("Gave %s a new password; its old one no longer works", u.Name)
		printUserURLs(c, e, u)
		return nil
	}
}

func printUserURLs(c *Context, e database.Engine, u *types.DatabaseUser) {
	c.Info("Apps connect with: %s", u.URL)
	if u.PublicURL != "" {
		c.Info("Clients outside:   %s", u.PublicURL)
	}
	c.Info("(again later: jokku db:%s:users:list %s --dsn)", e.Name, c.Args[0])
}

func dbUsersRemove(e database.Engine) func(*Context) error {
	return func(c *Context) error {
		if err := c.API.RemoveDatabaseUser(c, e.Name, c.Args[0], c.Args[1]); err != nil {
			return err
		}
		c.Step("Removed %s from %s; its sessions are closed", c.Args[1], c.Args[0])
		return nil
	}
}

// dbStatsRows are what db:<engine>:info shows of what's been seen inside a
// database.
func dbStatsRows(st *types.DatabaseStats) []row {
	conns, queries, latency, users := "", "", "", ""
	if st != nil && !st.At.IsZero() {
		conns = fmt.Sprintf("%d of %d", st.Connections, st.MaxConnections)
		if st.Active > 0 {
			conns += fmt.Sprintf(" (%d running a query)", st.Active)
		}
		if st.MaxUsed > 0 {
			conns += fmt.Sprintf(", at most %d since it started", st.MaxUsed)
		}
		queries = fmt.Sprintf("%s/s over the last minute, %s/s over the last hour", perSecond(st.Minute.PerSecond), perSecond(st.Hour.PerSecond))
		if st.Hour.HitRate >= 0 {
			queries += fmt.Sprintf(", %.1f%% %s hits", st.Hour.HitRate*100, st.HitsAre)
		}
		latency = latencyText(st.Hour, st.Per) + " over the last hour"
		var us []string
		for _, u := range st.Users {
			us = append(us, u.Name+" ("+u.Access+")")
		}
		users = strings.Join(us, ", ")
	}
	if st != nil && st.Error != "" {
		conns = "can't look inside: " + st.Error
	}
	if st != nil && st.Note != "" && st.Note != "stopped" {
		latency = strings.TrimSpace(latency + "; " + st.Note)
		latency = strings.TrimPrefix(latency, "; ")
	}
	return []row{
		{"connections", "Connections", conns},
		{"queries", "Queries", queries},
		{"latency", "Latency", latency},
		{"users", "Users", users},
	}
}

// latencyText is how long w's queries took: percentiles, or the mean of
// each "per" (a transaction, for Postgres without pg_stat_statements).
func latencyText(w types.DatabaseWindow, per string) string {
	switch {
	case w.Queries == 0:
		return "no queries"
	case w.P50 > 0:
		return fmt.Sprintf("p50 %s, p95 %s, p99 %s", dbMS(w.P50), dbMS(w.P95), dbMS(w.P99))
	case w.AvgMS > 0:
		return fmt.Sprintf("%s per %s on average", dbMS(w.AvgMS), per)
	}
	return "-"
}

func perSecond(f float64) string {
	if f < 10 {
		return fmt.Sprintf("%.1f", f)
	}
	return fmt.Sprintf("%.0f", f)
}

// dbMS is a query's time: microseconds for Redis's, milliseconds for most.
func dbMS(ms float64) string {
	switch {
	case ms < 1:
		return fmt.Sprintf("%.0f µs", max(ms*1000, 1))
	case ms < 10:
		return fmt.Sprintf("%.1f ms", ms)
	case ms < 1000:
		return fmt.Sprintf("%.0f ms", ms)
	}
	return fmt.Sprintf("%.1f s", ms/1000)
}
