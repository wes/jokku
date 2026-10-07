package cli

import (
	"strings"

	"github.com/wes/jokku/internal/types"
)

var domainsCommands = []*Command{
	{Name: "domains:report", Help: "Display domain information", App: OptionalApp, AnyFlags: true,
		Flags: []Flag{{Name: "global", Help: "Show only the global domains"}}, Run: domainsReport},
	{Name: "domains:add", Help: "Add domains to an app", App: NeedsApp, Args: "<domain> [<domain> ...]", MinArgs: 1, MaxArgs: -1, Run: domainsPatch("add")},
	{Name: "domains:remove", Help: "Remove domains from an app", App: NeedsApp, Args: "<domain> [<domain> ...]", MinArgs: 1, MaxArgs: -1, Run: domainsPatch("remove")},
	{Name: "domains:set", Help: "Replace an app's domains", App: NeedsApp, Args: "<domain> [<domain> ...]", MinArgs: 1, MaxArgs: -1, Run: domainsPatch("set")},
	{Name: "domains:clear", Help: "Remove every domain from an app", App: NeedsApp, Run: domainsPatch("clear")},
	{Name: "domains:add-global", Help: "Add global domains (apps get <app>.<domain>)", Args: "<domain> [<domain> ...]", MinArgs: 1, MaxArgs: -1, Run: domainsPatch("add")},
	{Name: "domains:remove-global", Help: "Remove global domains", Args: "<domain> [<domain> ...]", MinArgs: 1, MaxArgs: -1, Run: domainsPatch("remove")},
	{Name: "domains:set-global", Help: "Replace the global domains", Args: "<domain> [<domain> ...]", MinArgs: 1, MaxArgs: -1, Run: domainsPatch("set")},
	{Name: "domains:clear-global", Help: "Remove every global domain", Run: domainsPatch("clear")},
}

func domainsPatch(op string) func(*Context) error {
	return func(c *Context) error {
		var p types.DomainsPatch
		switch op {
		case "add":
			p.Add = c.Args
		case "remove":
			p.Remove = c.Args
		case "set":
			p.Set = c.Args
		case "clear":
			p.Clear = true
		}
		if _, err := c.API.PatchDomains(c, c.App, p); err != nil {
			return err
		}
		target := c.App
		if target == "" {
			target = "global domains"
		}
		list := strings.Join(c.Args, " ")
		switch op {
		case "add":
			c.Step("Added %s to %s", list, target)
		case "remove":
			c.Step("Removed %s from %s", list, target)
		case "set":
			c.Step("Set %s for %s", list, target)
		case "clear":
			c.Step("Cleared domains for %s", target)
		}
		return nil
	}
}

func domainsReport(c *Context) error {
	if c.Global() {
		d, err := c.API.Domains(c, "")
		if err != nil {
			return err
		}
		return c.report("Global domains information", []row{
			{"domains-global-enabled", "Domains global enabled", yesNo(len(d.Global) > 0)},
			{"domains-global-vhosts", "Domains global vhosts", strings.Join(d.Global, " ")},
		})
	}
	return forEachApp(c, func(a *types.App) error {
		d, err := c.API.Domains(c, a.Name)
		if err != nil {
			return err
		}
		return c.report(a.Name+" domains information", []row{
			{"domains-app-enabled", "Domains app enabled", yesNo(d.Enabled)},
			{"domains-app-vhosts", "Domains app vhosts", strings.Join(d.App, " ")},
			{"domains-global-enabled", "Domains global enabled", yesNo(len(d.Global) > 0)},
			{"domains-global-vhosts", "Domains global vhosts", strings.Join(d.Global, " ")},
		})
	})
}
