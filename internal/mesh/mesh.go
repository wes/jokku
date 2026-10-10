// Package mesh connects nodes with WireGuard. Every node peers with every
// other node; peer N's allowed IPs are its instance subnet (10.210.N.0/24),
// so a packet for any VM goes straight to the node that hosts it, encrypted.
//
// Edges are the exception: public machines that the other nodes dial, since
// those may sit behind NAT with no port open. An edge has no endpoint for
// its peers and learns each one's address from the packets it receives, so
// peers are updated in place, never replaced: replacing one would forget
// that address until the peer's next keepalive. An edge faces the internet,
// so the nodes behind it firewall what it can reach (see types.EdgeAccess).
//
// A single server has no peers and no mesh interface at all.
package mesh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
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

	chain        = "JOKKU-MESH"     // INPUT from the mesh
	edgeIn       = "JOKKU-EDGE-IN"  // INPUT from edges
	forwardChain = "JOKKU-MESH-FWD" // FORWARD to and from the mesh
	edgeForward  = "JOKKU-EDGE-FWD" // FORWARD from edges
	natChain     = "JOKKU-MESH-NAT" // POSTROUTING: edges' traffic to external apps' targets
)

// ProtectedPorts are jokku's own ports (control API, agent API). Over the
// mesh, only nodes may reach them, not VMs.
var ProtectedPorts = "7443,7444"

// keepalive keeps NAT mappings open on the way to an edge (and every peer).
const keepalive = 25 * time.Second

// roamGrace is how recent a handshake must be for a peer to keep the address
// it talks from instead of its configured endpoint: a peer behind NAT may
// come from anywhere, and a dead tunnel goes back to the configured one.
const roamGrace = 3 * time.Minute

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
	return key, SaveKey(path, key.String())
}

// SaveKey writes a private key made elsewhere (an edge's, by the control
// node).
func SaveKey(path, key string) error {
	if _, err := wgtypes.ParseKey(key); err != nil {
		return fmt.Errorf("invalid WireGuard key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(key+"\n"), 0o600)
}

// PublicKey is this node's WireGuard public key, base64.
func PublicKey(keyFile string) (string, error) {
	key, err := LoadOrCreateKey(keyFile)
	if err != nil {
		return "", err
	}
	return key.PublicKey().String(), nil
}

// Sync makes the mesh interface, its routes and the firewall match peers.
// Peers no longer listed are removed. access is what edges may reach on this
// node; nil without edges.
func (m *Mesh) Sync(ctx context.Context, self types.NodeIdentity, peers []types.Peer, access *types.EdgeAccess) error {
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
	c, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer c.Close()
	current := map[wgtypes.Key]wgtypes.Peer{}
	if dev, err := c.Device(Interface); err == nil {
		for _, p := range dev.Peers {
			current[p.PublicKey] = p
		}
	}
	cfg, err := peerConfig(key, peers, current, time.Now())
	if err != nil {
		return err
	}
	if err := c.ConfigureDevice(Interface, cfg); err != nil {
		return fmt.Errorf("configuring WireGuard: %w", err)
	}
	// Traffic for other nodes' subnets leaves through the mesh with this
	// node's mesh address as its source; the local subnet is more specific
	// and stays on the bridge. An edge also routes external apps' targets
	// to the nodes that forward to them.
	if err := run(ctx, "ip", "link", "set", Interface, "up"); err != nil {
		return err
	}
	routes := append([]string{self.ClusterCIDR}, peerRoutes(peers, endpoints(c))...)
	for _, r := range routes {
		if err := run(ctx, "ip", "route", "replace", r, "dev", Interface, "src", self.MeshIP); err != nil {
			return err
		}
	}
	pruneRoutes(ctx, self.MeshIP, routes)
	return firewall(ctx, self, peers, access)
}

// peerConfig is the device change that makes its peers match peers.
// Existing peers are updated in place; one that talked recently keeps the
// address it talks from.
func peerConfig(key wgtypes.Key, peers []types.Peer, current map[wgtypes.Key]wgtypes.Peer, now time.Time) (wgtypes.Config, error) {
	port := Port
	ka := keepalive
	cfg := wgtypes.Config{PrivateKey: &key, ListenPort: &port}
	want := map[wgtypes.Key]bool{}
	for _, p := range peers {
		pub, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			return cfg, fmt.Errorf("peer %s: %w", p.Name, err)
		}
		want[pub] = true
		allowed := []net.IPNet{}
		for _, s := range append([]string{p.Subnet}, p.Routes...) {
			_, ipnet, err := net.ParseCIDR(s)
			if err != nil {
				return cfg, fmt.Errorf("peer %s: %w", p.Name, err)
			}
			allowed = append(allowed, *ipnet)
		}
		pc := wgtypes.PeerConfig{PublicKey: pub, AllowedIPs: allowed, ReplaceAllowedIPs: true, PersistentKeepaliveInterval: &ka}
		if p.Endpoint != "" {
			cur, known := current[pub]
			if !known || cur.Endpoint == nil || now.Sub(cur.LastHandshakeTime) > roamGrace {
				ep, err := net.ResolveUDPAddr("udp", p.Endpoint)
				if err != nil {
					return cfg, fmt.Errorf("peer %s endpoint %s: %w", p.Name, p.Endpoint, err)
				}
				pc.Endpoint = ep
			}
		}
		cfg.Peers = append(cfg.Peers, pc)
	}
	for pub := range current {
		if !want[pub] {
			cfg.Peers = append(cfg.Peers, wgtypes.PeerConfig{PublicKey: pub, Remove: true})
		}
	}
	return cfg, nil
}

// peerRoutes are the extra routes into the mesh: external apps' targets on
// an edge. One that is where a peer is reached from (an edge on the same
// network as its nodes) is left out, or the mesh's own packets would loop
// into it.
func peerRoutes(peers []types.Peer, endpoints map[netip.Addr]bool) []string {
	var out []string
	for _, p := range peers {
		for _, r := range p.Routes {
			if pfx, err := netip.ParsePrefix(r); err == nil && endpoints[pfx.Addr()] {
				continue
			}
			out = append(out, r)
		}
	}
	return out
}

// endpoints are the addresses the device reaches its peers at now.
func endpoints(c *wgctrl.Client) map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	if dev, err := c.Device(Interface); err == nil {
		for _, p := range dev.Peers {
			if p.Endpoint != nil {
				if a, ok := netip.AddrFromSlice(p.Endpoint.IP); ok {
					out[a.Unmap()] = true
				}
			}
		}
	}
	return out
}

