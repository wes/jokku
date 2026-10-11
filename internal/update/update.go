// Package update finds jokku releases and runs their update.sh, which backs
// up, swaps the binary, runs "jokku setup" and rolls back on failure: the one
// update path, whether "jokku update" starts it or the control node asks a
// node to update itself.
package update

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wes/jokku/internal/version"
)

// Latest is the newest release's version. GitHub's /releases/latest
// redirects to /releases/tag/<version>.
func Latest(ctx context.Context) (string, error) {
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

// Script downloads release v's update.sh.
func Script(ctx context.Context, v string) ([]byte, error) {
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/update.sh", version.Repo, v)
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

// Unit is the systemd unit a node updates itself in. Its output stays in the
// journal: journalctl -u jokku-update.
const Unit = "jokku-update"

// Systemd updates this node in the background, as its own systemd unit: the
// update stops the jokku service, which would take anything it started with
// it.
type Systemd struct {
	DataDir string
}

// Start downloads release v's update.sh and starts it.
func (s *Systemd) Start(ctx context.Context, v string) error {
	script, err := Script(ctx, v)
	if err != nil {
		return fmt.Errorf("downloading the %s updater: %w", v, err)
	}
	path := filepath.Join(s.DataDir, "update.sh")
	if err := os.WriteFile(path, script, 0o700); err != nil {
		return err
	}
	// --collect unloads the unit when it ends, even if it failed, so the
	// next update can use the name again.
	out, err := exec.CommandContext(ctx, "systemd-run", "--unit="+Unit, "--collect",
		"--description=Update jokku to "+v, "--setenv=JOKKU_VERSION="+v, "/bin/sh", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("starting %s: %v: %s", Unit, err, bytes.TrimSpace(out))
	}
	return nil
}

// Running reports whether an update is under way.
func (s *Systemd) Running(ctx context.Context) bool {
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", Unit).Output()
	switch strings.TrimSpace(string(out)) {
	case "active", "activating", "deactivating", "reloading":
		return true
	}
	return false
}
