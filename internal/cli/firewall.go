package cli

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wes/jokku/internal/types"
)

var firewallCommands = []*Command{
	{Name: "firewall:report", Help: "Show the addresses the firewall banned and why, what it blocked, and its rules", Run: firewallReport},
	{Name: "firewall:unban", Help: "Let a banned address in again", Args: "<ip>", MinArgs: 1, Run: firewallUnban},
	{Name: "firewall:disable", Help: "Turn the firewall off for an app (one that answers many pages not found on purpose), or with --global for all",
		App: AppOrGlobal, Run: firewallEnable(false)},
	{Name: "firewall:enable", Help: "Turn the firewall back on for an app, or with --global for all", App: AppOrGlobal, Run: firewallEnable(true)},
	{Name: "firewall:set", Help: "Change a rule for every app: not-found (pages that ban an address), window, ban (how long), rate (requests a second, 0 for no limit) or burst; no value for the default",
		Args: "<not-found|window|ban|rate|burst> [<value>]", MinArgs: 1, MaxArgs: 2, Run: firewallSet},
	{Name: "firewall:add-allowed-ip", Help: "Never ban or slow an address (or CIDR); private addresses never are", Args: "<ip|cidr>", MinArgs: 1,
		Run: firewallAllow(true)},
	{Name: "firewall:remove-allowed-ip", Help: "Treat an allowed address (or CIDR) like any other again", Args: "<ip|cidr>", MinArgs: 1,
		Run: firewallAllow(false)},
}

func init() {
	namespaceHelp["firewall"] = "Ban addresses that scan sites for weaknesses, and slow floods of requests"
}

func firewallReport(c *Context) error {
	fw, err := c.API.Firewall(c)
	if err != nil {
		return err
	}
	s := fw.Settings
	status := "on"
	if !s.Enabled {
		status = "off (jokku firewall:enable --global)"
	} else if len(s.OffForApps) > 0 {
		status = "on, except for " + strings.Join(s.OffForApps, ", ")
	}
	rate := "none"
	if s.Rate > 0 {
		rate = fmt.Sprintf("%d requests a second, bursts of %d", s.Rate, s.Burst)
	}
	rows := []row{
		{"firewall-status", "Status", status},
		{"firewall-ban", "Ban", fmt.Sprintf("%d pages not found within %s bans an address for %s", s.NotFound, s.Window, s.Ban)},
		{"firewall-rate", "Rate limit", rate},
		{"firewall-allowed", "Allowed", strings.Join(append([]string{"private addresses"}, s.Allow...), ", ")},
		{"firewall-last-hour", "Last hour", firewallCounts(fw.Hour)},
		{"firewall-last-day", "Last day", firewallCounts(fw.Day)},
	}
	if len(fw.Missing) > 0 {
		rows = append(rows, row{"firewall-missing", "Without it", strings.Join(fw.Missing, ", ") + " (a release without the firewall: jokku update)"})
	}
	if err := c.report("Firewall", rows); err != nil || len(c.extra) > 0 {
		return err
	}
	if !s.Enabled {
		return nil
	}
	c.Header("Banned (%d)", len(fw.Bans))
	if len(fw.Bans) > 0 {
		tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ADDRESS\tSITE\tBANNED\tLEFT\tBLOCKED\tON\tPAGES NOT FOUND")
		for _, b := range fw.Bans {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", b.Addr, orDash(b.App), ago(b.At), left(b.Until), b.Blocked,
				strings.Join(b.Nodes, ","), strings.Join(b.Paths, " "))
		}
		tw.Flush()
	}
	if len(fw.Watching) > 0 {
		c.Header("Close to a ban")
		tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ADDRESS\tSITE\tNOT FOUND\tLAST")
		for _, w := range fw.Watching {
			fmt.Fprintf(tw, "%s\t%s\t%d of %d\t%s\n", w.Addr, orDash(w.App), w.NotFound, s.NotFound, ago(w.Last))
		}
		tw.Flush()
	}
	if len(fw.Slowed) > 0 {
		c.Header("Slowed in the last hour")
		tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ADDRESS\tSITE\tREQUESTS SLOWED\tLAST")
		for _, l := range fw.Slowed {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", l.Addr, orDash(l.App), l.Limited, ago(l.Last))
		}
		tw.Flush()
	}
	return nil
}

