package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wes/jokku/internal/dns"
	"github.com/wes/jokku/internal/props"
	"github.com/wes/jokku/internal/proxy"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// PollTimeout is how long a long-poll waits for a change before answering
// with the unchanged state, and PollRecheck how often it recomputes.
var (
	PollTimeout = 25 * time.Second
	PollRecheck = 2 * time.Second
)

// State returns node's desired state once it differs from etag, or after
// pollTimeout. An empty etag returns right away.
func (c *Controller) State(ctx context.Context, node, etag string) (*types.NodeState, error) {
	deadline := time.Now().Add(PollTimeout)
	for {
		ch := c.changes()
		st, err := c.ComputeState(ctx, node)
		if err != nil || st.ETag != etag || time.Now().After(deadline) {
			return st, err
		}
		// Recompute every two seconds even without a signal, so a change
		// made by a path that forgot to call Changed still arrives.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		case <-time.After(PollRecheck):
		}
	}
}

// ComputeState builds a node's desired state from the store.
func (c *Controller) ComputeState(ctx context.Context, name string) (*types.NodeState, error) {
	node, err := c.Store.Node(ctx, name)
	if err != nil {
		return nil, err
	}
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	subnet, meshIP := NodeSubnet(c.ClusterCIDR, node.SubnetIndex)
	st := &types.NodeState{
		Version: c.Version,
		Node: types.NodeIdentity{Name: node.Name, Role: node.Role, Subnet: subnet.String(), MeshIP: meshIP.String(),
			ClusterCIDR: c.ClusterCIDR.String()},
		Peers:     []types.Peer{},
		Instances: []types.InstanceSpec{},
	}
	apps, err := c.Store.Apps(ctx)
	if err != nil {
		return nil, err
	}
	ext, err := c.externals(ctx, apps, nodes)
	if err != nil {
		return nil, err
	}
	if node.WGPublicKey != "" {
		st.Peers = c.peers(*node, nodes, ext)
	}
	stopped := map[string]bool{}
	for _, a := range apps {
		stopped[a.Name] = a.Stopped
	}
	insts, err := c.Store.Instances(ctx, "")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	releases := map[int64]*store.Release{}
	policies := map[string]int{}
	for _, in := range insts {
		if in.Node != name || !held(in, now) {
			continue
		}
		rel := releases[in.ReleaseID]
		if rel == nil {
			if rel, err = c.Store.Release(ctx, in.ReleaseID); err != nil {
				return nil, err
			}
			releases[in.ReleaseID] = rel
		}
		max, ok := policies[in.App]
		if !ok {
			max = c.maxRestarts(ctx, in.App)
			policies[in.App] = max
		}
		artifact, sha, size, img := rel.ImageFor(in.ProcessType)
		spec := types.InstanceSpec{
			ID: in.ID, App: in.App, Process: in.Name(), ProcessType: in.ProcessType, Run: !stopped[in.App],
			Artifact: filepath.Base(artifact), ArtifactSHA256: sha, ArtifactSize: size,
			IP: in.IP, Port: in.Port, CPUs: in.CPUs, MemoryMB: in.MemoryMB,
			Argv: rel.Processes[in.ProcessType], Env: Env(rel, in), User: img.User, WorkDir: img.WorkingDir,
			Hostname: Hostname(in), MaxRestarts: max, Volumes: in.Volumes, StopSignal: img.StopSignal,
		}
		if svc, ok := rel.Services[in.ProcessType]; ok {
			spec.Check, spec.StopTimeout = svc.Check, svc.StopSecs
			if svc.Restart != "" {
				spec.MaxRestarts = restartBudget(svc.Restart)
			}
		}
		st.Instances = append(st.Instances, spec)
	}
	if st.Volumes, err = c.volumeSpecs(ctx, name, nodes); err != nil {
		return nil, err
	}
	current := map[string]*store.Release{}
	for _, a := range apps {
		if cur, err := c.Store.CurrentRelease(ctx, a.Name); err == nil && cur != nil {
			current[a.Name] = cur
		}
	}
	if !node.Edge() { // it runs no VMs to look names up
		st.DNS = dnsRecords(apps, current, insts, nodes, now)
		if st.EdgeAccess, err = c.edgeAccess(ctx, *node, nodes, apps, current, insts, ext); err != nil {
			return nil, err
		}
	}
	if node.Ingress {
		if st.Proxy, err = c.proxyState(ctx, *node, apps, current, insts, nodes, now, ext); err != nil {
			return nil, err
		}
	}
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	st.ETag = hex.EncodeToString(sum[:8])
	return st, nil
}

