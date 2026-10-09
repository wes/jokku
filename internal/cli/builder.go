package cli

import (
	"fmt"
	"strings"

	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/compose"
	"github.com/wes/jokku/internal/props"
	"github.com/wes/jokku/internal/types"
)

// How an app is built is one choice, made with one command: a Dockerfile
// (the default), a compose file, or a registry image. builder:set holds the
// settings that go with any of them (build-dir, procfile).
var builderCommands = []*Command{
	{Name: "builder:dockerfile", Help: "Build the app from a Dockerfile, the default (path relative to the build dir)",
		App: NeedsApp, Args: "[<path>]", MaxArgs: 1, Run: setBuilder("dockerfile")},
	{Name: "builder:compose", Help: "Deploy the app from a compose file, each service a process type (path relative to the build dir)",
		App: NeedsApp, Args: "[<path>]", MaxArgs: 1, Run: setBuilder("compose")},
	{Name: "builder:image", Help: "Deploy an image from a registry, like postgres:17 (creates the app if needed; pushes no longer deploy it)",
		App: NeedsApp, Args: "<image>", MinArgs: 1, MaxArgs: 1, Run: builderImage},
	{Name: "builder:report", Help: "Display how apps are built", App: OptionalApp, AnyFlags: true,
		Flags: []Flag{{Name: "global", Help: "Show only global values"}}, Run: builderReport},
}

// ensureApp returns the context's app, creating it if it doesn't exist yet.
func ensureApp(c *Context) (*types.App, error) {
	a, err := c.API.App(c, c.App)
	if client.IsNotFound(err) {
		c.Step("Creating %s", c.App)
		return c.API.CreateApp(c, c.App)
	}
	return a, err
}

func setBuilder(typ string) func(*Context) error {
	return func(c *Context) error {
		file := ""
		if len(c.Args) == 1 {
			file = c.Args[0]
		}
		a, err := ensureApp(c)
		if err != nil {
			return err
		}
		if err := c.API.SetBuilder(c, c.App, typ, file); err != nil {
			return err
		}
		c.Step("%s now builds from %s", c.App, builderFile(typ, file))
		if a.CurrentRelease > 0 {
			c.Info("From the next git push, or now with: jokku ps:rebuild %s", c.App)
		} else {
			c.Info("Deploy it with git push")
		}
		return nil
	}
}

// builderFile is the file a builder reads.
func builderFile(typ, file string) string {
	switch {
	case file != "":
		return file
	case typ == "compose":
		return "the first of " + strings.Join(compose.DefaultFiles, ", ")
	case typ == "dockerfile":
		return "Dockerfile"
	}
	return ""
}

func builderImage(c *Context) error {
	if _, err := ensureApp(c); err != nil {
		return err
	}
	return c.API.DeployImage(c, c.App, c.Args[0], func(e types.Event) { fmt.Fprintln(c.Stdout, e.Message) })
}

func builderReport(c *Context) error {
	if c.Global() {
		res, err := c.API.Properties(c, "", "builder")
		if err != nil {
			return err
		}
		p, _ := props.Lookup("builder")
		var rows []row
		for _, k := range p.SetKeys() {
			rows = append(rows, row{"builder-global-" + k, "Builder global " + strings.ReplaceAll(k, "-", " "), res.Global[k]})
		}
		return c.report("Global builder information", rows)
	}
	return forEachApp(c, func(a *types.App) error {
		res, err := c.API.Properties(c, a.Name, "builder")
		if err != nil {
			return err
		}
		b := res.Computed
		return c.report(a.Name+" builder information", []row{
			{"builder-type", "Builder type", b["type"]},
			{"builder-file", "Builder file", builderFile(b["type"], b["file"])},
			{"builder-image", "Builder image", b["image"]},
			{"builder-build-dir", "Builder build dir", b["build-dir"]},
			{"builder-procfile", "Builder procfile", b["procfile"]},
		})
	})
}
