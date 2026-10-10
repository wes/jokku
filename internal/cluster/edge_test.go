package cluster_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/agent"
	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/types"
)

// addEdge adds an edge the way "jokku edge:add" and "jokku setup --edge"
// do, and starts its agent. In these tests it reaches the control node's
// API directly rather than over the mesh.
func (h *harness) addEdge(name string) (*node, *cluster.EdgeBundle) {
	h.t.Helper()
	e, err := h.client.CreateEdge(h.ctx, types.CreateEdgeRequest{Name: name, Address: "203.0.113.7"})
	if err != nil {
		h.t.Fatal(err)
	}
	b, err := cluster.ParseEdgeBundle(e.Bundle)
	if err != nil {
		h.t.Fatal(err)
	}
	if !strings.Contains(e.Command, "--edge "+e.Bundle) {
		h.t.Fatalf("the install command doesn't carry the bundle: %s", e.Command)
	}
	return h.startNode(name, b.NodeToken, b.AgentToken, h.t.TempDir(), h.remote(b.NodeToken)), b
}

func (p *fakeProxy) get() *types.ProxyState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ps
}

func peerNamed(peers []types.Peer, name string) *types.Peer {
	for i := range peers {
		if peers[i].Name == name {
			return &peers[i]
		}
	}
	return nil
}

