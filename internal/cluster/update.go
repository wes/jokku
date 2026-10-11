package cluster

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/version"
)

// Updating the cluster
//
// "jokku update" on the control node updates it, then asks it to bring the
// other nodes to its version (UpdateNodes). The controller does that one node
// at a time: it puts an update order in the node's desired state, and waits
// for the node to report the new version (it worked) or a failure for that
// order (update.sh failed and put the old version back), for at most
// UpdateTimeout. A failure stops the update until the next "jokku update",
// which tries that node again: a release that breaks one node isn't rolled
// onto the rest. Nodes that are down take their turn when they are back;
// those whose jokku is too old to update itself are left to the user.

// UpdatePlugin keeps the cluster's update in the global properties: the
// version the nodes are going to ("version"), and the node it stopped at
// and why ("failed", "error"). Kept in the store, it carries on after the
// control node restarts, and nodes that were down catch up when they return.
const UpdatePlugin = "cluster-update"

// UpdateTimeout is how long a node has to update and report back. (A
// variable so tests can shorten it.)
var UpdateTimeout = 10 * time.Minute

// rollout is the node updating now. It is only in memory: after a restart
// the controller orders that node again, and the node goes on with the update
// it has under way rather than starting another.
type rollout struct {
	mu      sync.Mutex
	node    string
	order   types.UpdateOrder
	since   time.Time
	failure string // what the node reported about order
	// turnedAway are nodes speaking another protocol, which can update
	// themselves: they are told to right away.
	turnedAway map[string]bool
}

// UpdateNodes brings the other nodes to this control node's version, one at
// a time, starting again with the node a failed update stopped at.
func (c *Controller) UpdateNodes(ctx context.Context) error {
	if !version.Release(c.Version) {
		return fmt.Errorf("this control node runs a development build (%s): update the other nodes by hand", c.Version)
	}
	if err := c.Store.SetProperties(ctx, "", UpdatePlugin, map[string]string{"version": c.Version, "failed": "", "error": ""}); err != nil {
		return err
	}
	return c.tickUpdate(ctx, time.Now())
}

// tickUpdate checks on the node updating now, and when it is done starts
// the next.
func (c *Controller) tickUpdate(ctx context.Context, now time.Time) error {
	p, err := c.Store.Properties(ctx, "", UpdatePlugin)
	if err != nil || p["version"] == "" {
		return err
	}
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return err
	}
	r := &c.rollout
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.node != "" {
		i := slices.IndexFunc(nodes, func(n store.Node) bool { return n.Name == r.node })
		switch {
		case i < 0 || nodes[i].Removed():
		case nodes[i].Version == r.order.Version:
			c.Store.AddEvent(ctx, "node", "", r.node, "%s is updated to %s", r.node, r.order.Version)
		case r.failure != "" || now.Sub(r.since) > UpdateTimeout:
			why := r.failure
			if why == "" {
				why = fmt.Sprintf("it did not come back on %s within %s", r.order.Version, UpdateTimeout)
			}
			c.Store.AddEvent(ctx, "node", "", r.node, "updating %s to %s failed, so the other nodes wait: %s", r.node, r.order.Version, why)
			if err := c.Store.SetProperties(ctx, "", UpdatePlugin, map[string]string{"failed": r.node, "error": why}); err != nil {
				return err
			}
			p["failed"] = r.node
		default:
			return nil // still updating
		}
		r.node = ""
		c.Changed()
	}
	target := p["version"]
	if failed := p["failed"]; failed != "" {
		// Updated by hand, or gone: the update goes on.
		i := slices.IndexFunc(nodes, func(n store.Node) bool { return n.Name == failed })
		if i >= 0 && !nodes[i].Removed() && version.Older(nodes[i].Version, target) {
			return nil
		}
		if err := c.Store.SetProperties(ctx, "", UpdatePlugin, map[string]string{"failed": "", "error": ""}); err != nil {
			return err
		}
	}
	for _, n := range updateOrder(nodes) {
		if n.Name == c.Self || n.Removed() || !n.Ready(now) || !n.Has(types.FeatureUpdate) || !version.Older(n.Version, target) {
			continue
		}
		r.node, r.since, r.failure = n.Name, now, ""
		r.order = types.UpdateOrder{Version: target, Attempt: strconv.FormatInt(now.UnixNano(), 36)}
		c.Store.AddEvent(ctx, "node", "", n.Name, "updating %s from %s to %s", n.Name, n.Version, target)
		c.Changed()
		return nil
	}
	return nil
}

// updateOrder is the order nodes update in: workers, then edges, by name.
func updateOrder(nodes []store.Node) []store.Node {
	out := slices.Clone(nodes)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Edge() != out[j].Edge() {
			return !out[i].Edge()
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// updateReported hears a node's report: whether the update it was told to
// make failed.
func (c *Controller) updateReported(node string, st *types.NodeStatus) {
	r := &c.rollout
	r.mu.Lock()
	defer r.mu.Unlock()
	if f := st.UpdateFailed; f != nil && node == r.node && f.Attempt == r.order.Attempt && r.failure == "" {
		r.failure = f.Error
	}
}

// turnAway notes a node speaking another protocol. Its reports are refused,
// but its state polls are not, so one that can update itself is told to.
// It reports whether it was.
func (c *Controller) turnAway(node string, st *types.NodeStatus) bool {
	if !slices.Contains(st.Features, types.FeatureUpdate) || !version.Older(st.Version, c.Version) {
		return false
	}
	r := &c.rollout
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.turnedAway[node] {
		if r.turnedAway == nil {
			r.turnedAway = map[string]bool{}
		}
		r.turnedAway[node] = true
		c.Changed()
	}
	return true
}

// updateFor is the update order in node's desired state, if any.
func (c *Controller) updateFor(node string) *types.UpdateOrder {
	r := &c.rollout
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.node == node:
		o := r.order
		return &o
	case r.turnedAway[node]:
		return &types.UpdateOrder{Version: c.Version, Attempt: "protocol-" + c.Version}
	}
	return nil
}

// updating reports whether node is in the middle of an update, and so may
// be quiet for a while without its instances moving.
func (c *Controller) updating(node string, now time.Time) bool {
	r := &c.rollout
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.node == node && now.Sub(r.since) <= UpdateTimeout
}

// nodeUpdates is each node's part in the cluster's update, for the status:
// nothing for nodes already on its version (or newer, or a development
// build).
func (c *Controller) nodeUpdates(ctx context.Context, nodes []store.Node) (map[string]*types.NodeUpdate, error) {
	p, err := c.Store.Properties(ctx, "", UpdatePlugin)
	if err != nil || p["version"] == "" {
		return nil, err
	}
	r := &c.rollout
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]*types.NodeUpdate{}
	for _, n := range nodes {
		if n.Name == c.Self || !version.Older(n.Version, p["version"]) {
			continue
		}
		u := &types.NodeUpdate{To: p["version"], State: types.UpdateWaiting}
		switch {
		case n.Name == r.node:
			since := r.since
			u.State, u.Since = types.UpdateRunning, &since
		case n.Name == p["failed"]:
			u.State, u.Error = types.UpdateFailed, p["error"]
		case !n.Has(types.FeatureUpdate):
			u.State = types.UpdateByHand
		case p["failed"] != "":
			u.State = types.UpdateOnHold
		}
		out[n.Name] = u
	}
	return out, nil
}