// firewallCounts is a span's requests, and what the firewall did with them.
func firewallCounts(w types.FirewallCounts) string {
	total := w.Requests + w.Blocked + w.Limited
	if total == 0 {
		return "no requests"
	}
	return fmt.Sprintf("%d of %d requests blocked (%.0f%%), %d slowed, %d %s", w.Blocked, total,
		float64(w.Blocked)/float64(total)*100, w.Limited, w.Bans, plural(int(w.Bans), "ban", "bans"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// left is how long until t, roughly.
func left(t time.Time) string {
	d := time.Until(t)
	switch {
	case d <= 0:
		return "0"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func firewallUnban(c *Context) error {
	had, err := c.API.Unban(c, c.Args[0])
	if err != nil {
		return err
	}
	if !had {
		c.Step("%s isn't banned (if a proxy bans it in the meantime, that ban is lifted too)", c.Args[0])
		return nil
	}
	c.Step("Unbanned %s: the proxies let it in within a few seconds", c.Args[0])
	return nil
}

func firewallEnable(on bool) func(*Context) error {
	return func(c *Context) error {
		app := c.App
		if c.Global() {
			app = ""
		}
		if _, err := c.API.SetFirewall(c, app, types.FirewallPatch{Enabled: &on}); err != nil {
			return err
		}
		state := map[bool]string{true: "on", false: "off"}[on]
		if app == "" {
			c.Step("The firewall is %s", state)
			if on {
				c.Info("Apps it's off for (firewall:disable <app>) stay that way: see jokku firewall:report")
			}
			return nil
		}
		c.Step("The firewall is %s for %s", state, app)
		return nil
	}
}

func firewallSet(c *Context) error {
	key, value := c.Args[0], ""
	if len(c.Args) == 2 {
		value = c.Args[1]
	}
	var p types.FirewallPatch
	number := func(def int, set **int) error {
		n := def
		if value != "" {
			var err error
			if n, err = strconv.Atoi(value); err != nil {
				return usageErr("%s is a number", key)
			}
		}
		*set = &n
		return nil
	}
	duration := func(def time.Duration, set **string) {
		v := value
		if v == "" {
			v = def.String()
		}
		*set = &v
	}
	var err error
	switch key {
	case "not-found":
		err = number(types.DefaultFirewallNotFound, &p.NotFound)
	case "window":
		duration(types.DefaultFirewallWindow, &p.Window)
	case "ban":
		duration(types.DefaultFirewallBan, &p.Ban)
	case "rate":
		err = number(types.DefaultFirewallRate, &p.Rate)
	case "burst":
		err = number(types.DefaultFirewallBurst, &p.Burst)
	default:
		return usageErr("Unknown rule %q, expected not-found, window, ban, rate or burst", key)
	}
	if err != nil {
		return err
	}
	s, err := c.API.SetFirewall(c, "", p)
	if err != nil {
		return err
	}
	c.Step("%d pages not found within %s ban an address for %s", s.NotFound, s.Window, s.Ban)
	if s.Rate > 0 {
		c.Info("Each address may make %d requests a second, in bursts of %d", s.Rate, s.Burst)
	} else {
		c.Info("Addresses may make any number of requests a second")
	}
	return nil
}

func firewallAllow(add bool) func(*Context) error {
	return func(c *Context) error {
		fw, err := c.API.Firewall(c)
		if err != nil {
			return err
		}
		v, err := allowEntry(c.Args[0])
		if err != nil {
			return err
		}
		list := fw.Settings.Allow
		switch {
		case add && slices.Contains(list, v):
			c.Step("%s is already allowed", v)
			return nil
		case add:
			list = append(list, v)
		case !slices.Contains(list, v):
			return fmt.Errorf("%s isn't allowed: the allowed addresses are %s", v, orNone(list))
		default:
			list = slices.DeleteFunc(list, func(s string) bool { return s == v })
		}
		s, err := c.API.SetFirewall(c, "", types.FirewallPatch{Allow: &list})
		if err != nil {
			return err
		}
		c.Step("The firewall never bans nor slows: private addresses, %s", orNone(s.Allow))
		return nil
	}
}

// allowEntry is an address or range as the firewall's settings keep it.
func allowEntry(s string) (string, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		if p = p.Masked(); p.IsSingleIP() {
			return p.Addr().String(), nil
		}
		return p.String(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return "", usageErr("%q isn't an IP address or range (CIDR)", s)
	}
	return a.Unmap().String(), nil
}