// dnsRecords are the internal names of every running app's wanted instances
// on nodes that are up.
func dnsRecords(apps []types.App, current map[string]*store.Release, insts []store.Instance, nodes []store.Node, now time.Time) map[string][]string {
	ready := map[string]bool{}
	for _, n := range nodes {
		ready[n.Name] = n.Ready(now)
	}
	stopped := map[string]bool{}
	for _, a := range apps {
		stopped[a.Name] = a.Stopped
	}
	var running []dns.Instance
	for _, in := range insts {
		if in.Desired != store.DesiredRunning || stopped[in.App] || !ready[in.Node] {
			continue
		}
		web := "web"
		if cur := current[in.App]; cur != nil {
			web = cur.WebProcess()
		}
		running = append(running, dns.Instance{
			App: in.App, Process: in.ProcessType, IP: in.IP, Healthy: in.State == store.StateHealthy, Web: in.ProcessType == web,
		})
	}
	return dns.Records(running)
}

// held reports whether an instance's node should hold it: wanted, or retiring
// but still inside its wait-to-retire window (finishing in-flight requests).
func held(in store.Instance, now time.Time) bool {
	return in.Desired == store.DesiredRunning || (in.RetireAt != nil && now.Before(*in.RetireAt))
}

// proxyState routes each deployed app's domains to the healthy web instances
// of its current release, on whichever nodes they run, and each external
// app's to its target. Only edges and the node an external app goes through
// route it: they are the ones that reach its network.
func (c *Controller) proxyState(ctx context.Context, node store.Node, apps []types.App, current map[string]*store.Release,
	insts []store.Instance, nodes []store.Node, now time.Time, ext map[string]External) (*types.ProxyState, error) {
	ready := map[string]bool{}
	for _, n := range nodes {
		ready[n.Name] = n.Ready(now)
	}
	le, _ := props.Lookup("letsencrypt")
	px, _ := props.Lookup("proxy")
	globalLE, err := c.Store.Properties(ctx, "", "letsencrypt")
	if err != nil {
		return nil, err
	}
	globalProxy, err := c.Store.Properties(ctx, "", "proxy")
	if err != nil {
		return nil, err
	}
	auth, logins, err := c.authState(ctx, apps)
	if err != nil {
		return nil, err
	}
	ps := &types.ProxyState{Routes: []types.ProxyRoute{}, Email: globalLE["email"], Auth: auth}
	for _, app := range apps {
		e, external := ext[app.Name]
		if (app.CurrentRelease == 0 && !external) || (external && !node.Edge() && node.Name != e.Via) {
			continue
		}
		appProxy, _ := c.Store.Properties(ctx, app.Name, "proxy")
		if props.Compute(px, appProxy, globalProxy)["enabled"] != "true" {
			continue
		}
		cur := current[app.Name]
		if cur == nil && !external {
			continue
		}
		domains, err := c.Store.Domains(ctx, app.Name)
		if err != nil {
			return nil, err
		}
		upstreams := []string{}
		switch {
		case external:
			upstreams = append(upstreams, e.Target.String())
		case !app.Stopped:
			for _, in := range insts {
				// Only wanted instances get new requests; retiring ones are
				// left to finish what they have.
				if in.App == app.Name && in.ReleaseID == cur.ID && in.ProcessType == cur.WebProcess() &&
					in.Desired == store.DesiredRunning && in.State == store.StateHealthy && ready[in.Node] {
					upstreams = append(upstreams, fmt.Sprintf("%s:%d", in.IP, in.Port))
				}
			}
		}
		sort.Strings(upstreams)
		appLE, _ := c.Store.Properties(ctx, app.Name, "letsencrypt")
		tlsOn := props.Compute(le, appLE, globalLE)["enabled"] == "true"
		plain := types.ProxyRoute{App: app.Name, Upstreams: upstreams, Hosts: []string{}, Auth: logins[app.Name]}
		if external {
			plain.UpstreamTLS, plain.Insecure = e.TLS, e.Insecure
		}
		secure := plain
		secure.Hosts, secure.TLS = []string{}, true
		for _, d := range domains {
			if tlsOn && proxy.PublicHost(d) {
				secure.Hosts = append(secure.Hosts, d)
			} else {
				plain.Hosts = append(plain.Hosts, d)
			}
		}
		ps.Routes = append(ps.Routes, plain, secure)
	}
	return ps, nil
}

// peers is node's WireGuard peer list. Edges don't peer with each other,
// and dial nobody: every other node dials them, since those nodes may be
// behind NAT. An edge reaches each external app's target through its via
// node. Edges have no say in fencing (see agent/fence.go), so they get no
// agent address.
func (c *Controller) peers(node store.Node, nodes []store.Node, ext map[string]External) []types.Peer {
	via := map[string][]string{}
	if node.Edge() {
		seen := map[netip.Addr]bool{}
		for _, app := range sortedKeys(ext) {
			e := ext[app]
			if ip := e.Target.Addr(); !seen[ip] {
				seen[ip] = true
				via[e.Via] = append(via[e.Via], netip.PrefixFrom(ip, 32).String())
			}
		}
	}
	peers := []types.Peer{}
	for _, n := range nodes {
		if n.Name == node.Name || n.WGPublicKey == "" || n.WGEndpoint == "" || (node.Edge() && n.Edge()) {
			continue
		}
		sub, ip := NodeSubnet(c.ClusterCIDR, n.SubnetIndex)
		p := types.Peer{Name: n.Name, PublicKey: n.WGPublicKey, Endpoint: n.WGEndpoint, Subnet: sub.String(), MeshIP: ip.String()}
		switch {
		case node.Edge():
			p.Endpoint, p.Routes = "", via[n.Name]
		case n.Edge():
			p.Role = types.RoleEdge
		default:
			p.AgentAddr = c.agentAddr(n)
		}
		peers = append(peers, p)
	}
	return peers
}

