// Package mesh connects nodes with WireGuard. Every node peers with every
// other node; peer N's allowed IPs are its instance subnet (10.210.N.0/24),
// so a packet for any VM goes straight to the node that hosts it, encrypted.
//
// A single server has no peers and no mesh interface at all.
package mesh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/wes/jokku/internal/types"
)

const (
	Interface = "jokku-wg"
	Port      = 51820
	chain     = "JOKKU-MESH"
)

// ProtectedPorts are jokku's own ports (control API, agent API). Over the
// mesh, only nodes may reach them, not VMs.
var ProtectedPorts = "7443,7444"

type Mesh struct {
	KeyFile string // the private key, created on first use
}

// LoadOrCreateKey returns this node's WireGuard private key.
func LoadOrCreateKey(path string) (wgtypes.Key, error) {
	if b, err := os.ReadFile(path); err == nil {
		return wgtypes.ParseKey(strings.TrimSpace(string(b)))
	}
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return wgtypes.Key{}, err
	}
	return key, os.WriteFile(path, []byte(key.String()+"\n"), 0o600)
}

// PublicKey is this node's WireGuard public key, base64.
func PublicKey(keyFile string) (string, error) {
	key, err := LoadOrCreateKey(keyFile)
	if err != nil {
		return "", err
	}
	return key.PublicKey().String(), nil
}

// Sync makes the mesh interface and firewall match peers. It replaces the
// whole peer set each time, so removed nodes disappear too.
func (m *Mesh) Sync(ctx context.Context, self types.NodeIdentity, peers []types.Peer) error {
	if len(peers) == 0 {
		return m.down(ctx)
	}
	key, err := LoadOrCreateKey(m.KeyFile)
	if err != nil {
		return err
	}
	if run(ctx, "ip", "link", "show", Interface) != nil {
		if err := run(ctx, "ip", "link", "add", Interface, "type", "wireguard"); err != nil {
			return fmt.Errorf("WireGuard is not available in this kernel: %w", err)
		}
	}
	port := Port
	cfg := wgtypes.Config{PrivateKey: &key, ListenPort: &port, ReplacePeers: true}
	keepalive := 25 * time.Second
	for _, p := range peers {
		pub, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			return fmt.Errorf("peer %s: %w", p.Name, err)
		}
		ep, err := net.ResolveUDPAddr("udp", p.Endpoint)
		if err != nil {
			return fmt.Errorf("peer %s endpoint %s: %w", p.Name, p.Endpoint, err)
		}
		_, subnet, err := net.ParseCIDR(p.Subnet)
		if err != nil {
			return fmt.Errorf("peer %s subnet: %w", p.Name, err)
		}
		cfg.Peers = append(cfg.Peers, wgtypes.PeerConfig{
			PublicKey: pub, Endpoint: ep, AllowedIPs: []net.IPNet{*subnet},
			ReplaceAllowedIPs: true, PersistentKeepaliveInterval: &keepalive,
		})
	}
	c, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.ConfigureDevice(Interface, cfg); err != nil {
		return fmt.Errorf("configuring WireGuard: %w", err)
	}
	// Traffic for other nodes' subnets leaves through the mesh with this
	// node's mesh address as its source; the local subnet is more specific
	// and stays on the bridge.
	steps := [][]string{
		{"ip", "link", "set", Interface, "up"},
		{"ip", "route", "replace", self.ClusterCIDR, "dev", Interface, "src", self.MeshIP},
	}
	for _, s := range steps {
		if err := run(ctx, s[0], s[1:]...); err != nil {
			return err
		}
	}
	return firewall(ctx, self, peers)
}

// firewall lets only nodes (their .1 mesh addresses) reach jokku's ports
// over the mesh; VMs on other nodes cannot.
func firewall(ctx context.Context, self types.NodeIdentity, peers []types.Peer) error {
	run(ctx, "iptables", "-w", "-N", chain) // fails harmlessly if it exists
	rules := [][]string{{"-F", chain}}
	for _, ip := range append([]string{self.MeshIP}, meshIPs(peers)...) {
		rules = append(rules, []string{"-A", chain, "-s", ip + "/32", "-j", "RETURN"})
	}
	rules = append(rules, []string{"-A", chain, "-p", "tcp", "-m", "multiport", "--dports", ProtectedPorts, "-j", "DROP"})
	for _, r := range rules {
		if err := run(ctx, "iptables", append([]string{"-w"}, r...)...); err != nil {
			return err
		}
	}
	jump := []string{"INPUT", "-i", Interface, "-j", chain}
	if run(ctx, "iptables", append([]string{"-w", "-C"}, jump...)...) != nil {
		return run(ctx, "iptables", append([]string{"-w", "-I", jump[0], "1"}, jump[1:]...)...)
	}
	return nil
}

func meshIPs(peers []types.Peer) []string {
	out := make([]string, len(peers))
	for i, p := range peers {
		out[i] = p.MeshIP
	}
	return out
}

func (m *Mesh) down(ctx context.Context) error {
	if run(ctx, "ip", "link", "show", Interface) == nil {
		return run(ctx, "ip", "link", "del", Interface)
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ErrNoWireGuard is returned by Check when the kernel lacks WireGuard.
var ErrNoWireGuard = errors.New("this kernel has no WireGuard support (Linux 5.6 or newer has it built in)")

// Check verifies the kernel can create WireGuard interfaces.
func Check(ctx context.Context) error {
	if err := run(ctx, "ip", "link", "add", "jokku-wgtest", "type", "wireguard"); err != nil {
		return ErrNoWireGuard
	}
	return run(ctx, "ip", "link", "del", "jokku-wgtest")
}
