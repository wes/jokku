// Package vm runs microVMs on this node: the bridge they share, and one
// Firecracker process per instance.
//
// Each VM runs in its own transient systemd unit (jokku-vm-<id>), not as a
// child of the jokku daemon, so restarting or updating jokku leaves apps
// running. The unit's output is the guest's serial console, so app logs land
// in the journal tagged with JOKKU_APP and JOKKU_PROCESS.
package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/wes/jokku/internal/deps"
	"github.com/wes/jokku/internal/guest"
)

const (
	unitPrefix = "jokku-vm-"
	// scratchSize is the apparent size of each instance's writable layer. The
	// file is sparse, so only what the app writes uses disk.
	scratchSize = "10G"
)

// Host manages the microVMs on this node.
type Host struct {
	DataDir string       // /var/lib/jokku
	Bridge  string       // jokku0
	Gateway netip.Prefix // this node's address and subnet, e.g. 10.210.1.1/24
	Cluster netip.Prefix // the whole cluster network, e.g. 10.210.0.0/16
}

// Spec is everything needed to boot one instance.
type Spec struct {
	ID       string
	App      string
	Process  string // web.1
	Artifact string // rootfs image
	IP       string
	CPUs     int
	MemoryMB int
	Guest    guest.Config
}

// Available explains why this machine cannot run microVMs, or returns nil.
func (h *Host) Available() error {
	switch {
	case runtime.GOOS != "linux":
		return errors.New("microVMs need a Linux server (this is " + runtime.GOOS + ")")
	case os.Geteuid() != 0:
		return errors.New("the jokku daemon must run as root to start microVMs")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return errors.New("this server has no /dev/kvm, so it cannot run microVMs. " +
			"On a virtual machine, enable nested virtualization (Proxmox: CPU type \"host\"; libvirt: host-passthrough); " +
			"otherwise use bare metal")
	}
	if err := cpuProblem(); err != nil {
		return err
	}
	for _, d := range []deps.Dep{deps.Firecracker, deps.Kernel} {
		if !d.Installed() {
			return fmt.Errorf("%s %s is not installed; run: sudo jokku setup", d.Name, d.Version)
		}
	}
	return nil
}

// cpuProblem catches CPUs that have KVM but lack what Firecracker needs, so
// the problem shows up before a build instead of when the first VM boots.
func cpuProblem() error {
	if runtime.GOARCH != "amd64" {
		return nil
	}
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return nil
	}
	model, flags := "", map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "model name":
			if model == "" {
				model = strings.Join(strings.Fields(val), " ")
			}
		case "flags":
			if len(flags) == 0 {
				for _, f := range strings.Fields(val) {
					flags[f] = true
				}
			}
		}
	}
	if !flags["xsave"] {
		return fmt.Errorf("this CPU (%s) is too old for Firecracker, which needs XSAVE support: "+
			"Intel Sandy Bridge or AMD Bulldozer (2011) or newer, and Firecracker is tested on Intel Skylake (2015) and newer", model)
	}
	return nil
}

func (h *Host) dir(id string) string { return filepath.Join(h.DataDir, "instances", id) }
func tap(id string) string           { return "jk" + id }
func unit(id string) string          { return unitPrefix + id }

