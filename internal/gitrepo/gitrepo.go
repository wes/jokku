// Package gitrepo manages the bare repositories that "git push" targets.
//
// Each app gets <dir>/<app>.git with a pre-receive hook that hands the pushed
// revision to "jokku git-hook", which archives it and posts it to the deploy
// API. Repos are owned by the jokku user because sshd runs git-receive-pack
// as that user.
package gitrepo

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/wes/jokku/internal/sshkeys"
)

type Manager struct {
	Dir   string         // e.g. /var/lib/jokku/git
	Exe   string         // absolute path of the jokku binary
	Owner *sshkeys.Owner // owner of the repos; nil when running unprivileged
}

func (m *Manager) Path(app string) string {
	return filepath.Join(m.Dir, app+".git")
}

// Ensure creates the app's bare repo if needed and (re)writes its hook, so a
// moved jokku binary is picked up on the next push.
func (m *Manager) Ensure(app string) (string, error) {
	path := m.Path(app)
	if _, err := os.Stat(filepath.Join(path, "HEAD")); os.IsNotExist(err) {
		if err := os.MkdirAll(m.Dir, 0o755); err != nil {
			return "", err
		}
		out, err := exec.Command("git", "init", "--bare", "--quiet", "--initial-branch=main", path).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git init: %v: %s", err, out)
		}
	}
	hook := "#!/bin/sh\n# Managed by jokku.\nexec " + strconv.Quote(m.Exe) + " git-hook " + strconv.Quote(app) + "\n"
	if err := os.WriteFile(filepath.Join(path, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		return "", err
	}
	if m.Owner != nil {
		err := filepath.WalkDir(path, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Lchown(p, m.Owner.UID, m.Owner.GID)
		})
		if err != nil {
			return "", err
		}
	}
	return path, nil
}

// Rename moves a repo when its app is renamed. A missing repo is fine: the
// app was never pushed to.
func (m *Manager) Rename(app, newApp string) error {
	err := os.Rename(m.Path(app), m.Path(newApp))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = m.Ensure(newApp) // the hook embeds the app name
	return err
}

func (m *Manager) Remove(app string) error {
	return os.RemoveAll(m.Path(app))
}
