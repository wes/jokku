package cli

import (
	"fmt"
	"strconv"

	"github.com/wes/jokku/internal/types"
)

var forceFlag = Flag{Name: "force", Help: "Skip the confirmation prompt"}

var appsCommands = []*Command{
	{Name: "apps:list", Help: "List your apps", Run: appsList},
	{Name: "apps:create", Help: "Create a new app", App: NeedsApp, Run: appsCreate},
	{Name: "apps:destroy", Help: "Permanently destroy an app", App: NeedsApp, Flags: []Flag{forceFlag}, Run: appsDestroy},
	{Name: "apps:rename", Help: "Rename an app", App: NeedsApp, Args: "<new-app>", MinArgs: 1, Run: appsRename},
	{Name: "apps:clone", Help: "Clone an app's config, scale and settings into a new app", App: NeedsApp, Args: "<new-app>", MinArgs: 1, Run: appsClone},
	{Name: "apps:lock", Help: "Lock an app against deploys", App: NeedsApp, Run: appsLock(true)},
	{Name: "apps:unlock", Help: "Unlock an app for deploys", App: NeedsApp, Run: appsLock(false)},
	{Name: "apps:exists", Help: "Exit 0 if the app exists, 1 otherwise", App: NeedsApp, Run: appsExists},
	{Name: "apps:report", Help: "Display app information", App: OptionalApp, AnyFlags: true, Run: appsReport},
}

func appsList(c *Context) error {
	apps, err := c.API.Apps(c)
	if err != nil {
		return err
	}
	c.Header("My Apps")
	for _, a := range apps {
		fmt.Fprintln(c.Stdout, a.Name)
	}
	return nil
}

func appsCreate(c *Context) error {
	c.Step("Creating %s...", c.App)
	_, err := c.API.CreateApp(c, c.App)
	return err
}

func appsDestroy(c *Context) error {
	if _, err := c.API.App(c, c.App); err != nil {
		return err
	}
	if !c.Bool("force") {
		if err := c.confirm("destroy app "+c.App, c.App); err != nil {
			return err
		}
	}
	c.Step("Destroying %s", c.App)
	return c.API.DestroyApp(c, c.App)
}

func appsRename(c *Context) error {
	newName := c.Args[0]
	if err := c.API.RenameApp(c, c.App, newName); err != nil {
		return err
	}
	c.Step("Renamed %s to %s", c.App, newName)
	return nil
}

func appsClone(c *Context) error {
	newName := c.Args[0]
	c.Step("Cloning %s to %s", c.App, newName)
	return c.API.CloneApp(c, c.App, newName)
}

func appsLock(locked bool) func(*Context) error {
	return func(c *Context) error {
		if err := c.API.SetAppLocked(c, c.App, locked); err != nil {
			return err
		}
		if locked {
			c.Step("Deploy lock created")
		} else {
			c.Step("Deploy lock removed")
		}
		return nil
	}
}

func appsExists(c *Context) error {
	if _, err := c.API.App(c, c.App); err != nil {
		return err
	}
	c.Step("App %s exists", c.App)
	return nil
}

func appsReport(c *Context) error {
	return forEachApp(c, func(a *types.App) error {
		release, source := "none", "none"
		if a.CurrentRelease > 0 {
			release = "v" + strconv.Itoa(a.CurrentRelease)
		}
		if a.DeploySource != "" {
			source = a.DeploySource
		}
		return c.report(a.Name+" app information", []row{
			{"app-created-at", "App created at", a.CreatedAt.Local().Format("2006-01-02 15:04:05 MST")},
			{"app-current-release", "App current release", release},
			{"app-deploy-source", "App deploy source", source},
			{"app-locked", "App locked", yesNo(a.Locked)},
		})
	})
}

// forEachApp runs fn for the context's app, or for every app when none was
// given (reports without an app cover everything, as in Dokku).
func forEachApp(c *Context, fn func(*types.App) error) error {
	if c.App != "" {
		a, err := c.API.App(c, c.App)
		if err != nil {
			return err
		}
		return fn(a)
	}
	apps, err := c.API.Apps(c)
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		c.Warn("You haven't deployed any applications yet")
	}
	for i := range apps {
		if err := fn(&apps[i]); err != nil {
			return err
		}
	}
	return nil
}