// EnsureNetwork creates the bridge VMs attach to, with NAT for outbound
// traffic. It is idempotent and runs at every daemon start.
func (h *Host) EnsureNetwork(ctx context.Context) error {
	if run(ctx, "ip", "link", "show", h.Bridge) != nil {
		if err := run(ctx, "ip", "link", "add", h.Bridge, "type", "bridge"); err != nil {
			return err
		}
	}
	steps := [][]string{
		{"ip", "addr", "replace", h.Gateway.String(), "dev", h.Bridge},
		{"ip", "link", "set", h.Bridge, "up"},
		{"sysctl", "-q", "-w", "net.ipv4.ip_forward=1"},
	}
	for _, s := range steps {
		if err := run(ctx, s[0], s[1:]...); err != nil {
			return err
		}
	}
	subnet := h.Gateway.Masked().String()
	// Rules are checked (-C) before being added, so restarts don't duplicate
	// them. Inserting the FORWARD rules first keeps them ahead of Docker's or
	// ufw's default-drop chains.
	rules := []struct {
		table, chain string
		insert       bool
		rule         []string
	}{
		{"nat", "POSTROUTING", false, []string{"-s", subnet, "!", "-d", h.Cluster.String(), "-j", "MASQUERADE"}},
		{"filter", "FORWARD", true, []string{"-i", h.Bridge, "-j", "ACCEPT"}},
		{"filter", "FORWARD", true, []string{"-o", h.Bridge, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},
	}
	for _, r := range rules {
		check := append([]string{"-w", "-t", r.table, "-C", r.chain}, r.rule...)
		if run(ctx, "iptables", check...) == nil {
			continue
		}
		op := []string{"-A", r.chain}
		if r.insert {
			op = []string{"-I", r.chain, "1"}
		}
		if err := run(ctx, "iptables", append(append([]string{"-w", "-t", r.table}, op...), r.rule...)...); err != nil {
			return err
		}
	}
	return nil
}

// Start boots an instance. A restarted instance keeps its writable layer, as
// a restarted container does.
func (h *Host) Start(ctx context.Context, s Spec) error {
	dir := h.dir(s.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	scratch := filepath.Join(dir, "scratch.ext4")
	if _, err := os.Stat(scratch); os.IsNotExist(err) {
		tmp := scratch + ".tmp"
		if err := run(ctx, "truncate", "-s", scratchSize, tmp); err != nil {
			return err
		}
		if err := run(ctx, "mkfs.ext4", "-q", "-F", "-O", "^has_journal", "-m", "0", "-E", "lazy_itable_init=1,nodiscard", tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, scratch); err != nil {
			return err
		}
	}
	cfg, err := s.Guest.Encode()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.img"), cfg, 0o600); err != nil {
		return err
	}

	t := tap(s.ID)
	if run(ctx, "ip", "link", "show", t) != nil {
		if err := run(ctx, "ip", "tuntap", "add", "dev", t, "mode", "tap"); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"link", "set", t, "master", h.Bridge}, {"link", "set", t, "up"}} {
		if err := run(ctx, "ip", args...); err != nil {
			return err
		}
	}

	vmConfig, err := h.firecrackerConfig(s, dir)
	if err != nil {
		return err
	}
	configPath := filepath.Join(dir, "vm.json")
	if err := os.WriteFile(configPath, vmConfig, 0o600); err != nil {
		return err
	}
	sock := filepath.Join(dir, "fc.sock")
	os.Remove(sock) // Firecracker refuses to start over a stale socket

	process := strings.SplitN(s.Process, ".", 2)[0]
	args := []string{
		"--unit=" + unit(s.ID), "--collect", "--quiet",
		"--description=Jokku " + s.App + " " + s.Process,
		"--property=LogExtraFields=JOKKU_APP=" + s.App,
		"--property=LogExtraFields=JOKKU_PROCESS=" + s.Process,
		"--property=LogExtraFields=JOKKU_PROCESS_TYPE=" + process,
		"--property=LogExtraFields=JOKKU_INSTANCE=" + s.ID,
		"--property=TimeoutStopSec=5",
		"--",
		deps.Firecracker.Path("firecracker"),
		"--api-sock", sock,
		"--config-file", configPath,
		// Warnings include harmless guest probing noise (PCI ports with
		// pci=off) that would otherwise show up in app logs.
		"--level", "Error",
	}
	return run(ctx, "systemd-run", args...)
}