// edgeAccess is what the cluster's edges may reach on or through node: the
// web ports of the instances it runs for routed apps, the external apps'
// targets it forwards to and, on the control node, the API. Nil without
// edges.
func (c *Controller) edgeAccess(ctx context.Context, node store.Node, nodes []store.Node, apps []types.App,
	current map[string]*store.Release, insts []store.Instance, ext map[string]External) (*types.EdgeAccess, error) {
	acc := &types.EdgeAccess{Control: node.Role == store.RoleControl}
	for _, n := range nodes {
		if n.Edge() {
			sub, _ := NodeSubnet(c.ClusterCIDR, n.SubnetIndex)
			acc.Edges = append(acc.Edges, sub.String())
		}
	}
	if len(acc.Edges) == 0 {
		return nil, nil
	}
	px, _ := props.Lookup("proxy")
	globalProxy, err := c.Store.Properties(ctx, "", "proxy")
	if err != nil {
		return nil, err
	}
	routed := map[string]bool{}
	for _, a := range apps {
		appProxy, err := c.Store.Properties(ctx, a.Name, "proxy")
		if err != nil {
			return nil, err
		}
		routed[a.Name] = props.Compute(px, appProxy, globalProxy)["enabled"] == "true"
	}
	allow := map[string]bool{}
	for _, in := range insts {
		cur := current[in.App]
		// Every wanted web instance, healthy or not yet, so the firewall
		// is ready before an edge routes to it.
		if in.Node == node.Name && routed[in.App] && cur != nil && in.ProcessType == cur.WebProcess() &&
			in.Desired == store.DesiredRunning && in.IP != "" && in.Port > 0 {
			allow[fmt.Sprintf("%s:%d", in.IP, in.Port)] = true
		}
	}
	targets := map[string]bool{}
	for _, e := range ext {
		if e.Via == node.Name && routed[e.App] {
			allow[e.Target.String()] = true
			targets[e.Target.Addr().String()] = true
		}
	}
	acc.Allow, acc.Targets = sortedKeys(allow), sortedKeys(targets)
	return acc, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// maxRestarts turns the app's restart policy into a restart budget: -1 for
// always, 0 for never.
func (c *Controller) maxRestarts(ctx context.Context, app string) int {
	global, _ := c.Store.Properties(ctx, "", "ps")
	appProps, _ := c.Store.Properties(ctx, app, "ps")
	p, _ := props.Lookup("ps")
	return restartBudget(props.Compute(p, appProps, global)["restart-policy"])
}

// restartBudget turns a restart policy into a number of restarts: -1 for
// always, 0 for never.
func restartBudget(policy string) int {
	switch {
	case policy == "no":
		return 0
	case policy == "always" || policy == "unless-stopped" || policy == "on-failure":
		return -1
	}
	if n, ok := strings.CutPrefix(policy, "on-failure:"); ok {
		if max, err := strconv.Atoi(n); err == nil {
			return max
		}
	}
	return 10
}

// Env is the process environment: the image's, then config vars, then what
// Jokku sets. A compose service gets the compose file's environment instead
// of the config vars, as with docker compose (config vars fill in ${VAR}
// there).
func Env(rel *store.Release, in store.Instance) []string {
	var env []string
	if svc, ok := rel.Services[in.ProcessType]; ok {
		env = append(append(env, svc.Image.Env...), svc.Env...)
	} else {
		env = append(env, rel.Image.Env...)
		keys := make([]string, 0, len(rel.ConfigVars))
		for k := range rel.ConfigVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+rel.ConfigVars[k])
		}
	}
	if in.Port > 0 {
		env = append(env, "PORT="+strconv.Itoa(in.Port))
	}
	return append(env,
		"DYNO="+in.Name(),
		"JOKKU_APP_NAME="+in.App,
		"JOKKU_PROCESS_TYPE="+in.ProcessType,
	)
}

// Hostname is <app>-<type>-<n>, a valid DNS label.
func Hostname(in store.Instance) string {
	h := strings.ToLower(fmt.Sprintf("%s-%s-%d", in.App, in.ProcessType, in.Index))
	h = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, h)
	if len(h) > 63 {
		h = h[:63]
	}
	return strings.Trim(h, "-")
}
