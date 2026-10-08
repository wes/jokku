package cluster

import (
	"context"
	"fmt"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// Report records what a node's agent observed: it is alive, its metrics, and
// each instance's state. Retired instances the node no longer reports are
// deleted, since the node has removed them.
func (c *Controller) Report(ctx context.Context, node string, st *types.NodeStatus) error {
	if st.Protocol != types.ProtocolVersion {
		return fmt.Errorf("node %s speaks protocol %d, this control node speaks %d: update it with sudo jokku update",
			node, st.Protocol, types.ProtocolVersion)
	}
	if err := c.Store.NodeReported(ctx, node, st.Version, st.CanRun, st.Features, st.Metrics); err != nil {
		return err
	}
	volumesChanged := false
	for _, vs := range st.Volumes {
		changed, err := c.Store.ReportVolume(ctx, node, vs)
		if err != nil {
			return err
		}
		volumesChanged = volumesChanged || changed
		if changed && vs.State == types.VolumeMissing {
			if v, err := c.Store.VolumeByID(ctx, vs.ID); err == nil {
				c.Store.AddEvent(ctx, "volume", v.App, node, "the disk of volume %s is missing on %s; its instance cannot start", v.Name, node)
			}
		}
	}
	if volumesChanged {
		// Moves advance on what nodes report; don't wait for the next tick.
		if _, err := c.tickVolumes(ctx); err != nil {
			return err
		}
	}
	insts, err := c.Store.Instances(ctx, "")
	if err != nil {
		return err
	}
	mine := map[string]store.Instance{}
	for _, in := range insts {
		if in.Node == node {
			mine[in.ID] = in
		}
	}
	reported := map[string]bool{}
	changed := false
	for _, is := range st.Instances {
		reported[is.ID] = true
		in, ok := mine[is.ID]
		if !ok {
			continue
		}
		before, err := c.Store.ReportInstance(ctx, node, is)
		if err != nil {
			return err
		}
		if before == is.State {
			continue
		}
		changed = true
		switch is.State {
		case store.StateCrashed:
			c.Store.AddEvent(ctx, "instance", in.App, node, "%s crashed; restarting it (restart %d)", in.Name(), is.Restarts+1)
		case store.StateFailed:
			if is.HealthyOnce {
				c.Store.AddEvent(ctx, "instance", in.App, node, "%s keeps crashing; not restarting it (restart policy)", in.Name())
			} else {
				c.Store.AddEvent(ctx, "instance", in.App, node, "%s exited before passing checks", in.Name())
			}
		case store.StateHealthy:
			if before == store.StateCrashed {
				c.Store.AddEvent(ctx, "instance", in.App, node, "%s recovered", in.Name())
			}
		}
	}
	now := time.Now()
	for id, in := range mine {
		if !reported[id] && !held(in, now) && in.Desired == store.DesiredStopped {
			if err := c.Store.DeleteInstance(ctx, id); err != nil {
				return err
			}
			changed = true
		}
	}
	if changed || volumesChanged {
		c.Changed()
	}
	return nil
}
