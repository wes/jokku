package guest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// IsInit reports whether this process was started by the guest kernel as
// init.
func IsInit() bool {
	return os.Getpid() == 1 && os.Args[0] == InitPath
}

// Init runs the guest: it prepares the filesystem and network, runs the app
// until it exits or the VM is asked to stop, then powers the VM off. It never
// returns.
func Init() {
	if err := boot(); err != nil {
		logf("%v", err)
	}
	unix.Sync()
	// With reboot=k (x86) and PSCI (arm64), a guest reboot ends the
	// Firecracker process, which is how the host learns the app stopped.
	unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	for {
		time.Sleep(time.Hour)
	}
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "jokku: "+format+"\n", args...)
}

func boot() error {
	cfg, err := readConfig(ConfigDevice)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	if err := writableRoot(); err != nil {
		return fmt.Errorf("preparing the filesystem: %w", err)
	}
	if err := mountSystem(); err != nil {
		return fmt.Errorf("mounting system filesystems: %w", err)
	}
	if err := setupNetwork(cfg); err != nil {
		return fmt.Errorf("configuring the network: %w", err)
	}
	if err := mountVolumes(cfg); err != nil {
		return fmt.Errorf("mounting volumes: %w", err)
	}
	defer unmountVolumes(cfg)
	// Ctrl-Alt-Del (Firecracker's graceful stop on x86) now sends SIGINT to
	// init instead of rebooting.
	unix.Reboot(unix.LINUX_REBOOT_CMD_CAD_OFF)
	return supervise(cfg)
}

// mountVolumes mounts each volume at its path. A fresh volume is seeded the
// way Docker seeds a new named volume: with what the image has at that path,
// owned as in the image, or, where the image has nothing, owned by the app's
// user so the app can write to it. mkfs's lost+found goes first, since some
// programs (initdb) refuse a data directory that is not empty.
func mountVolumes(cfg *Config) error {
	if len(cfg.Mounts) == 0 {
		return nil
	}
	cred, _, err := lookupUser(cfg.User)
	if err != nil {
		return err
	}
	for i, m := range cfg.Mounts {
		stage := fmt.Sprintf("%s/volume%d", MountDir, i)
		if err := os.MkdirAll(stage, 0o700); err != nil {
			return err
		}
		if err := unix.Mount(m.Device, stage, "ext4", unix.MS_NOATIME, "discard"); err != nil {
			return fmt.Errorf("%s at %s: %w", m.Device, m.Path, err)
		}
		image, err := os.Stat(m.Path)
		switch {
		case err == nil && !image.IsDir():
			return fmt.Errorf("%s is a file in the image, not a directory", m.Path)
		case err != nil:
			image = nil
		}
		fresh, err := isFresh(stage)
		if err != nil {
			return err
		}
		if fresh {
			os.Remove(stage + "/lost+found")
			switch {
			case image != nil:
				if err := copyTree(m.Path, stage); err != nil {
					return fmt.Errorf("copying the image's %s into the new volume: %w", m.Path, err)
				}
			case cred != nil:
				if err := os.Chown(stage, int(cred.Uid), int(cred.Gid)); err != nil {
					return err
				}
			}
		}
		if image == nil {
			if err := os.MkdirAll(m.Path, 0o755); err != nil {
				return err
			}
		}
		if err := unix.Mount(stage, m.Path, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("moving the volume to %s: %w", m.Path, err)
		}
	}
	return nil
}

// unmountVolumes leaves each volume's filesystem clean before power off, or
// at least read-only when something still holds files open.
func unmountVolumes(cfg *Config) {
	for i := len(cfg.Mounts) - 1; i >= 0; i-- {
		path := cfg.Mounts[i].Path
		if unix.Unmount(path, 0) == nil {
			continue
		}
		if err := unix.Mount("", path, "", unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			logf("unmounting the volume at %s: %v", path, err)
		}
	}
}

func readConfig(dev string) (*Config, error) {
	f, err := os.Open(dev)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return nil, err
	}
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	if len(cfg.Argv) == 0 {
		return nil, errors.New("no command to run")
	}
	return &cfg, nil
}

