// Package setup prepares a Linux server to run Jokku and (re)starts it.
//
// install.sh and update.sh only fetch the jokku binary and run "jokku setup",
// so every release carries the host changes it needs (packages, the jokku
// user, systemd units, later Firecracker and its kernel). Each step checks
// before it changes anything, so setup is safe to run any number of times.
package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/daemon"
	"github.com/wes/jokku/internal/version"
)

const (
	home     = "/home/" + daemon.DefaultUser
	unitPath = "/etc/systemd/system/jokku.service"
	service  = "jokku"
)

// Run brings the server to the state this version of jokku needs and leaves
// the jokku service running this binary.
func Run(ctx context.Context, out io.Writer) error {
	if runtime.GOOS != "linux" {
		return errors.New("jokku setup prepares Linux servers")
	}
	if os.Geteuid() != 0 {
		return errors.New("jokku setup must run as root: sudo jokku setup")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return errors.New("jokku needs systemd, which is not running on this machine")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	s := &setup{out: out, exe: exe}
	for _, step := range []func(context.Context) error{s.packages, s.user, s.dirs, s.unit, s.start} {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return nil
}

type setup struct {
	out io.Writer
	exe string
	uid int
	gid int
}

func (s *setup) step(format string, args ...any) {
	fmt.Fprintf(s.out, "-----> "+format+"\n", args...)
}

// packages installs what pushes need: git, and sshd to receive them.
func (s *setup) packages(ctx context.Context) error {
	var missing []string
	if _, err := exec.LookPath("git"); err != nil {
		missing = append(missing, "git")
	}
	if _, err := os.Stat("/usr/sbin/sshd"); err != nil {
		missing = append(missing, "openssh-server")
	}
	if len(missing) == 0 {
		return nil
	}
	s.step("Installing %s", strings.Join(missing, " "))
	switch {
	case have("apt-get"):
		if err := run(ctx, "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "update", "-qq"); err != nil {
			return err
		}
		if err := run(ctx, "env", append([]string{"DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "-qq"}, missing...)...); err != nil {
			return err
		}
	case have("dnf"):
		if err := run(ctx, "dnf", append([]string{"install", "-y", "-q"}, missing...)...); err != nil {
			return err
		}
	default:
		return fmt.Errorf("install %s with your package manager, then run: sudo jokku setup", strings.Join(missing, " "))
	}
	// A freshly installed sshd is not always started (RHEL-likes).
	if run(ctx, "systemctl", "enable", "--now", "ssh") != nil {
		run(ctx, "systemctl", "enable", "--now", "sshd")
	}
	return nil
}

// user creates the account sshd runs git pushes and remote commands as. Every
// key in its authorized_keys is pinned to "jokku ssh-command".
func (s *setup) user(ctx context.Context) error {
	if _, err := user.Lookup(daemon.DefaultUser); err != nil {
		s.step("Creating the %s user", daemon.DefaultUser)
		// No --create-home: the account only runs git and jokku, so skip /etc/skel.
		err := run(ctx, "useradd", "--system", "--user-group", "--no-create-home",
			"--home-dir", home, "--shell", "/bin/sh", daemon.DefaultUser)
		if err != nil {
			return err
		}
	}
	// No password login, but not "locked" either: sshd refuses key logins for
	// locked accounts on systems without PAM.
	if err := run(ctx, "usermod", "-p", "*", daemon.DefaultUser); err != nil {
		return err
	}
	u, err := user.Lookup(daemon.DefaultUser)
	if err != nil {
		return err
	}
	s.uid, _ = strconv.Atoi(u.Uid)
	s.gid, _ = strconv.Atoi(u.Gid)
	return nil
}

func (s *setup) dirs(context.Context) error {
	for _, d := range []struct {
		path  string
		mode  os.FileMode
		owned bool // by the jokku user rather than root
	}{
		{home, 0o750, true},
		{home + "/.ssh", 0o700, true},
		{daemon.DefaultDataDir, 0o755, false},
		{daemon.DefaultDataDir + "/git", 0o755, true},
	} {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
		uid, gid := 0, 0
		if d.owned {
			uid, gid = s.uid, s.gid
		}
		if err := os.Chown(d.path, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

func (s *setup) unit(ctx context.Context) error {
	want := []byte(unitFile(s.exe))
	if have, err := os.ReadFile(unitPath); err == nil && bytes.Equal(have, want) {
		return nil
	}
	s.step("Installing the %s service", service)
	if err := os.WriteFile(unitPath, want, 0o644); err != nil {
		return err
	}
	return run(ctx, "systemctl", "daemon-reload")
}

func unitFile(exe string) string {
	return `[Unit]
Description=Jokku
Documentation=https://github.com/wes/jokku
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=` + exe + ` daemon
Restart=always
RestartSec=2
RuntimeDirectory=jokku

[Install]
WantedBy=multi-user.target
`
}

// start (re)starts the service and waits until the API answers with this
// binary's version, which proves the new code is what is running.
func (s *setup) start(ctx context.Context) error {
	if run(ctx, "systemctl", "is-active", "--quiet", service) == nil {
		s.step("Restarting %s", service)
	} else {
		s.step("Starting %s", service)
	}
	if err := run(ctx, "systemctl", "enable", "--quiet", service); err != nil {
		return err
	}
	if err := run(ctx, "systemctl", "restart", service); err != nil {
		return err
	}

	api := client.New(client.Target{Socket: client.DefaultSocket}, "setup")
	deadline := time.Now().Add(30 * time.Second)
	for {
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		v, err := api.Version(reqCtx)
		cancel()
		if err == nil && v.Version == version.Version {
			fmt.Fprintf(s.out, "       jokku %s is running\n", v.Version)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("jokku %s did not start within 30s; see: journalctl -u %s -n 50", version.Version, service)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func have(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}
