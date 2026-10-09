package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
		Version:   c.Version,
		Node:      types.NodeIdentity{Name: node.Name, Subnet: subnet.String(), MeshIP: meshIP.String(), ClusterCIDR: c.ClusterCIDR.String()},
		Peers:     []types.Peer{},
		Instances: []types.InstanceSpec{},
	}
	if node.WGPublicKey != "" {
		for _, n := range nodes {
			if n.Name == node.Name || n.WGPublicKey == "" || n.WGEndpoint == "" {
				continue
			}
			sub, ip := NodeSubnet(c.ClusterCIDR, n.SubnetIndex)
			st.Peers = append(st.Peers, types.Peer{
				Name: n.Name, PublicKey: n.WGPublicKey, Endpoint: n.WGEndpoint, Subnet: sub.String(), MeshIP: ip.String(),
				AgentAddr: c.agentAddr(n),
			})
		}
	}

	apps, err := c.Store.Apps(ctx)
	if err != nil {
		return nil, err
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
	st.DNS = dnsRecords(apps, current, insts, nodes, now)
	if node.Ingress {
		if st.Proxy, err = c.proxyState(ctx, apps, current, insts, nodes, now); err != nil {
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
// of its current release, on whichever nodes they run.
func (c *Controller) proxyState(ctx context.Context, apps []types.App, current map[string]*store.Release, insts []store.Instance, nodes []store.Node, now time.Time) (*types.ProxyState, error) {
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
	ps := &types.ProxyState{Routes: []types.ProxyRoute{}, Email: globalLE["email"]}
	for _, app := range apps {
		if app.CurrentRelease == 0 {
			continue
		}
		appProxy, _ := c.Store.Properties(ctx, app.Name, "proxy")
		if props.Compute(px, appProxy, globalProxy)["enabled"] != "true" {
			continue
		}
		cur := current[app.Name]
		if cur == nil {
			continue
		}
		domains, err := c.Store.Domains(ctx, app.Name)
		if err != nil {
			return nil, err
		}
		upstreams := []string{}
		if !app.Stopped {
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
		plain := types.ProxyRoute{App: app.Name, Upstreams: upstreams, Hosts: []string{}}
		secure := types.ProxyRoute{App: app.Name, Upstreams: upstreams, Hosts: []string{}, TLS: true}
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
