package cluster

import (
	"context"
	"time"

	"github.com/wes/jokku/internal/store"
)

// DrainGrace is how long a moved instance keeps running after its
// replacement takes traffic, to finish in-flight requests.
var DrainGrace = 10 * time.Second

// RetryAfter spaces out attempts to replace the same instance.
var RetryAfter = 30 * time.Second

func (c *Controller) tick(ctx context.Context) error {
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	byName := map[string]store.Node{}
	changed := false
	for _, n := range nodes {
		byName[n.Name] = n
		ready := n.Ready(now)
		if was, seen := c.lastReady[n.Name]; seen && was != ready {
			changed = true
			if ready {
				c.Store.AddEvent(ctx, "node", "", n.Name, "%s is back", n.Name)
			} else {
				c.Store.AddEvent(ctx, "node", "", n.Name, "%s stopped reporting; its instances move elsewhere if it is not back within %s", n.Name, RescheduleAfter)
			}
		}
		c.lastReady[n.Name] = ready
	}

	insts, err := c.Store.Instances(ctx, "")
	if err != nil {
		return err
	}
	byID := map[string]store.Instance{}
	replacing := map[string]bool{} // instance ID -> a wanted replacement exists
	for _, in := range insts {
		byID[in.ID] = in
		if in.Replaces != "" && in.Desired == store.DesiredRunning {
			replacing[in.Replaces] = true
		}
	}

	for _, in := range insts {
		// A replacement that passed checks takes over: the original stops
		// (after a grace period, if it is still serving).
		if in.Replaces != "" && in.Desired == store.DesiredRunning && in.State == store.StateHealthy {
			if orig, ok := byID[in.Replaces]; ok && orig.Desired == store.DesiredRunning {
				at := now.Add(DrainGrace)
				if n, ok := byName[orig.Node]; !ok || !n.Ready(now) {
					at = now
				}
				if err := c.Store.StopInstances(ctx, []string{orig.ID}, &at); err != nil {
					return err
				}
				changed = true
			}
		}
		// A replacement that failed is given up; another is tried later.
		if in.Replaces != "" && in.Desired == store.DesiredRunning && in.State == store.StateFailed {
			c.Store.AddEvent(ctx, "instance", in.App, in.Node, "%s failed to start on %s while moving", in.Name(), in.Node)
			if err := c.Store.StopInstances(ctx, []string{in.ID}, nil); err != nil {
				return err
			}
			changed = true
		}

		// Skip instances already being replaced, and replacements whose
		// original still serves (that move is in progress).
		if in.Desired != store.DesiredRunning || replacing[in.ID] ||
			in.Replaces != "" && byID[in.Replaces].Desired == store.DesiredRunning {
			continue
		}
		n, exists := byName[in.Node]
		reason := ""
		switch {
		case !exists:
			reason = "its node was removed"
		case !n.Ready(now) && now.Sub(n.LastSeen) > RescheduleAfter:
			reason = in.Node + " is down"
		case n.Draining:
			reason = in.Node + " is draining"
		default:
			continue
		}
		if last, tried := c.attempts[in.ID]; tried && now.Sub(last) < RetryAfter {
			continue
		}
		c.attempts[in.ID] = now
		if err := c.replace(ctx, in, reason, !exists || !n.Ready(now)); err != nil {
			c.Store.AddEvent(ctx, "instance", in.App, in.Node, "cannot move %s off %s: %v", in.Name(), in.Node, err)
			continue
		}
		changed = true
	}

	// Rows for stopped instances whose node is gone, or long dead, will never
	// be confirmed removed by that node; drop them. (A node that comes back
	// stops anything it is no longer told to run.)
	for _, in := range insts {
		if in.Desired != store.DesiredStopped || held(in, now) {
			continue
		}
		n, exists := byName[in.Node]
		if !exists || now.Sub(n.LastSeen) > 10*time.Minute {
			if err := c.Store.DeleteInstance(ctx, in.ID); err != nil {
				return err
			}
			delete(c.attempts, in.ID)
			changed = true
		}
	}
	if changed {
		c.Changed()
	}
	return nil
}

// replace starts a copy of in on another node. When in's node is gone, in is
// stopped right away (it isn't serving); otherwise it keeps serving until the
// copy passes checks.
func (c *Controller) replace(ctx context.Context, in store.Instance, reason string, nodeGone bool) error {
	node, subnet, err := c.Place(ctx, Placement{
		App: in.App, ProcessType: in.ProcessType, CPUs: in.CPUs, MemoryMB: in.MemoryMB, Exclude: in.Node,
	})
	if err != nil {
		return err
	}
	repl := store.Instance{
		App: in.App, ReleaseID: in.ReleaseID, ProcessType: in.ProcessType, Index: in.Index, Node: node,
		Port: in.Port, CPUs: in.CPUs, MemoryMB: in.MemoryMB, Desired: store.DesiredRunning, Replaces: in.ID,
	}
	if err := c.Store.CreateInstance(ctx, &repl, subnet); err != nil {
		return err
	}
	c.Store.AddEvent(ctx, "instance", in.App, node, "moving %s from %s to %s: %s", in.Name(), in.Node, node, reason)
	if nodeGone {
		return c.Store.StopInstances(ctx, []string{in.ID}, nil)
	}
	return nil
}
