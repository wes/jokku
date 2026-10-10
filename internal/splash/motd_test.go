package splash

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeMOTD is an Ubuntu /etc/update-motd.d, with a dpkg-statoverride that
// keeps its overrides in a map (and, like the real one with --update,
// applies the mode). It starts with two overrides of the admin's own: one
// turning 90-updates-available off, one keeping 91-contract-ua-esm-status
// on.
type fakeMOTD struct {
	dir       string
	overrides map[string]bool
	calls     []string
}

func newFakeMOTD(t *testing.T, statoverride bool) *fakeMOTD {
	t.Helper()
	root := t.TempDir()
	f := &fakeMOTD{dir: filepath.Join(root, "update-motd.d"), overrides: map[string]bool{}}
	defer func(dir, state string) { t.Cleanup(func() { MOTDDir, Dir = dir, state }) }(MOTDDir, Dir)
	defer func(c func(string, ...string) error, h func(string) bool) {
		t.Cleanup(func() { command, haveCommand = c, h })
	}(command, haveCommand)
	MOTDDir, Dir = f.dir, filepath.Join(root, "jokku", "splash")
	haveCommand = func(name string) bool { return statoverride && name == "dpkg-statoverride" }
	command = func(name string, args ...string) error {
		f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
		path := args[len(args)-1]
		switch args[0] {
		case "--list":
			if !f.overrides[path] {
				return errors.New("no override")
			}
		case "--update":
			if f.overrides[path] {
				return errors.New("an override for " + path + " already exists, aborting")
			}
			f.overrides[path] = true
			return os.Chmod(path, 0o644)
		case "--remove":
			delete(f.overrides, path)
		}
		return nil
	}
	for name, mode := range map[string]os.FileMode{
		"00-header": 0o755, "10-help-text": 0o644, // the admin turned it off
		"50-motd-news": 0o755, "90-updates-available": 0o644, "91-contract-ua-esm-status": 0o755,
		"98-reboot-required": 0o755,
	} {
		write(t, filepath.Join(f.dir, name), "#!/bin/sh\n")
		os.Chmod(filepath.Join(f.dir, name), mode)
	}
	f.overrides[filepath.Join(f.dir, "90-updates-available")] = true
	f.overrides[filepath.Join(f.dir, "91-contract-ua-esm-status")] = true
	if err := os.Symlink("/usr/share/landscape/landscape-sysinfo.wrapper", filepath.Join(f.dir, "50-landscape-sysinfo")); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeMOTD) runs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	var on []string
	for _, e := range entries {
		fi, err := os.Lstat(filepath.Join(f.dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&0o111 != 0 {
			on = append(on, e.Name())
		}
	}
	return on
}

func TestEnableReplacesTheWelcomeTextAndDisablePutsItBack(t *testing.T) {
	f := newFakeMOTD(t, true)
	before := f.runs(t)
	if err := Enable("/usr/local/bin/jokku"); err != nil {
		t.Fatal(err)
	}
	// The splash runs, with the warnings that were on; the welcome text
	// doesn't, except where the admin's own override keeps it on.
	if got := f.runs(t); !slices.Equal(got, []string{"00-jokku", "91-contract-ua-esm-status", "98-reboot-required"}) {
		t.Errorf("running at login: %v", got)
	}
	b, _ := os.ReadFile(filepath.Join(f.dir, "00-jokku"))
	if !strings.Contains(string(b), "exec timeout 5 /usr/local/bin/jokku splash --color\n") || !strings.HasPrefix(string(b), "#!/bin/sh\n"+marker) {
		t.Errorf("banner script:\n%s", b)
	}
	// The overrides last through package upgrades; the admin's own is left alone.
	if !slices.Contains(f.calls, "dpkg-statoverride --update --add root root 0644 "+filepath.Join(f.dir, "00-header")) ||
		slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, "--add") && strings.Contains(c, "/9") }) {
		t.Errorf("dpkg-statoverride calls: %v", f.calls)
	}
	st, err := LoadState()
	if err != nil || !st.On || st.Declined || len(st.Silenced) != 3 {
		t.Errorf("state %+v, %v", st, err)
	}

	// Again, as every update does: nothing more to do, and the script
	// follows the binary.
	f.calls = nil
	if err := Enable("/opt/jokku"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "00-jokku")); !strings.Contains(string(b), "/opt/jokku splash") {
		t.Errorf("banner script didn't follow the binary:\n%s", b)
	}
	if st, _ := LoadState(); len(st.Silenced) != 3 || slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, "--add") }) {
		t.Errorf("second enable: state %+v, calls %v", st, f.calls)
	}

	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	if got := f.runs(t); !slices.Equal(got, before) {
		t.Errorf("after disable: %v, want %v", got, before)
	}
	if target, _ := os.Readlink(filepath.Join(f.dir, "50-landscape-sysinfo")); target != "/usr/share/landscape/landscape-sysinfo.wrapper" {
		t.Errorf("landscape link: %q", target)
	}
	if f.overrides[filepath.Join(f.dir, "00-header")] || !f.overrides[filepath.Join(f.dir, "90-updates-available")] ||
		!f.overrides[filepath.Join(f.dir, "91-contract-ua-esm-status")] {
		t.Errorf("overrides left: %v", f.overrides)
	}
	if st, _ := LoadState(); st.On || !st.Declined || len(st.Silenced) != 0 {
		t.Errorf("state after disable: %+v", st)
	}
}

func TestEnableWithoutDpkgChmods(t *testing.T) {
	f := newFakeMOTD(t, false)
	before := f.runs(t)
	if err := Enable("/usr/local/bin/jokku"); err != nil {
		t.Fatal(err)
	}
	if got := f.runs(t); !slices.Equal(got, []string{"00-jokku", "98-reboot-required"}) || len(f.calls) != 0 {
		t.Errorf("running at login: %v; commands %v", got, f.calls)
	}
	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	// 90-updates-available was off already, and stays off.
	if got := f.runs(t); !slices.Equal(got, before) {
		t.Errorf("after disable: %v, want %v", got, before)
	}
}

func TestTheAdminsOwnBannerIsKept(t *testing.T) {
	f := newFakeMOTD(t, true)
	theirs := filepath.Join(f.dir, "00-jokku")
	write(t, theirs, "#!/bin/bash\n# my banner\n")
	if !Theirs() {
		t.Fatal("Theirs() = false for the admin's script")
	}
	if err := Enable("/usr/local/bin/jokku"); err != nil {
		t.Fatal(err)
	}
	if Theirs() {
		t.Error("Theirs() = true for jokku's script")
	}
	if b, _ := os.ReadFile(filepath.Join(Dir, "00-jokku")); string(b) != "#!/bin/bash\n# my banner\n" {
		t.Errorf("kept copy: %q", b)
	}
	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(theirs); string(b) != "#!/bin/bash\n# my banner\n" {
		t.Errorf("after disable: %q", b)
	}
}

func TestEnableNeedsUpdateMOTD(t *testing.T) {
	newFakeMOTD(t, true)
	MOTDDir = filepath.Join(t.TempDir(), "missing")
	if err := Enable("/usr/local/bin/jokku"); err == nil || !strings.Contains(err.Error(), "Debian and Ubuntu") {
		t.Errorf("err = %v", err)
	}
}
