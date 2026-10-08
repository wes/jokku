// Package setup prepares a Linux server to run Jokku and (re)starts it.
//
// install.sh and update.sh only fetch the jokku binary and run "jokku setup",
// so every release carries the host changes it needs: packages, the jokku
// user, pinned Firecracker / kernel / BuildKit, and the systemd services.
// Each step checks before it changes anything, so setup is safe to run any
// number of times.
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/wes/jokku/internal/build"
	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/daemon"
	"github.com/wes/jokku/internal/deps"
	"github.com/wes/jokku/internal/mesh"
	"github.com/wes/jokku/internal/proxy"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/version"
	"github.com/wes/jokku/internal/vm"
)

const home = "/home/" + daemon.DefaultUser

// Options select what setup does beyond bringing the server up to date.
type Options struct {
	// Join makes this server a worker in the cluster whose control node is at
	// this address (host or host:port), using Token from
	// "jokku cluster:join-command".
	Join  string
	Token string
	// Name is this node's name in the cluster (default: the hostname).
	Name string
	// Advertise is the address other nodes reach this one at (default: the
	// address used to reach the internet).
	Advertise string
}

// Run brings the server to the state this version of jokku needs and leaves
// its services running this binary. A control node (the default) gets
// everything; a worker gets only what running microVMs needs.
func Run(ctx context.Context, out io.Writer, o Options) error {
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
	_, joined := joinedCluster()
	worker := joined || o.Join != ""
	if o.Join != "" && !joined && exists(filepath.Join(daemon.DefaultDataDir, "jokku.db")) {
		return errors.New("this server already runs Jokku as its own cluster, so it cannot join another; " +
			"use a fresh server, or remove /var/lib/jokku to start over")
	}
	s := &setup{out: out, exe: exe, opts: o, worker: worker}
	steps := []func(context.Context) error{s.packages, s.user, s.dirs, s.deps, s.sysctl, s.units, s.start, s.defaultDomain}
	if worker {
		steps = []func(context.Context) error{s.packages, s.wireguard, s.dirs, s.deps, s.join, s.sysctl, s.units, s.start}
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	if err := (&vm.Host{}).Available(); err != nil {
		fmt.Fprintln(out, " !     Apps cannot run on this server yet: "+err.Error())
	}
	return nil
}

// joinedCluster reports whether this server is a worker in a cluster.
func joinedCluster() (*daemon.NodeFile, bool) {
	nf, err := daemon.LoadNodeFile(daemon.DefaultDataDir)
	return nf, err == nil && nf.Role == store.RoleWorker
}

type setup struct {
	out          io.Writer
	exe          string
	opts         Options
	worker       bool
	uid, gid     int
	changedUnits map[string]bool
}

func (s *setup) step(format string, args ...any) {
	fmt.Fprintf(s.out, "-----> "+format+"\n", args...)
}

// packages installs what Jokku runs: git and sshd for pushes, iptables for VM
// networking, e2fsprogs to build root filesystems.
func (s *setup) packages(ctx context.Context) error {
	var missing []string
	need := map[string]bool{
		"iptables":  have("iptables"),
		"e2fsprogs": have("mkfs.ext4"),
	}
	if !s.worker { // pushes land on the control node
		need["git"] = have("git")
		need["openssh-server"] = exists("/usr/sbin/sshd")
	}
	for pkg, present := range need {
		if !present {
			missing = append(missing, pkg)
		}
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
	if s.worker {
		return os.MkdirAll(daemon.DefaultDataDir, 0o755)
	}
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

// deps installs the pinned Firecracker, guest kernel and BuildKit.
func (s *setup) deps(context.Context) error {
	for _, d := range deps.All {
		if d.Installed() || (s.worker && d.Name == deps.BuildKit.Name) {
			continue // workers don't build
		}
		s.step("Installing %s %s", d.Name, d.Version)
		if err := d.Install(); err != nil {
			return err
		}
	}
	return deps.Prune()
}

// sysctl lets VMs reach the internet through this host, across reboots.
func (s *setup) sysctl(ctx context.Context) error {
	return writeIfChanged("/etc/sysctl.d/90-jokku.conf", "net.ipv4.ip_forward = 1\n", 0o644)
}

// units writes the systemd services: the daemon, the image builder and the
// proxy.
func (s *setup) units(ctx context.Context) error {
	s.changedUnits = map[string]bool{}
	bk := deps.BuildKit
	units := map[string]string{
		"jokku": unit("Jokku", s.exe+" daemon", "jokku", ""),
		"jokku-buildkitd": unit("Jokku image builder (BuildKit)",
			bk.Path("buildkitd")+" --addr unix://"+build.BuildKitSocket+" --root "+daemon.DefaultDataDir+"/buildkit"+
				" --oci-worker-net host --containerd-worker=false",
			"jokku-buildkit", "Environment=PATH="+bk.Dir()+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n"),
		"jokku-proxy": unit("Jokku proxy (Caddy)", s.exe+" proxy", "jokku-proxy", "LimitNOFILE=1048576\n"),
	}
	if s.worker {
		delete(units, "jokku-buildkitd")
	}
	for name, content := range units {
		changed, err := writeFileIfChanged("/etc/systemd/system/"+name+".service", content, 0o644)
		if err != nil {
			return err
		}
		s.changedUnits[name] = changed
	}
	return run(ctx, "systemctl", "daemon-reload")
}

func unit(description, execStart, runtimeDir, extra string) string {
	return `[Unit]
Description=` + description + `
Documentation=https://github.com/` + version.Repo + `
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=` + execStart + `
Restart=always
RestartSec=2
RuntimeDirectory=` + runtimeDir + `
` + extra + `
[Install]
WantedBy=multi-user.target
`
}

// start restarts the daemon (it is this binary) and waits until its API
// reports this version. The builder and proxy are only restarted when their
// unit or the proxy version changed, so updates don't interrupt traffic.
func (s *setup) start(ctx context.Context) error {
	if run(ctx, "systemctl", "is-active", "--quiet", "jokku") == nil {
		s.step("Restarting jokku")
	} else {
		s.step("Starting jokku")
	}
	if err := run(ctx, "systemctl", "enable", "--quiet", "jokku"); err != nil {
		return err
	}
	if err := run(ctx, "systemctl", "restart", "jokku"); err != nil {
		return err
	}
	if err := waitForVersion(ctx, s.out); err != nil {
		return err
	}

	proxyMarker := filepath.Join(daemon.DefaultDataDir, "proxy", "version")
	marker, _ := os.ReadFile(proxyMarker)
	restart := map[string]bool{
		"jokku-buildkitd": s.changedUnits["jokku-buildkitd"],
		"jokku-proxy":     s.changedUnits["jokku-proxy"] || string(marker) != proxy.Version,
	}
	services := []string{"jokku-buildkitd", "jokku-proxy"}
	if s.worker {
		services = services[1:]
	}
	for _, svc := range services {
		active := run(ctx, "systemctl", "is-active", "--quiet", svc) == nil
		if active && !restart[svc] {
			continue
		}
		s.step("Starting %s", svc)
		if err := run(ctx, "systemctl", "enable", "--quiet", svc); err != nil {
			return err
		}
		if err := run(ctx, "systemctl", "restart", svc); err != nil {
			return err
		}
	}
	if _, err := writeFileIfChanged(proxyMarker, proxy.Version, 0o644); err != nil {
		return err
	}
	// A proxy that cannot bind 80/443 restarts in a loop; say why now rather
	// than leaving a silent failure.
	time.Sleep(2 * time.Second)
	if run(ctx, "systemctl", "is-active", "--quiet", "jokku-proxy") != nil {
		fmt.Fprintln(s.out, " !     jokku-proxy is not running. Is another web server using ports 80 or 443? See: journalctl -u jokku-proxy -n 20")
	}
	return nil
}

// wireguard checks the kernel can build the mesh before joining.
func (s *setup) wireguard(ctx context.Context) error {
	if _, joined := joinedCluster(); joined {
		return nil
	}
	return mesh.Check(ctx)
}

// join asks the control node to admit this server and saves the identity it
// gets back. A server that already joined keeps its identity.
func (s *setup) join(ctx context.Context) error {
	if nf, joined := joinedCluster(); joined {
		if s.opts.Join != "" {
			s.step("Already part of the cluster at %s as %s", nf.Control, nf.Node.Name)
		}
		return nil
	}
	o := s.opts
	addr := o.Join
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, strconv.Itoa(cluster.APIPort))
	}
	pin, _, err := cluster.ParseJoinToken(o.Token)
	if err != nil {
		return err
	}
	pub, err := mesh.PublicKey(filepath.Join(daemon.DefaultDataDir, "wireguard.key"))
	if err != nil {
		return err
	}
	name := o.Name
	if name == "" {
		host, _ := os.Hostname()
		name = nodeName(host)
	}
	advertise := o.Advertise
	if advertise == "" {
		advertise = daemon.OutboundIP()
	}
	s.step("Joining the cluster at %s as %s", addr, name)
	req := types.JoinRequest{
		Protocol: types.ProtocolVersion, Version: version.Version, Token: o.Token, Name: name, PublicKey: pub,
		Endpoint: net.JoinHostPort(advertise, strconv.Itoa(mesh.Port)), Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(), MemoryMB: daemon.TotalMemoryMB(),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: cluster.PinnedTLS(pin)}}
	resp, err := client.Post("https://"+addr+"/v1/cluster/join", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("reaching the control node at %s (is port %d open?): %w", addr, cluster.APIPort, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e types.Error
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("the control node refused: %s", e.Error)
	}
	var res types.JoinResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}
	err = daemon.SaveNodeFile(daemon.DefaultDataDir, &daemon.NodeFile{
		Role: store.RoleWorker, Control: addr, Pin: pin,
		NodeToken: res.NodeToken, AgentToken: res.AgentToken, Node: res.Node,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(s.out, "       %s joined with subnet %s\n", res.Node.Name, res.Node.Subnet)
	return nil
}

// nodeName turns a hostname into a valid node name.
func nodeName(host string) string {
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	n := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, host)
	if n = strings.Trim(n, "-"); n == "" {
		n = "node"
	}
	if len(n) > 63 {
		n = n[:63]
	}
	return n
}

func waitForVersion(ctx context.Context, out io.Writer) error {
	api := client.New(client.Target{Socket: client.DefaultSocket}, "setup")
	deadline := time.Now().Add(30 * time.Second)
	for {
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		v, err := api.Version(reqCtx)
		cancel()
		if err == nil && v.Version == version.Version {
			fmt.Fprintf(out, "       jokku %s is running\n", v.Version)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("jokku %s did not start within 30s; see: journalctl -u jokku -n 50", version.Version)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// defaultDomain sets the global domain to <server-ip>.sslip.io once, so the
// first deploy gets a working URL (myapp.<ip>.sslip.io) without any DNS.
func (s *setup) defaultDomain(ctx context.Context) error {
	marker := filepath.Join(daemon.DefaultDataDir, ".default-domain")
	if exists(marker) {
		return nil
	}
	api := client.New(client.Target{Socket: client.DefaultSocket}, "setup")
	d, err := api.Domains(ctx, "")
	if err != nil {
		return err
	}
	if ip := outboundIP(); len(d.Global) == 0 && ip != "" {
		domain := ip + ".sslip.io"
		s.step("Setting the global domain to %s (change it with: jokku domains:set-global <domain>)", domain)
		if _, err := api.PatchDomains(ctx, "", types.DomainsPatch{Set: []string{domain}}); err != nil {
			return err
		}
	}
	return os.WriteFile(marker, nil, 0o644)
}

func outboundIP() string {
	conn, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func writeIfChanged(path, content string, mode os.FileMode) error {
	_, err := writeFileIfChanged(path, content, mode)
	return err
}

func writeFileIfChanged(path, content string, mode os.FileMode) (bool, error) {
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, []byte(content)) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(content), mode)
}

func have(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}