// pruneRoutes deletes routes through the mesh this node added (they carry
// its mesh address as source) that it no longer wants.
func pruneRoutes(ctx context.Context, meshIP string, want []string) {
	out, err := exec.CommandContext(ctx, "ip", "-4", "route", "show", "dev", Interface).Output()
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for _, r := range want {
		keep[normalizeRoute(r)] = true
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.Contains(" "+line+" ", " src "+meshIP+" ") || keep[normalizeRoute(f[0])] {
			continue
		}
		run(ctx, "ip", "route", "del", f[0], "dev", Interface)
	}
}

// normalizeRoute writes a destination the way "ip route" prints it: a
// single address without /32.
func normalizeRoute(r string) string {
	if p, err := netip.ParsePrefix(r); err == nil {
		if p.Bits() == 32 {
			return p.Addr().String()
		}
		return p.Masked().String()
	}
	return r
}

// firewall applies the mesh's rules (see Rules) in one atomic step, so no
// packet ever meets a half-written chain, and makes sure their chains are
// reached: from INPUT, and from FORWARD ahead of the VM network's rule that
// accepts everything from the mesh.
func firewall(ctx context.Context, self types.NodeIdentity, peers []types.Peer, access *types.EdgeAccess) error {
	cmd := exec.CommandContext(ctx, "iptables-restore", "-w", "--noflush")
	cmd.Stdin = strings.NewReader(RestoreScript(Rules(self, peers, access)))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables-restore: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if err := ensureFirst(ctx, "filter", "INPUT", [][]string{{"-i", Interface, "-j", chain}}); err != nil {
		return err
	}
	if err := ensureFirst(ctx, "filter", "FORWARD", [][]string{
		{"-i", Interface, "-j", forwardChain}, {"-o", Interface, "-j", forwardChain},
	}); err != nil {
		return err
	}
	nat := []string{"POSTROUTING", "-j", natChain}
	if run(ctx, "iptables", append([]string{"-w", "-t", "nat", "-C"}, nat...)...) != nil {
		return run(ctx, "iptables", append([]string{"-w", "-t", "nat", "-I"}, nat...)...)
	}
	return nil
}

// RestoreScript renders rules for "iptables-restore --noflush": declaring a
// chain creates it, or empties it if it exists, and each table's changes
// apply at once.
func RestoreScript(rules []Rule) string {
	var b strings.Builder
	for _, table := range []string{"filter", "nat"} {
		var chains, lines []string
		for _, r := range rules {
			t := r.Table
			if t == "" {
				t = "filter"
			}
			if t != table {
				continue
			}
			if len(r.Rule) == 0 {
				chains = append(chains, ":"+r.Chain+" - [0:0]")
			} else {
				lines = append(lines, "-A "+r.Chain+" "+strings.Join(r.Rule, " "))
			}
		}
		if len(chains) == 0 && len(lines) == 0 {
			continue
		}
		b.WriteString("*" + table + "\n")
		for _, l := range append(chains, lines...) {
			b.WriteString(l + "\n")
		}
		b.WriteString("COMMIT\n")
	}
	return b.String()
}