// writableRoot overlays the per-instance scratch disk on the read-only image
// and makes the result the root. Everything is staged in a tmpfs so no
// overlay layer contains its own mount point.
func writableRoot() error {
	stage := MountDir
	if err := unix.Mount("tmpfs", stage, "tmpfs", 0, "mode=0755"); err != nil {
		return err
	}
	lower, scratch, merged := stage+"/lower", stage+"/scratch", stage+"/merged"
	for _, d := range []string{lower, scratch, merged} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return err
		}
	}
	if err := unix.Mount("/", lower, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if err := unix.Mount(ScratchDevice, scratch, "ext4", 0, ""); err != nil {
		return err
	}
	for _, d := range []string{scratch + "/upper", scratch + "/work"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	opts := "lowerdir=" + lower + ",upperdir=" + scratch + "/upper,workdir=" + scratch + "/work"
	if err := unix.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		return err
	}

	old := merged + "/.jokku/old"
	if err := os.MkdirAll(old, 0o700); err != nil {
		return err
	}
	if err := unix.PivotRoot(merged, old); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	// The overlay keeps its layers alive; the old tree is no longer needed.
	unix.Unmount("/.jokku/old", unix.MNT_DETACH)
	os.Remove("/.jokku/old")
	return nil
}

func mountSystem() error {
	for _, m := range []struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}{
		{"proc", "/proc", "proc", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"sysfs", "/sys", "sysfs", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755"},
		{"devpts", "/dev/pts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"},
		{"tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
	} {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil && m.fstype != "cgroup2" {
			return fmt.Errorf("mount %s: %w", m.target, err)
		}
	}
	os.Symlink("/proc/self/fd", "/dev/fd")
	os.Symlink("pts/ptmx", "/dev/ptmx")
	// As in a container. Without them, a program that logs to /dev/stdout
	// (nginx images do) creates a plain file in /dev instead.
	for fd, name := range []string{"stdin", "stdout", "stderr"} {
		os.Symlink("/proc/self/fd/"+strconv.Itoa(fd), "/dev/"+name)
	}
	return nil
}

// setupNetwork finishes what the kernel's ip= boot argument started (eth0
// address and default route): loopback, hostname, /etc/hosts and DNS.
func setupNetwork(cfg *Config) error {
	if err := linkUp("lo"); err != nil {
		return err
	}
	if cfg.Hostname != "" {
		if err := unix.Sethostname([]byte(cfg.Hostname)); err != nil {
			return err
		}
		replaceFile("/etc/hostname", cfg.Hostname+"\n")
	}
	hosts := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"
	if cfg.IP != "" && cfg.Hostname != "" {
		hosts += cfg.IP + "\t" + cfg.Hostname + "\n"
	}
	replaceFile("/etc/hosts", hosts)
	var resolv strings.Builder
	if len(cfg.Search) > 0 {
		resolv.WriteString("search " + strings.Join(cfg.Search, " ") + "\n")
	}
	for _, ns := range cfg.DNS {
		resolv.WriteString("nameserver " + ns + "\n")
	}
	replaceFile("/etc/resolv.conf", resolv.String())
	return nil
}

func linkUp(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// replaceFile writes a file, replacing symlinks (images often link
// /etc/resolv.conf elsewhere).
func replaceFile(path, content string) {
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.Remove(path)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		logf("writing %s: %v", path, err)
	}
}

// supervise runs the app as PID 1's child: it reaps every orphan, forwards a
// stop request to the app's process group as its stop signal (SIGTERM unless
// the image says otherwise) and escalates to SIGKILL after the stop timeout.
func supervise(cfg *Config) error {
	env := withDefaults(cfg.Env)
	path, err := lookPath(cfg.Argv[0], getenv(env, "PATH"))
	if err != nil {
		return err
	}
	cred, home, err := lookupUser(cfg.User)
	if err != nil {
		return err
	}
	if getenv(env, "HOME") == "" {
		env = append(env, "HOME="+home)
	}
	dir := cfg.WorkDir
	if dir == "" {
		dir = "/"
	}

	// The app's output is the console, which the kernel opened for init with
	// mode 0600. A program that reopens it by name, such as nginx with
	// error_log /dev/stderr, may run as another user (the image's USER, or a
	// worker that drops privileges), so any user in the VM may write to it.
	unix.Fchmod(int(os.Stdout.Fd()), 0o666)
	unix.Fchmod(int(os.Stderr.Fd()), 0o666)

	signals := make(chan os.Signal, 16)
	signal.Notify(signals, unix.SIGINT, unix.SIGTERM, unix.SIGCHLD)

	cmd := &exec.Cmd{
		Path: path, Args: cfg.Argv, Env: env, Dir: dir,
		Stdout: os.Stdout, Stderr: os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true, Credential: cred},
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", strings.Join(cfg.Argv, " "), err)
	}
	pid := cmd.Process.Pid
	stopTimeout := time.Duration(cfg.StopTimeout) * time.Second
	if stopTimeout <= 0 {
		stopTimeout = 10 * time.Second
	}
	stopSignal, name := stopSignal(cfg.StopSignal)
	var kill <-chan time.Time

	for {
		select {
		case sig := <-signals:
			if sig != unix.SIGCHLD {
				if kill == nil {
					logf("stopping (%s, then SIGKILL after %s)", name, stopTimeout)
					unix.Kill(-pid, stopSignal)
					kill = time.After(stopTimeout)
				}
				continue
			}
			for {
				var ws unix.WaitStatus
				reaped, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
				if reaped <= 0 || err != nil {
					break
				}
				if reaped == pid {
					unix.Kill(-pid, unix.SIGKILL) // anything the app left behind
					if ws.Signaled() {
						logf("app exited on signal %s", ws.Signal())
					} else {
						logf("app exited with status %d", ws.ExitStatus())
					}
					return nil
				}
			}
		case <-kill:
			logf("app did not stop in time, killing it")
			unix.Kill(-pid, unix.SIGKILL)
		}
	}
}

