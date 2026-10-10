package splash

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/wes/jokku/internal/daemon"
)

// The splash becomes the login banner as a script in /etc/update-motd.d,
// which pam_motd runs at every SSH login on Debian and Ubuntu. It takes the
// place of the distribution's own welcome text, which it turns off and
// remembers turning off, so that Disable can turn it back on. The scripts
// that warn about something (a restart needed, a new release) keep running.
var (
	MOTDDir = "/etc/update-motd.d"
	// Dir holds what Disable needs: the state, and a banner script of the
	// admin's own that the splash took the place of.
	Dir = filepath.Join(daemon.DefaultDataDir, "splash")
)

const (
	scriptName = "00-jokku"
	marker     = "# Installed by jokku"
)

// stock are the welcome scripts the splash replaces: Ubuntu's header, help
// links, news, system information, update count and Ubuntu Pro notice, and
// Debian's kernel line.
var stock = []string{
	"00-header", "10-help-text", "10-uname", "50-landscape-sysinfo", "50-motd-news",
	"90-updates-available", "91-contract-ua-esm-status",
}

// State is whether the splash is the login banner here.
type State struct {
	On bool `json:"on"`
	// Declined means the admin said no (or turned it off), so setup
	// doesn't ask again.
	Declined bool       `json:"declined,omitempty"`
	Silenced []Silenced `json:"silenced,omitempty"`
}

// Silenced is a stock welcome script the splash turned off.
type Silenced struct {
	Name   string `json:"name"`
	How    string `json:"how"`              // statoverride, chmod or unlink
	Target string `json:"target,omitempty"` // the symlink it removed points here
}

// Supported reports whether this machine runs login banners from
// /etc/update-motd.d.
func Supported() bool {
	fi, err := os.Stat(MOTDDir)
	return err == nil && fi.IsDir()
}

func LoadState() (*State, error) {
	b, err := os.ReadFile(filepath.Join(Dir, "state.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	return &st, json.Unmarshal(b, &st)
}

func saveState(st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(Dir, "state.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(Dir, "state.json"))
}

// Theirs reports whether the splash's place in /etc/update-motd.d is taken
// by a script jokku didn't write. Enable keeps it in Dir, and Disable puts
// it back.
func Theirs() bool {
	b, err := os.ReadFile(filepath.Join(MOTDDir, scriptName))
	return err == nil && !bytes.Contains(b, []byte(marker))
}

// Enable makes the splash, drawn by the jokku binary at exe, the login
// banner. Running it again only brings it up to date.
func Enable(exe string) error {
	if !Supported() {
		return fmt.Errorf("this server has no %s, which the login splash needs (Debian and Ubuntu have it)", MOTDDir)
	}
	st, err := LoadState()
	if err != nil {
		return err
	}
	path := filepath.Join(MOTDDir, scriptName)
	if Theirs() {
		backup := filepath.Join(Dir, scriptName)
		if _, err := os.Stat(backup); err == nil {
			return fmt.Errorf("%s is not jokku's, and %s already holds an earlier one: move one of them away first", path, backup)
		}
		if err := os.MkdirAll(Dir, 0o755); err != nil {
			return err
		}
		if err := os.Rename(path, backup); err != nil {
			return err
		}
	}
	script := fmt.Sprintf("#!/bin/sh\n%s: the login splash screen. Turn it off with: sudo jokku splash:disable\n"+
		"[ -x %[2]s ] || exit 0\nexec timeout 5 %[2]s splash --color\n", marker, exe)
	if b, err := os.ReadFile(path); err != nil || string(b) != script {
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			return err
		}
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return err
	}
	for _, name := range stock {
		if slices.ContainsFunc(st.Silenced, func(s Silenced) bool { return s.Name == name }) {
			continue
		}
		s, err := silence(filepath.Join(MOTDDir, name))
		if err != nil {
			return err
		}
		if s != nil {
			s.Name = name
			st.Silenced = append(st.Silenced, *s)
		}
	}
	st.On, st.Declined = true, false
	return saveState(st)
}

// silence turns a welcome script off, the way that lasts: a dpkg stat
// override survives the package's upgrades, where a chmod would not. It
// leaves alone a script that is missing, already off, or that the admin
// set an override for.
func silence(path string) (*Silenced, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, nil
	}
	if fi.Mode()&fs.ModeSymlink != 0 { // landscape-common links its sysinfo in
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		return &Silenced{How: "unlink", Target: target}, os.Remove(path)
	}
	if fi.Mode()&0o111 == 0 {
		return nil, nil
	}
	if haveCommand("dpkg-statoverride") {
		if command("dpkg-statoverride", "--list", path) == nil {
			return nil, nil // the admin's
		}
		if err := command("dpkg-statoverride", "--update", "--add", "root", "root", "0644", path); err != nil {
			return nil, fmt.Errorf("turning off %s: %w", path, err)
		}
		return &Silenced{How: "statoverride"}, nil
	}
	return &Silenced{How: "chmod"}, os.Chmod(path, fi.Mode().Perm()&^0o111)
}

// Disable takes the splash off the login banner, puts back what it
// replaced, and notes not to offer it again.
func Disable() error {
	st, err := LoadState()
	if err != nil {
		return err
	}
	path := filepath.Join(MOTDDir, scriptName)
	if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte(marker)) {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if backup := filepath.Join(Dir, scriptName); !exists(path) && exists(backup) {
		if err := os.Rename(backup, path); err != nil {
			return err
		}
	}
	var errs []error
	for _, s := range st.Silenced {
		p := filepath.Join(MOTDDir, s.Name)
		switch s.How {
		case "unlink":
			if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, os.Symlink(s.Target, p))
			}
		case "statoverride":
			command("dpkg-statoverride", "--remove", p) // gone already is fine
			fallthrough
		case "chmod":
			if fi, err := os.Stat(p); err == nil {
				errs = append(errs, os.Chmod(p, fi.Mode().Perm()|0o111))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return saveState(&State{Declined: true})
}

// Decline notes that the admin doesn't want the splash, so setup doesn't
// ask again.
func Decline() error {
	st, err := LoadState()
	if err != nil {
		return err
	}
	st.Declined = true
	return saveState(st)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Overridden in tests.
var (
	command = func(name string, args ...string) error {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, bytes.TrimSpace(out))
		}
		return nil
	}
	haveCommand = func(name string) bool {
		_, err := exec.LookPath(name)
		return err == nil
	}
)