func (h *Host) firecrackerConfig(s Spec, dir string) ([]byte, error) {
	ip, err := netip.ParseAddr(s.IP)
	if err != nil {
		return nil, err
	}
	mask := net.CIDRMask(h.Gateway.Bits(), 32)
	b := ip.As4()
	bootArgs := strings.Join([]string{
		"console=ttyS0", "reboot=k", "panic=1", "pci=off", "quiet", "loglevel=3",
		"root=" + guest.RootDevice, "ro", "rootfstype=ext4", "init=" + guest.InitPath,
		// ip=<client>::<gateway>:<netmask>:<hostname>:<device>:<autoconf>
		fmt.Sprintf("ip=%s::%s:%s:%s:eth0:off", s.IP, h.Gateway.Addr(), net.IP(mask).String(), s.Guest.Hostname),
	}, " ")
	cfg := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": deps.KernelPath(),
			"boot_args":         bootArgs,
		},
		// Order matters: vda (root), vdb (config), vdc (scratch).
		"drives": []map[string]any{
			{"drive_id": "rootfs", "path_on_host": s.Artifact, "is_root_device": true, "is_read_only": true},
			{"drive_id": "config", "path_on_host": filepath.Join(dir, "config.img"), "is_root_device": false, "is_read_only": true},
			{"drive_id": "scratch", "path_on_host": filepath.Join(dir, "scratch.ext4"), "is_root_device": false, "is_read_only": false},
		},
		"machine-config": map[string]any{"vcpu_count": s.CPUs, "mem_size_mib": s.MemoryMB},
		"network-interfaces": []map[string]any{{
			"iface_id":      "eth0",
			"guest_mac":     fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3]),
			"host_dev_name": tap(s.ID),
		}},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// Units returns the state of every VM unit on this node, keyed by instance
// ID: active, activating, deactivating, failed. Missing means not running.
func (h *Host) Units(ctx context.Context) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "list-units", "--all", "--plain", "--no-legend", "--full", unitPrefix+"*").Output()
	if err != nil {
		return nil, fmt.Errorf("listing VM units: %w", err)
	}
	units := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(f[0], unitPrefix), ".service")
		if f[2] != "inactive" {
			units[id] = f[2]
		}
	}
	return units, sc.Err()
}

// Stop asks the guest to shut down (SIGTERM to the app) and waits up to
// grace before stopping the VM outright.
func (h *Host) Stop(ctx context.Context, id string, grace time.Duration) error {
	if runtime.GOARCH == "amd64" {
		if err := ctrlAltDel(ctx, filepath.Join(h.dir(id), "fc.sock")); err == nil {
			deadline := time.Now().Add(grace)
			for time.Now().Before(deadline) {
				if run(ctx, "systemctl", "is-active", "--quiet", unit(id)) != nil {
					return nil
				}
				time.Sleep(200 * time.Millisecond)
			}
		}
	}
	if run(ctx, "systemctl", "is-active", "--quiet", unit(id)) != nil {
		return nil
	}
	return run(ctx, "systemctl", "stop", unit(id))
}

// Remove deletes a stopped instance's network device and files.
func (h *Host) Remove(ctx context.Context, id string) error {
	run(ctx, "ip", "link", "del", tap(id))
	return os.RemoveAll(h.dir(id))
}

// ctrlAltDel uses Firecracker's API to press Ctrl-Alt-Del, which the guest
// init turns into SIGTERM for the app.
func ctrlAltDel(ctx context.Context, sock string) error {
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}, Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/actions",
		strings.NewReader(`{"action_type":"SendCtrlAltDel"}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("SendCtrlAltDel: %s", resp.Status)
	}
	return nil
}

// HostDNS returns the resolvers this host uses, skipping local stubs such as
// systemd-resolved's 127.0.0.53 that a VM cannot reach.
func HostDNS() []string {
	var out []string
	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) == 2 && fields[0] == "nameserver" {
				if a, err := netip.ParseAddr(fields[1]); err == nil && a.Is4() && !a.IsLoopback() {
					out = append(out, a.String())
				}
			}
		}
		f.Close()
		if len(out) > 0 {
			return out
		}
	}
	return []string{"1.1.1.1", "8.8.8.8"}
}

// CheckTCP reports whether something accepts connections at addr.
func CheckTCP(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
