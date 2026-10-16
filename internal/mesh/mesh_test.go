package mesh

import (
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/wes/jokku/internal/types"
)

func key(t *testing.T) wgtypes.Key {
	k, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestPeersKeepTheAddressTheyTalkFrom checks a peer behind NAT isn't sent
// back to its configured endpoint while its tunnel is up, an edge never
// dials, and stale peers are removed rather than the whole set replaced.
func TestPeersKeepTheAddressTheyTalkFrom(t *testing.T) {
	now := time.Now()
	self := key(t)
	talking, quiet, fresh, gone := key(t).PublicKey(), key(t).PublicKey(), key(t).PublicKey(), key(t).PublicKey()
	roamed := &net.UDPAddr{IP: net.ParseIP("198.51.100.7"), Port: 40123}
	current := map[wgtypes.Key]wgtypes.Peer{
		talking: {PublicKey: talking, Endpoint: roamed, LastHandshakeTime: now.Add(-time.Minute)},
		quiet:   {PublicKey: quiet, Endpoint: roamed, LastHandshakeTime: now.Add(-time.Hour)},
		gone:    {PublicKey: gone},
	}
	peers := []types.Peer{
		{Name: "talking", PublicKey: talking.String(), Endpoint: "192.168.1.10:51820", Subnet: "10.210.2.0/24"},
		{Name: "quiet", PublicKey: quiet.String(), Endpoint: "192.168.1.11:51820", Subnet: "10.210.3.0/24"},
		// As an edge sees a home node: no endpoint, and a LAN target behind it.
		{Name: "home", PublicKey: fresh.String(), Subnet: "10.210.1.0/24", Routes: []string{"192.168.1.50/32"}},
	}
	cfg, unusable, err := peerConfig(self, peers, current, now)
	if err != nil || len(unusable) > 0 {
		t.Fatal(err, unusable)
	}
	if cfg.ReplacePeers {
		t.Error("replacing every peer would forget the addresses they talk from")
	}
	byKey := map[wgtypes.Key]wgtypes.PeerConfig{}
	for _, p := range cfg.Peers {
		byKey[p.PublicKey] = p
	}
	if p := byKey[talking]; p.Endpoint != nil {
		t.Errorf("a peer with a recent handshake got its endpoint reset to %v", p.Endpoint)
	}
	if p := byKey[quiet]; p.Endpoint == nil || p.Endpoint.String() != "192.168.1.11:51820" {
		t.Errorf("a quiet peer should go back to its configured endpoint, got %v", p.Endpoint)
	}
	home := byKey[fresh]
	if home.Endpoint != nil {
		t.Errorf("an edge dialed %v; the nodes behind it dial it", home.Endpoint)
	}
	if len(home.AllowedIPs) != 2 || home.AllowedIPs[1].String() != "192.168.1.50/32" || home.PersistentKeepaliveInterval == nil {
		t.Errorf("home peer: %+v", home)
	}
	if p, ok := byKey[gone]; !ok || !p.Remove {
		t.Error("a peer no longer listed should be removed")
	}
}

// TestUnusableEndpointKeepsTheMesh checks a peer whose endpoint has no
// address (a control node that started before its network, which registered
// ":51820") is set up without one, and named, instead of failing the device
// for every peer, while a peer it talks with keeps its tunnel.
func TestUnusableEndpointKeepsTheMesh(t *testing.T) {
	now := time.Now()
	self := key(t)
	control, talking, worker := key(t).PublicKey(), key(t).PublicKey(), key(t).PublicKey()
	current := map[wgtypes.Key]wgtypes.Peer{
		talking: {PublicKey: talking, Endpoint: &net.UDPAddr{IP: net.ParseIP("192.168.1.9"), Port: 51820}, LastHandshakeTime: now.Add(-time.Minute)},
	}
	peers := []types.Peer{
		{Name: "control", PublicKey: control.String(), Endpoint: ":51820", Subnet: "10.210.1.0/24"},
		{Name: "talking", PublicKey: talking.String(), Endpoint: ":51820", Subnet: "10.210.2.0/24"},
		{Name: "unspecified", PublicKey: key(t).PublicKey().String(), Endpoint: "0.0.0.0:51820", Subnet: "10.210.4.0/24"},
		{Name: "worker", PublicKey: worker.String(), Endpoint: "192.168.1.11:51820", Subnet: "10.210.3.0/24"},
	}
	cfg, unusable, err := peerConfig(self, peers, current, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"control (:51820)", "unspecified (0.0.0.0:51820)"}; !slices.Equal(unusable, want) {
		t.Errorf("unusable = %q, want %q", unusable, want)
	}
	if len(cfg.Peers) != 4 {
		t.Fatalf("every peer should be set up, got %d", len(cfg.Peers))
	}
	for _, p := range cfg.Peers {
		switch p.PublicKey {
		case worker:
			if p.Endpoint == nil || p.Endpoint.String() != "192.168.1.11:51820" {
				t.Errorf("worker endpoint = %v", p.Endpoint)
			}
		default:
			if p.Endpoint != nil {
				t.Errorf("%s got endpoint %v", p.PublicKey, p.Endpoint)
			}
		}
	}
}

func rules(rs []Rule) string {
	var b strings.Builder
	for _, r := range rs {
		if len(r.Rule) > 0 {
			b.WriteString(r.Table + "/" + r.Chain + " " + strings.Join(r.Rule, " ") + "\n")
		}
	}
	return b.String()
}

func TestRulesForANodeBehindAnEdge(t *testing.T) {
	self := types.NodeIdentity{Name: "home", Role: "control", MeshIP: "10.210.1.1", ClusterCIDR: "10.210.0.0/16"}
	peers := []types.Peer{
		{Name: "w1", MeshIP: "10.210.2.1"},
		{Name: "edge1", Role: types.RoleEdge, MeshIP: "10.210.3.1"},
	}
	access := &types.EdgeAccess{Edges: []string{"10.210.3.0/24"}, Allow: []string{"10.210.1.5:5000", "192.168.1.50:8123"},
		Targets: []string{"192.168.1.50"}, Control: true}
	got := rules(Rules(self, peers, access))
	for _, want := range []string{
		"/JOKKU-MESH -s 10.210.3.0/24 -j JOKKU-EDGE-IN",
		"/JOKKU-MESH -s 10.210.2.1/32 -j RETURN",
		"/JOKKU-EDGE-IN -p tcp --dport 7443 -j ACCEPT",
		"/JOKKU-EDGE-FWD -p tcp -d 10.210.1.5/32 --dport 5000 -j ACCEPT",
		"/JOKKU-EDGE-FWD -p tcp -d 192.168.1.50/32 --dport 8123 -j ACCEPT",
		"/JOKKU-EDGE-FWD -j DROP",
		"/JOKKU-MESH-FWD -i jokku-wg -s 10.210.3.0/24 -j JOKKU-EDGE-FWD",
		"nat/JOKKU-MESH-NAT -s 10.210.3.0/24 -d 192.168.1.50/32 -j MASQUERADE",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The edge is no node: it doesn't get the nodes' free pass.
	if strings.Contains(got, "-s 10.210.3.1/32 -j RETURN") {
		t.Errorf("the edge's address is let through like a node's:\n%s", got)
	}
	// Edge rules come before the nodes' pass and the drop of jokku's ports.
	if strings.Index(got, "JOKKU-EDGE-IN\n") > strings.Index(got, "-j RETURN") {
		t.Errorf("edges must be sorted out first:\n%s", got)
	}

	// A worker isn't the control node: no API for edges there.
	access.Control = false
	if got := rules(Rules(self, peers, access)); strings.Contains(got, "--dport 7443 -j ACCEPT") {
		t.Errorf("a worker lets edges reach 7443:\n%s", got)
	}
}

func TestRulesOnAnEdge(t *testing.T) {
	self := types.NodeIdentity{Name: "edge1", Role: types.RoleEdge, MeshIP: "10.210.3.1"}
	got := rules(Rules(self, []types.Peer{{Name: "home", MeshIP: "10.210.1.1"}}, nil))
	want := "/JOKKU-MESH -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN\n" +
		"/JOKKU-MESH -s 10.210.3.1/32 -j RETURN\n" +
		"/JOKKU-MESH -s 10.210.1.1/32 -j RETURN\n" +
		"/JOKKU-MESH -j DROP\n"
	if got != want {
		t.Errorf("an edge accepts only nodes and replies over the mesh; got:\n%s", got)
	}
}

func TestRestoreScript(t *testing.T) {
	got := RestoreScript([]Rule{{Chain: "A"}, {Chain: "A", Rule: []string{"-j", "DROP"}}, {Table: "nat", Chain: "N"}})
	want := "*filter\n:A - [0:0]\n-A A -j DROP\nCOMMIT\n*nat\n:N - [0:0]\nCOMMIT\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestNormalizeRoute(t *testing.T) {
	for in, want := range map[string]string{"192.168.1.50/32": "192.168.1.50", "10.210.0.0/16": "10.210.0.0/16", "192.168.1.50": "192.168.1.50"} {
		if got := normalizeRoute(in); got != want {
			t.Errorf("normalizeRoute(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRoutesSkipPeerEndpoints(t *testing.T) {
	peers := []types.Peer{{Name: "home", Routes: []string{"192.168.1.50/32", "192.168.78.1/32"}}}
	got := peerRoutes(peers, map[netip.Addr]bool{netip.MustParseAddr("192.168.78.1"): true})
	if len(got) != 1 || got[0] != "192.168.1.50/32" {
		t.Errorf("a route to where a peer is reached would loop the mesh into itself: %v", got)
	}
}
