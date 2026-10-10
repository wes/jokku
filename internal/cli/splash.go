package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/term"

	"github.com/wes/jokku/internal/splash"
)

var splashCommands = []*Command{
	{Name: "splash", Help: "Show the splash screen: the logo, and live figures about this server and the cluster",
		Local: true, serverOnly: true, Run: runSplash,
		Flags: []Flag{{Name: "color", Help: "Use color even when not writing to a terminal (the login banner does)"}}},
	{Name: "splash:enable", Help: "Show the splash screen at every login to this server, in place of the system's welcome text",
		Local: true, serverOnly: true, Run: runSplashEnable},
	{Name: "splash:disable", Help: "Stop showing the splash screen at login, and bring back the system's welcome text",
		Local: true, serverOnly: true, Run: runSplashDisable},
}

// splashWait is how long the splash waits for the jokku daemon before
// leaving out the cluster's figures.
const splashWait = 2 * time.Second

func runSplash(c *Context) error {
	if runtime.GOOS != "linux" {
		return errors.New("the splash screen shows a jokku server; log in to one and run: sudo jokku splash")
	}
	// Without root it can't read this node's identity or reach the daemon,
	// and would say jokku isn't running. The login banner runs as root.
	if os.Geteuid() != 0 {
		return errors.New("the splash screen reads this server's own files, so it needs root: sudo jokku splash")
	}
	o := splash.Options{Color: c.Bool("color")}
	if f, ok := c.Stdout.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		o.Color = o.Color || os.Getenv("NO_COLOR") == ""
		o.Width, _, _ = term.GetSize(int(f.Fd()))
	}
	splash.Render(c.Stdout, splash.Gather(c, splash.DefaultSources, splashWait), o)
	return nil
}

func runSplashEnable(c *Context) error {
	if os.Geteuid() != 0 {
		return errors.New("this changes the login banner, so it needs root: sudo jokku splash:enable")
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return err
	}
	theirs := splash.Theirs()
	if err := splash.Enable(exe); err != nil {
		return err
	}
	c.Step("The splash screen shows at every login now")
	if theirs {
		c.Info("Your own %s/00-jokku is kept in %s; splash:disable puts it back", splash.MOTDDir, splash.Dir)
	}
	c.Info("See it now with: sudo jokku splash")
	return nil
}

func runSplashDisable(c *Context) error {
	if os.Geteuid() != 0 {
		return errors.New("this changes the login banner, so it needs root: sudo jokku splash:disable")
	}
	if err := splash.Disable(); err != nil {
		return err
	}
	c.Step("The splash screen is off, and the system's welcome text is back")
	return nil
}