func TestEdgeRoutesToTheNodesBehindIt(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	edge, bundle := h.addEdge("edge1")
	if bundle.Control != "10.210.1.1:7443" || len(bundle.Peers) != 1 || bundle.Peers[0].Name != "control" || bundle.Peers[0].Endpoint != "" {
		t.Errorf("the edge should start with the control node as its peer, reached over the mesh: %+v", bundle)
	}
	if bundle.Node.Role != types.RoleEdge || bundle.Node.MeshIP != "10.210.3.1" {
		t.Errorf("edge identity: %+v", bundle.Node)
	}
	eventually(t, 5*time.Second, "all three nodes report in", func() error {
		st, _ := h.ctl.Status(h.ctx)
		if st.Totals.NodesReady != 3 {
			return fmt.Errorf("%d of 3 nodes ready", st.Totals.NodesReady)
		}
		return nil
	})
	h.deploy("web", 4)

	// Nothing runs on the edge, though its (fake) runtime could.
	if got := h.placement("web"); got["edge1"] != 0 || got["control"]+got["w1"] != 4 {
		t.Fatalf("placement %v: nothing may run on an edge", got)
	}
	if n := edge.rt.startCount(); n != 0 {
		t.Errorf("the edge started %d VMs", n)
	}
	// It routes to every instance, behind it.
	eventually(t, 3*time.Second, "the edge routes to every instance", func() error {
		if got := edge.proxy.upstreams("web"); len(got) != 4 {
			return fmt.Errorf("edge routes to %v", got)
		}
		return nil
	})

	// The edge dials nobody; the nodes dial it.
	eventually(t, 3*time.Second, "mesh peers", func() error {
		peers, access := edge.mesh.get()
		if names := edge.mesh.names(); strings.Join(names, ",") != "control,w1" {
			return fmt.Errorf("edge peers %v", names)
		}
		for _, p := range peers {
			if p.Endpoint != "" || p.AgentAddr != "" || p.Role != "" {
				return fmt.Errorf("edge's peer %s: %+v", p.Name, p)
			}
		}
		if access != nil {
			return fmt.Errorf("an edge got edge access rules: %+v", access)
		}
		for _, name := range []string{"control", "w1"} {
			peers, _ := h.nodes[name].mesh.get()
			p := peerNamed(peers, "edge1")
			if p == nil || p.Role != types.RoleEdge || p.Endpoint != "203.0.113.7:51820" || p.AgentAddr != "" {
				return fmt.Errorf("%s's edge peer: %+v", name, p)
			}
		}
		return nil
	})
	if self := edge.mesh.self; self.Role != types.RoleEdge {
		t.Errorf("the edge's identity has role %q", self.Role)
	}

	// Each node lets the edge reach its own instances' web ports, and only
	// the control node its API.
	insts, _ := h.client.Instances(h.ctx, "web")
	for _, name := range []string{"control", "w1"} {
		_, access := h.nodes[name].mesh.get()
		if access == nil || !slices.Equal(access.Edges, []string{"10.210.3.0/24"}) || access.Control != (name == "control") {
			t.Fatalf("%s's edge access: %+v", name, access)
		}
		var want []string
		for _, in := range insts {
			if in.Node == name {
				want = append(want, fmt.Sprintf("%s:%d", in.IP, in.Port))
			}
		}
		slices.Sort(want)
		if !slices.Equal(access.Allow, want) {
			t.Errorf("%s lets the edge reach %v, want its instances %v", name, access.Allow, want)
		}
	}

	// The edge never gets an app's root filesystem.
	req, _ := http.NewRequest("GET", h.ts.URL+"/v1/agent/artifacts/web.ext4", nil)
	req.Header.Set("Authorization", "Bearer "+bundle.NodeToken)
	pinned := &http.Client{Transport: &http.Transport{TLSClientConfig: cluster.PinnedTLS(h.pin)}}
	resp, err := pinned.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("an edge downloading an artifact got %s", resp.Status)
	}
	if _, err := h.client.SetNodeFlag(h.ctx, "edge1", "schedulable", true); err == nil {
		t.Error("an edge was made schedulable")
	}

	// An edge that never connected goes at once.
	if _, err := h.client.CreateEdge(h.ctx, types.CreateEdgeRequest{Name: "edge2", Address: "edge2.example.test"}); err != nil {
		t.Fatal(err)
	}
	if err := h.client.RemoveEdge(h.ctx, "edge2"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.Node(h.ctx, "edge2"); err == nil {
		t.Error("an edge that never connected is still there")
	}

	// Removing one revokes its credentials at once, but keeps it a peer a
	// moment, so it hears it was removed and stops routing; then it's gone
	// from every node's mesh. It held the session key, so that changes.
	key, err := h.ctl.AuthKey(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.client.RemoveEdge(h.ctx, "edge1"); err != nil {
		t.Fatal(err)
	}
	if now, _ := h.ctl.AuthKey(h.ctx); now == key {
		t.Error("removing an edge kept the session key it had")
	}
	if edges, _ := h.client.Edges(h.ctx); len(edges) != 0 {
		t.Errorf("edge:list still shows %v", edges)
	}
	eventually(t, 5*time.Second, "the edge is gone", func() error {
		peers, access := h.nodes["control"].mesh.get()
		if peerNamed(peers, "edge1") != nil || access != nil {
			return fmt.Errorf("control still has the edge: %v, %+v", peers, access)
		}
		if ps := edge.proxy.get(); ps == nil || len(ps.Routes) != 0 {
			return fmt.Errorf("the removed edge still routes")
		}
		return nil
	})
}

func TestExternalAppsAreReachedThroughTheirNode(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	edge, _ := h.addEdge("edge1")
	ctx := h.ctx

	for _, bad := range []types.ExternalRequest{
		{Name: "ha", URL: "http://homeassistant.local:8123"},
		{Name: "ha", URL: "http://127.0.0.1:8123"},
		{Name: "ha", URL: "http://10.210.1.5:5000"},
		{Name: "ha", URL: "ftp://192.168.1.50"},
		{Name: "ha", URL: "http://8.8.8.8"},
		{Name: "ha", URL: "http://192.168.1.50:8123/lovelace"},
		{Name: "ha", URL: "http://192.168.1.50:8123", Via: "edge1"},
		{Name: "ha", URL: "http://192.168.1.50:8123", Via: "nosuchnode"},
	} {
		if _, err := h.client.CreateExternal(ctx, bad); err == nil {
			t.Errorf("created %+v", bad)
		}
	}
	e, err := h.client.CreateExternal(ctx, types.ExternalRequest{Name: "ha", URL: "http://192.168.1.50:8123", Via: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Via != "w1" || e.URL != "http://192.168.1.50:8123" {
		t.Errorf("created %+v", e)
	}
	if _, err := h.client.CreateExternal(ctx, types.ExternalRequest{Name: "ha2", URL: "http://192.168.1.50:9000"}); err == nil {
		t.Error("a second app at the same address through another node")
	}
	if _, err := h.client.PatchDomains(ctx, "ha", types.DomainsPatch{Add: []string{"ha.example.test"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, "the edge and w1 route to the target", func() error {
		for _, n := range []*node{h.nodes["w1"], edge} {
			if got := n.proxy.upstreams("ha"); !slices.Equal(got, []string{"192.168.1.50:8123"}) {
				return fmt.Errorf("%s routes ha to %v", n.name, got)
			}
		}
		// The control node isn't on its network.
		if got := h.nodes["control"].proxy.upstreams("ha"); len(got) != 0 {
			return fmt.Errorf("control routes ha to %v", got)
		}
		return nil
	})
	eventually(t, 3*time.Second, "the edge reaches the target through w1", func() error {
		peers, _ := edge.mesh.get()
		if w1 := peerNamed(peers, "w1"); w1 == nil || !slices.Equal(w1.Routes, []string{"192.168.1.50/32"}) {
			return fmt.Errorf("edge's w1 peer: %+v", w1)
		}
		if c := peerNamed(peers, "control"); c == nil || len(c.Routes) != 0 {
			return fmt.Errorf("edge's control peer: %+v", c)
		}
		_, access := h.nodes["w1"].mesh.get()
		if access == nil || !slices.Contains(access.Allow, "192.168.1.50:8123") || !slices.Equal(access.Targets, []string{"192.168.1.50"}) {
			return fmt.Errorf("w1's edge access: %+v", access)
		}
		if _, access := h.nodes["control"].mesh.get(); access == nil || len(access.Targets) != 0 {
			return fmt.Errorf("control's edge access: %+v", access)
		}
		return nil
	})

	// An external app is only routed to: it has no deploys or processes.
	if _, err := h.client.Scale(ctx, "ha", types.ScaleRequest{Quantities: map[string]int{"web": 1}}); err == nil {
		t.Error("an external app was scaled")
	}
	apps, _ := h.client.Apps(ctx)
	for _, a := range apps {
		if a.Name == "ha" {
			t.Error("apps:list shows an external app")
		}
	}

	// Moving it to an https target through the control node.
	insecure := true
	if _, err := h.client.PatchExternal(ctx, "ha", types.ExternalRequest{URL: "https://192.168.1.60:8443", Via: "control", Insecure: &insecure}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, "the change reaches the edge", func() error {
		ps := edge.proxy.get()
		for _, r := range ps.Routes {
			if r.App == "ha" && len(r.Hosts) > 0 && (!r.UpstreamTLS || !r.Insecure || !slices.Equal(r.Upstreams, []string{"192.168.1.60:8443"})) {
				return fmt.Errorf("edge's route: %+v", r)
			}
		}
		peers, _ := edge.mesh.get()
		if c := peerNamed(peers, "control"); c == nil || !slices.Equal(c.Routes, []string{"192.168.1.60/32"}) {
			return fmt.Errorf("edge's control peer: %+v", c)
		}
		return nil
	})

	if err := h.client.DestroyExternal(ctx, "ha"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, "routes go with it", func() error {
		if got := edge.proxy.upstreams("ha"); len(got) != 0 {
			return fmt.Errorf("edge still routes ha to %v", got)
		}
		return nil
	})
}

func TestLoginsReachEveryProxy(t *testing.T) {
	h := newHarness(t)
	edge, _ := h.addEdge("edge1")
	ctx := h.ctx
	h.deploy("web", 1)

	if _, err := h.client.PatchAuth(ctx, "web", types.AuthPatch{Mode: types.AuthPassword}); err == nil {
		t.Error("a password login without a password")
	}
	if _, err := h.client.PatchAuth(ctx, "web", types.AuthPatch{Mode: types.AuthPassword, Password: "1234"}); err == nil {
		t.Error("a 4-character password")
	}
	if _, err := h.client.PatchAuth(ctx, "web", types.AuthPatch{Mode: types.AuthPassword, Password: "123456"}); err != nil {
		t.Fatal(err)
	}
	route := func() (*types.RouteAuth, *types.ProxyAuth) {
		ps := edge.proxy.get()
		if ps == nil {
			return nil, nil
		}
		for _, r := range ps.Routes {
			if r.App == "web" && len(r.Hosts) > 0 {
				return r.Auth, ps.Auth
			}
		}
		return nil, ps.Auth
	}
	eventually(t, 3*time.Second, "the edge has the login", func() error {
		ra, pa := route()
		if ra == nil || ra.Mode != types.AuthPassword || !strings.HasPrefix(ra.PasswordHash, "$2") || pa == nil || pa.Key == "" {
			return fmt.Errorf("route %+v, settings %+v", ra, pa)
		}
		return nil
	})

	// Users: only ones that exist, with their TOTP secret handed to proxies.
	users := []string{"wes"}
	if _, err := h.client.PatchAuth(ctx, "web", types.AuthPatch{Mode: types.AuthUsers, Users: &users}); err == nil {
		t.Error("allowed a user that doesn't exist")
	}
	if _, err := h.client.CreateAuthUser(ctx, types.AuthUserRequest{Name: "wes", Password: "short"}); err == nil {
		t.Error("a user with a 5-character password")
	}
	totp := true
	u, err := h.client.CreateAuthUser(ctx, types.AuthUserRequest{Name: "wes", Password: "correct horse", TOTP: &totp})
	if err != nil {
		t.Fatal(err)
	}
	if u.TOTPSecret == "" || !strings.HasPrefix(u.TOTPURI, "otpauth://totp/") {
		t.Errorf("new user: %+v", u)
	}
	if _, err := h.client.PatchAuth(ctx, "web", types.AuthPatch{Mode: types.AuthUsers, Users: &users,
		AllowIPs: &[]string{"192.168.1.0/24", "10.0.0.7"}, BypassPaths: &[]string{"/api/webhook/*"}}); err != nil {
		t.Fatal(err)
	}
	sh, err := h.client.CreateAuthShare(ctx, "web", types.CreateShareRequest{TTLSeconds: 3600, Note: "for Sam"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sh.URL, "http://web.example.test/.jokku/share/") {
		t.Errorf("share link %s", sh.URL)
	}
	eventually(t, 3*time.Second, "the edge has the users login", func() error {
		ra, pa := route()
		if ra == nil || ra.Mode != types.AuthUsers || !slices.Equal(ra.Users, users) || len(ra.Shares) != 1 ||
			!slices.Equal(ra.AllowIPs, []string{"192.168.1.0/24", "10.0.0.7/32"}) || !slices.Equal(ra.BypassPaths, []string{"/api/webhook/*"}) {
			return fmt.Errorf("route %+v", ra)
		}
		if pa == nil || len(pa.Users) != 1 || pa.Users[0].TOTPSecret != u.TOTPSecret {
			return fmt.Errorf("settings %+v", pa)
		}
		return nil
	})

	// The login domain can't be an app's.
	if _, err := h.client.PatchAuth(ctx, "", types.AuthPatch{LoginDomain: ptr("web.example.test")}); err == nil {
		t.Error("the login domain took an app's domain")
	}
	if _, err := h.client.PatchAuth(ctx, "", types.AuthPatch{LoginDomain: ptr("auth.example.test"), SessionDays: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.PatchDomains(ctx, "web", types.DomainsPatch{Add: []string{"auth.example.test"}}); err == nil {
		t.Error("an app took the login domain")
	}
	if err := h.client.DeleteAuthShare(ctx, "web", sh.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, "settings and revocations reach the edge", func() error {
		ra, pa := route()
		if pa == nil || pa.LoginDomain != "auth.example.test" || pa.SessionDays != 7 || ra == nil || len(ra.Shares) != 0 {
			return fmt.Errorf("route %+v, settings %+v", ra, pa)
		}
		return nil
	})

	if _, err := h.client.PatchAuth(ctx, "web", types.AuthPatch{Mode: "off"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, "the login is gone", func() error {
		if ra, _ := route(); ra != nil {
			return fmt.Errorf("route still has %+v", ra)
		}
		return nil
	})
}

func ptr[T any](v T) *T { return &v }

// Edges have no vote in fencing: they run nothing and sit outside. Counted
// as nodes a cut-off node can't ask, two of them would make w1 think itself
// cut off when the control node is down for everyone, and stop its
// database for nothing.
func TestEdgesHaveNoVoteInFencing(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.join("w2")
	h.addEdge("edge1")
	h.addEdge("edge2")
	h.waitReady(5)
	undo := h.onlyOn("w1")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()
	v := h.volume("db", "data")
	h.backedUp(filepath.Join(t.TempDir(), "bucket"), v, 0)

	// w1 knows the volume would be restored elsewhere, so fencing applies.
	eventually(t, 5*time.Second, "w1 has the volume's auto-restore", func() error {
		b, err := os.ReadFile(filepath.Join(h.nodes["w1"].dir, "agent-state.json"))
		if err != nil || !strings.Contains(string(b), `"auto_restore":true`) {
			return errFmt("not yet")
		}
		return nil
	})
	h.down.Store(true)
	time.Sleep(4 * agent.FenceAfter)
	if h.nodes["w1"].rt.apps()["db"] != 1 {
		t.Error("w1 stopped db while the control node was down for every node")
	}
	h.down.Store(false)
	eventually(t, 10*time.Second, "db still on w1", func() error { return h.healthy("db", "w1", "") })
}