// ensureFirst puts jumps at the top of a built-in chain, in order, unless
// they are there already: ahead of anything that might accept mesh traffic
// wholesale (the VM network's rule, a firewall manager's chains).
func ensureFirst(ctx context.Context, table, chain string, rules [][]string) error {
	out, err := exec.CommandContext(ctx, "iptables", "-w", "-t", table, "-S", chain).Output()
	if err != nil {
		return err
	}
	var have []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(line, "-A ") {
			have = append(have, line)
		}
	}
	top := len(have) >= len(rules)
	for i, r := range rules {
		if top && have[i] != "-A "+chain+" "+strings.Join(r, " ") {
			top = false
		}
	}
	if top {
		return nil
	}
	for i := len(rules) - 1; i >= 0; i-- {
		for run(ctx, "iptables", append([]string{"-w", "-t", table, "-D", chain}, rules[i]...)...) == nil {
		}
		if err := run(ctx, "iptables", append([]string{"-w", "-t", table, "-I", chain, "1"}, rules[i]...)...); err != nil {
			return err
		}
	}
	return nil
}

// Rule is one iptables rule appended to Chain, or with no Rule, a chain to
// create and flush.
type Rule struct {
	Table string // "" for filter
	Chain string
	Rule  []string
}

// Rules are the mesh's firewall, chain by chain:
//
//   - INPUT from the mesh: nodes may reach jokku's ports, VMs on other nodes
//     may not. An edge accepts only nodes and replies: anything else on the
//     mesh is a VM it has no business with.
//   - Edges get only what their routes need: the API on the control node,
//     and the instances' web ports and external targets allowed here, on
//     this node or through it, where targets outside the cluster are
//     masqueraded so they answer.
func Rules(self types.NodeIdentity, peers []types.Peer, access *types.EdgeAccess) []Rule {
	var rs []Rule
	add := func(table, chain string, rule ...string) { rs = append(rs, Rule{table, chain, rule}) }
	for _, c := range []string{chain, edgeIn, forwardChain, edgeForward} {
		add("", c)
	}
	add("nat", natChain)

	var edges []string
	if access != nil {
		edges = access.Edges
	}
	established := []string{"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED"}
	edge := self.Role == types.RoleEdge
	if edge {
		add("", chain, append(established, "-j", "RETURN")...)
	}
	for _, e := range edges {
		add("", chain, "-s", e, "-j", edgeIn)
	}
	for _, ip := range append([]string{self.MeshIP}, nodeIPs(peers)...) {
		add("", chain, "-s", ip+"/32", "-j", "RETURN")
	}
	if edge {
		add("", chain, "-j", "DROP")
	} else {
		add("", chain, "-p", "tcp", "-m", "multiport", "--dports", ProtectedPorts, "-j", "DROP")
	}

	if len(edges) > 0 {
		add("", edgeIn, append(established, "-j", "ACCEPT")...)
		add("", edgeIn, "-p", "icmp", "-j", "ACCEPT")
		if access.Control {
			add("", edgeIn, "-p", "tcp", "--dport", "7443", "-j", "ACCEPT")
		}
		add("", forwardChain, append([]string{"-o", Interface}, append(established, "-j", "ACCEPT")...)...)
		for _, e := range edges {
			add("", forwardChain, "-i", Interface, "-s", e, "-j", edgeForward)
		}
		add("", edgeForward, append(established, "-j", "ACCEPT")...)
		for _, a := range access.Allow {
			ap, err := netip.ParseAddrPort(a)
			if err != nil {
				continue
			}
			// The same address may be this node's own (INPUT) or one it
			// forwards to (FORWARD); a rule in each covers both.
			for _, c := range []string{edgeIn, edgeForward} {
				add("", c, "-p", "tcp", "-d", ap.Addr().String()+"/32", "--dport", fmt.Sprint(ap.Port()), "-j", "ACCEPT")
			}
		}
		add("", edgeIn, "-j", "DROP")
		add("", edgeForward, "-j", "DROP")
		for _, e := range edges {
			for _, t := range access.Targets {
				add("nat", natChain, "-s", e, "-d", t+"/32", "-j", "MASQUERADE")
			}
		}
	}
	return rs
}

// nodeIPs are the mesh addresses of the peers that are nodes, not edges.
func nodeIPs(peers []types.Peer) []string {
	var out []string
	for _, p := range peers {
		if p.Role != types.RoleEdge {
			out = append(out, p.MeshIP)
		}
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
