package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/wes/jokku/internal/version"
)

var updateCommand = &Command{
	Name: "update", Help: "Update jokku on this server to the latest release (asks first)",
	Local: true, serverOnly: true, Run: runUpdate,
	Flags: []Flag{
		{Name: "yes", Short: "y", Help: "Don't ask for confirmation"},
		{Name: "version", Value: "VERSION", Help: "Update to a specific release, e.g. v0.0.3"},
	},
}

// runUpdate checks for a newer release, asks, then runs that release's
// update.sh, which backs up, swaps the binary, runs "jokku setup" and rolls
// back on failure. One update path, whichever way it is started.
func runUpdate(c *Context) error {
	if os.Geteuid() != 0 {
		return errors.New("updating needs root: sudo jokku update")
	}
	target := c.String("version")
	if target == "" {
		var err error
		if target, err = latestRelease(c); err != nil {
			return fmt.Errorf("checking for a new release: %w", err)
		}
	}
	current := version.Version
	if target == current {
		c.Step("Jokku is up to date (%s)", current)
		return nil
	}
	c.Step("Jokku %s is available (this server runs %s)", target, current)
	c.Info("Release notes: https://github.com/%s/releases/tag/%s", version.Repo, target)
	if !c.Bool("yes") {
		ok, err := c.ask(fmt.Sprintf("Update jokku from %s to %s? [y/N] ", current, target))
		if err != nil {
			return err
		}
		if !ok {
			c.Step("Update cancelled")
			return nil
		}
	}

	script, err := fetch(c, fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/update.sh", version.Repo, target))
	if err != nil {
		return fmt.Errorf("downloading the %s updater: %w", target, err)
	}
	cmd := exec.CommandContext(c, "sh", "-s")
	cmd.Stdin = strings.NewReader(string(script))
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	cmd.Env = append(os.Environ(), "JOKKU_VERSION="+target)
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return &exitError{code: exit.ExitCode()} // update.sh already explained
		}
		return err
	}
	return nil
}

// ask prints a yes/no question and reads the answer from the terminal.
func (c *Context) ask(question string) (bool, error) {
	f, ok := c.Stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return false, fmt.Errorf("%s needs confirmation; pass --yes to run it non-interactively", c.Cmd.Name)
	}
	fmt.Fprint(c.Stdout, question)
	answer, _ := bufio.NewReader(c.Stdin).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

// latestRelease follows /releases/latest, which redirects to
// /releases/tag/<version>.
func latestRelease(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://github.com/"+version.Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	_, tag, ok := strings.Cut(resp.Request.URL.Path, "/releases/tag/")
	if !ok || !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("unexpected response from GitHub (%s)", resp.Request.URL)
	}
	return tag, nil
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
