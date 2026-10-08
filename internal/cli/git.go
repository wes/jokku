package cli

import (
	"fmt"

	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/types"
)

var gitCommands = []*Command{
	// Dokku takes a git author for the commit it makes; Jokku makes none, so
	// those arguments are accepted and ignored.
	{Name: "git:from-image", Help: "Deploy an image from a registry, like postgres:17 or ghcr.io/you/app:v2 (creates the app if needed)",
		App: NeedsApp, Args: "<image> [<git-username> <git-email>]", MinArgs: 1, MaxArgs: 3, Run: gitFromImage},
}

func gitFromImage(c *Context) error {
	if _, err := c.API.App(c, c.App); client.IsNotFound(err) {
		c.Step("Creating %s", c.App)
		if _, err := c.API.CreateApp(c, c.App); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return c.API.DeployImage(c, c.App, c.Args[0], func(e types.Event) { fmt.Fprintln(c.Stdout, e.Message) })
}
