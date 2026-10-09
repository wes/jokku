package cluster

import (
	"context"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// NodeInfo is the API form of a node.
func (c *Controller) NodeInfo(n store.Node, now time.Time) types.Node {
	subnet, meshIP := NodeSubnet(c.ClusterCIDR, n.SubnetIndex)
	return types.Node{
		Name: n.Name, Role: n.Role, Status: n.Status(now), Address: n.Address,
		MeshIP: meshIP.String(), Subnet: subnet.String(), Arch: n.Arch, CPUs: n.CPUs, MemoryMB: n.MemoryMB,
		Schedulable: n.Schedulable, Ingress: n.Ingress, LastSeen: n.LastSeen, CreatedAt: n.CreatedAt,
	}
}

// Status is the whole cluster at a glance: nodes, apps, instances and recent
// events.
func (c *Controller) Status(ctx context.Context) (*types.ClusterStatus, error) {
	now := time.Now()
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	apps, err := c.Store.Apps(ctx)
	if err != nil {
		return nil, err
	}
	insts, err := c.Store.Instances(ctx, "")
	if err != nil {
		return nil, err
	}
	events, err := c.Store.Events(ctx, "", 100)
	if err != nil {
		return nil, err
	}
	st := &types.ClusterStatus{Version: c.Version, Control: c.Self, At: now, Events: events,
		Nodes: []types.NodeView{}, Apps: []types.AppView{}, Instances: []types.InstanceView{}}

	ready := map[string]bool{}
	allocated, count := map[string]int{}, map[string]int{}
	for _, n := range nodes {
		ready[n.Name] = n.Ready(now)
	}
	current := map[string]int64{}
	for _, a := range apps {
		if cur, err := c.Store.CurrentRelease(ctx, a.Name); err == nil && cur != nil {
			current[a.Name] = cur.ID
		}
	}
	healthy, wanted := map[string]int{}, map[string]int{}
	for _, in := range insts {
		if !held(in, now) {
			continue
		}
		allocated[in.Node] += in.MemoryMB
		count[in.Node]++
		state := in.State
		if !ready[in.Node] {
			state = "unknown"
		}
		if in.Desired == store.DesiredRunning && in.ReleaseID == current[in.App] {
			wanted[in.App]++
			if state == store.StateHealthy {
				healthy[in.App]++
				st.Totals.InstancesHealthy++
			}
			st.Totals.Instances++
		}
		st.Instances = append(st.Instances, types.InstanceView{
			App: in.App, CPUPercent: in.CPUPercent, MemoryUsed: in.MemoryUsedMB,
			Instance: types.Instance{
				ID: in.ID, Name: in.Name(), Release: in.Release, Node: in.Node, IP: in.IP, Port: in.Port,
				CPUs: in.CPUs, MemoryMB: in.MemoryMB, State: state, Desired: in.Desired,
				Restarts: in.Restarts, StartedAt: in.StartedAt,
			},
		})
	}
	for _, n := range nodes {
		st.Nodes = append(st.Nodes, types.NodeView{
			Node: c.NodeInfo(n, now), Version: n.Version, CanRun: n.CanRun, Metrics: n.Metrics,
			AllocatedMB: allocated[n.Name], Instances: count[n.Name],
		})
		if ready[n.Name] {
			st.Totals.NodesReady++
		}
	}
	st.Totals.Nodes = len(nodes)
	st.Totals.Apps = len(apps)
	for _, a := range apps {
		domains, err := c.Store.Domains(ctx, a.Name)
		if err != nil {
			return nil, err
		}
		view := types.AppView{
			Name: a.Name, Kind: a.Kind, Release: a.CurrentRelease, Stopped: a.Stopped, Locked: a.Locked, Domains: domains,
			Healthy: healthy[a.Name], Wanted: wanted[a.Name], CreatedAt: a.CreatedAt,
		}
		if ds, err := c.Store.Deploys(ctx, a.Name, 1); err == nil && len(ds) > 0 {
			view.Deploy = &ds[0]
		}
		st.Apps = append(st.Apps, view)
	}
	return st, nil
}