// stopSignal reads a signal as Docker's STOPSIGNAL takes it (SIGINT, INT,
// 2), falling back to SIGTERM.
func stopSignal(s string) (unix.Signal, string) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if n, err := strconv.Atoi(s); err == nil && n > 0 && n < 65 {
		return unix.Signal(n), unix.SignalName(unix.Signal(n))
	}
	if s != "" && !strings.HasPrefix(s, "SIG") {
		s = "SIG" + s
	}
	if sig := unix.SignalNum(s); sig != 0 {
		return sig, s
	}
	if s != "" {
		logf("unknown stop signal %q, using SIGTERM", s)
	}
	return unix.SIGTERM, "SIGTERM"
}

func withDefaults(env []string) []string {
	if getenv(env, "PATH") == "" {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return env
}

func getenv(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			return v
		}
	}
	return ""
}

func lookPath(file, pathEnv string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		p := filepath.Join(dir, file)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: command not found in PATH (%s)", file, pathEnv)
}

// lookupUser resolves an image USER ("name", "uid", "name:group" or
// "uid:gid") against the image's /etc/passwd and /etc/group.
func lookupUser(spec string) (*syscall.Credential, string, error) {
	if spec == "" || spec == "root" || spec == "0" || spec == "0:0" {
		return nil, "/root", nil
	}
	name, group, _ := strings.Cut(spec, ":")
	uid, gid, home := -1, -1, "/"
	if n, err := strconv.Atoi(name); err == nil {
		uid = n
	}
	for _, f := range readColonFile("/etc/passwd") {
		if len(f) < 6 {
			continue
		}
		if f[0] == name || f[2] == name {
			uid, _ = strconv.Atoi(f[2])
			gid, _ = strconv.Atoi(f[3])
			home = f[5]
			break
		}
	}
	if uid < 0 {
		return nil, "", fmt.Errorf("user %q not found in the image", name)
	}
	if group != "" {
		gid = -1
		if n, err := strconv.Atoi(group); err == nil {
			gid = n
		}
		for _, f := range readColonFile("/etc/group") {
			if len(f) >= 3 && f[0] == group {
				gid, _ = strconv.Atoi(f[2])
			}
		}
		if gid < 0 {
			return nil, "", fmt.Errorf("group %q not found in the image", group)
		}
	}
	if gid < 0 {
		gid = uid
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, home, nil
}

func readColonFile(path string) [][]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, strings.Split(line, ":"))
		}
	}
	return out
}
