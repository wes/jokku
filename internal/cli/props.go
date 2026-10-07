package cli

import (
	"strings"

	"github.com/wes/jokku/internal/props"
	"github.com/wes/jokku/internal/types"
)

// propertyCommands generates Dokku's "<plugin>:set", "<plugin>:report" and,
// for plugins with an "enabled" key, "<plugin>:enable" / "<plugin>:disable".
// Commands registered by hand (ps:report) take precedence.
func propertyCommands() []*Command {
	var out []*Command
	add := func(c *Command) {
		if _, exists := commands[c.Name]; !exists {
			out = append(out, c)
		}
	}
	for _, p := range props.Plugins {
		if p.Settable {
			var keys []string
			for _, k := range p.Keys {
				keys = append(keys, k.Name)
			}
			add(&Command{
				Name: p.Name + ":set", Help: "Set or clear a " + p.Name + " property (" + strings.Join(keys, ", ") + ")",
				App: AppOrGlobal, Args: "<key> [<value>]", MinArgs: 1, MaxArgs: 2, Run: propertySet(p),
			})
		}
		if _, ok := p.Key("enabled"); ok {
			add(&Command{Name: p.Name + ":enable", Help: "Enable " + p.Name + " for an app", App: NeedsApp, Run: propertyToggle(p, "true")})
			add(&Command{Name: p.Name + ":disable", Help: "Disable " + p.Name + " for an app", App: NeedsApp, Run: propertyToggle(p, "false")})
		}
		add(&Command{
			Name: p.Name + ":report", Help: "Display " + p.Name + " information", App: OptionalApp, AnyFlags: true,
			Flags: []Flag{{Name: "global", Help: "Show only global values"}}, Run: propertyReport(p),
		})
	}
	return out
}

func propertySet(p props.Plugin) func(*Context) error {
	return func(c *Context) error {
		key, value := c.Args[0], ""
		if len(c.Args) == 2 {
			value = c.Args[1]
		}
		if err := props.Check(p.Name, key, value); err != nil {
			return usageErr("%v", err)
		}
		if err := c.API.SetProperty(c, c.App, p.Name, key, value); err != nil {
			return err
		}
		if value == "" {
			c.Step("Unsetting %s", key)
		} else {
			c.Step("Setting %s to %s", key, value)
		}
		return nil
	}
}

func propertyToggle(p props.Plugin, value string) func(*Context) error {
	return func(c *Context) error {
		if err := c.API.SetProperty(c, c.App, p.Name, "enabled", value); err != nil {
			return err
		}
		if value == "true" {
			c.Step("Enabled %s for %s", p.Name, c.App)
		} else {
			c.Step("Disabled %s for %s", p.Name, c.App)
		}
		return nil
	}
}

func propertyReport(p props.Plugin) func(*Context) error {
	label := strings.ToUpper(p.Name[:1]) + strings.ReplaceAll(p.Name[1:], "-", " ")
	return func(c *Context) error {
		if c.Global() {
			res, err := c.API.Properties(c, "", p.Name)
			if err != nil {
				return err
			}
			var rows []row
			for _, k := range p.Keys {
				rows = append(rows, row{p.Name + "-global-" + k.Name, label + " global " + strings.ReplaceAll(k.Name, "-", " "), res.Global[k.Name]})
			}
			return c.report("Global "+p.Name+" information", rows)
		}
		return forEachApp(c, func(a *types.App) error {
			res, err := c.API.Properties(c, a.Name, p.Name)
			if err != nil {
				return err
			}
			var rows []row
			for _, k := range p.Keys {
				words := strings.ReplaceAll(k.Name, "-", " ")
				rows = append(rows,
					row{p.Name + "-computed-" + k.Name, label + " computed " + words, res.Computed[k.Name]},
					row{p.Name + "-global-" + k.Name, label + " global " + words, res.Global[k.Name]},
					row{p.Name + "-" + k.Name, label + " " + words, res.App[k.Name]})
			}
			return c.report(a.Name+" "+p.Name+" information", rows)
		})
	}
}
