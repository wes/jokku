package cluster

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// A local volume is a disk on one node, and the instance it is mounted in
// runs there. Moving it (a drain, storage:move) goes:
//
//  1. The volume is marked moving to the new node, and a replacement
//     instance is placed there. It waits (syncing) for the disk.
//  2. The new node copies the disk from the owner while the app keeps
//     running, until a pass changes little, and reports synced.
//  3. The instance on the old node is stopped. The owner refuses the final
//     pass until the VM has let go of the disk, then sends what changed and
//     hands the disk over (renamed .moved, so nothing there can use it).
//  4. The new node reports received; the move is committed. The new node
//     owns the disk and starts the replacement. The old copy is deleted once
//     the new node reports the disk ready.
//
// If the new node goes down or is removed before the commit, the move is
// called off: the old node takes its disk back and the instance is started
// there again. A node going down does not move its volumes' instances at all:
// their data is there, so they wait for it to come back.

// volumeSpecs lists the volume disks node should hold, receive or delete.
func (c *Controller) volumeSpecs(ctx context.Context, node string, nodes []store.Node) ([]types.VolumeSpec, error) {
	vols, err := c.Store.Volumes(ctx, "")
	if err != nil {
		return nil, err
	}
	byName := map[string]store.Node{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	var out []types.VolumeSpec
	for _, v := range vols {
		if v.Type != types.VolumeLocal {
			continue
		}
		spec := types.VolumeSpec{ID: v.ID, App: v.App, Name: v.Name, SizeMB: v.SizeMB}
		switch {
		case v.State == store.VolumeDestroying:
			if v.Node != node {
				continue
			}
			spec.Role = types.VolumeDestroy
		case v.Node == node:
			spec.Role, spec.Create, spec.Token = types.VolumeOwner, v.State == store.VolumeNew, v.MoveToken
			if v.Restore != "" && !v.Moving() {
				if spec.Restore, err = c.restoreSpec(ctx, v); err != nil {
					c.Log.Warn("cannot restore volume", "app", v.App, "volume", v.Name, "err", err)
				}
			}
		case v.MovingTo == node && v.Restore != "":
			r, err := c.restoreSpec(ctx, v)
			if err != nil {
				c.Log.Warn("cannot restore volume", "app", v.App, "volume", v.Name, "err", err)
				continue
			}
			spec.Role, spec.Token, spec.Restore = types.VolumeIncoming, v.MoveToken, r
		case v.MovingTo == node:
			src, ok := byName[v.Node]
			if !ok {
				continue
			}
			spec.Role, spec.Token, spec.From = types.VolumeIncoming, v.MoveToken, c.agentAddr(src)
		case v.PreviousNode == node:
			spec.Role = types.VolumePrevious
		default:
			continue
		}
		out = append(out, spec)
	}
	return out, nil
}

// tickVolumes advances moves on what the nodes reported, moves volumes no
// instance uses off draining nodes, and forgets destroyed volumes whose node
// is gone.
func (c *Controller) tickVolumes(ctx context.Context) (bool, error) {
	c.volMu.Lock()
	defer c.volMu.Unlock()
	vols, err := c.Store.Volumes(ctx, "")
	if err != nil || len(vols) == 0 {
		return false, err
	}
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return false, err
	}
	insts, err := c.Store.Instances(ctx, "")
	if err != nil {
		return false, err
	}
	now := time.Now()
	byName := map[string]store.Node{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	byID := map[string]store.Instance{}
	for _, in := range insts {
		byID[in.ID] = in
	}
	handled := map[string]bool{} // instances already stopped or restored this pass
	changed := false
	for _, v := range vols {
		src, srcOK := byName[v.Node]
		switch {
		case v.State == store.VolumeDestroying:
			if !srcOK {
				if err := c.Store.DeleteVolume(ctx, v.ID); err != nil {
					return changed, err
				}
				changed = true
			}

		case v.Restore != "":
			ch, err := c.tickRestore(ctx, v, insts, byID, byName, handled, now)
			if err != nil {
				return changed, err
			}
			changed = changed || ch

		case v.Moving():
			dest, destOK := byName[v.MovingTo]
			why := ""
			switch {
			case !destOK:
				why = v.MovingTo + " was removed"
			case !dest.Ready(now):
				why = v.MovingTo + " is down"
			case !srcOK:
				why = v.Node + " was removed"
			}
			if why != "" {
				if err := c.abortMove(ctx, v, why, insts, byID, byName, handled); err != nil {
					return changed, err
				}
				changed = true
				continue
			}
			switch v.Transfer {
			case types.VolumeReceived:
				if err := c.Store.CommitVolumeMove(ctx, v.ID); err != nil {
					return changed, err
				}
				c.Store.AddEvent(ctx, "volume", v.App, v.MovingTo, "volume %s moved from %s to %s", v.Name, v.Node, v.MovingTo)
				changed = true
			case types.VolumeSynced:
				// The copy is nearly there: stop the instance using the disk so
				// the final pass gets all of it.
				for _, in := range insts {
					if in.Node != v.Node || in.Desired != store.DesiredRunning || !usesVolume(in, v.ID) || handled[in.ID] {
						continue
					}
					handled[in.ID] = true
					if err := c.Store.StopInstances(ctx, []string{in.ID}, nil); err != nil {
						return changed, err
					}
					c.Store.AddEvent(ctx, "instance", in.App, in.Node, "stopping %s on %s for the final copy of volume %s", in.Name(), in.Node, v.Name)
					changed = true
				}
			}

		case v.Type == types.VolumeLocal && srcOK && src.Draining && src.Ready(now) && !inUse(insts, v.ID):
			// Instances leaving a draining node bring their volumes along
			// (see tick); this one has none, but must go too.
			dest, _, err := c.Place(ctx, Placement{App: v.App, Exclude: v.Node, Volumes: true, DiskMB: diskNeeded([]store.Volume{v})})
			if err != nil {
				if last, tried := c.volAttempts[v.ID]; !tried || now.Sub(last) >= RetryAfter {
					c.volAttempts[v.ID] = now
					c.Store.AddEvent(ctx, "volume", v.App, v.Node, "cannot move volume %s off %s: %v", v.Name, v.Node, err)
				}
				continue
			}
			if err := c.startMove(ctx, []store.Volume{v}, nil, dest, netip.Prefix{}, v.Node+" is draining"); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	return changed, nil
}

// abortMove calls off v's move: the replacement waiting for it is stopped,
// and the instance the move stopped is started again where the disk is.
func (c *Controller) abortMove(ctx context.Context, v store.Volume, why string, insts []store.Instance,
	byID map[string]store.Instance, byName map[string]store.Node, handled map[string]bool) error {
	if err := c.Store.AbortVolumeMove(ctx, v.ID, why); err != nil {
		return err
	}
	c.Store.AddEvent(ctx, "volume", v.App, v.Node, "moving volume %s to %s was called off: %s", v.Name, v.MovingTo, why)
	src, srcOK := byName[v.Node]
	for _, repl := range insts {
		if repl.Node != v.MovingTo || repl.Desired != store.DesiredRunning || !usesVolume(repl, v.ID) || handled[repl.ID] {
			continue
		}
		handled[repl.ID] = true
		if err := c.Store.StopInstances(ctx, []string{repl.ID}, nil); err != nil {
			return err
		}
		if orig, ok := byID[repl.Replaces]; (ok && orig.Desired == store.DesiredRunning) || !srcOK {
			continue // it never stopped, or there is nowhere to start it
		}
		subnet, _ := NodeSubnet(c.ClusterCIDR, src.SubnetIndex)
		back := repl
		back.Node, back.Replaces = v.Node, ""
		if err := c.Store.CreateInstance(ctx, &back, subnet); err != nil {
			return err
		}
		c.Store.AddEvent(ctx, "instance", back.App, back.Node, "starting %s on %s again", back.Name(), back.Node)
	}
	return nil
}

// startMove copies vols to dest. With in, the instance using them follows:
// a replacement waits on dest until the disks have arrived.
func (c *Controller) startMove(ctx context.Context, vols []store.Volume, in *store.Instance, dest string, subnet netip.Prefix, reason string) error {
	b := make([]byte, 24)
	rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	var names []string
	for _, v := range vols {
		if err := c.Store.StartVolumeMove(ctx, v.ID, dest, token); err != nil {
			return fmt.Errorf("volume %s: %w", v.Name, err)
		}
		names = append(names, v.Name)
	}
	what := "volume " + strings.Join(names, ", ")
	if len(names) > 1 {
		what = "volumes " + strings.Join(names, ", ")
	}
	from := vols[0].Node
	if in != nil {
		repl := store.Instance{
			App: in.App, ReleaseID: in.ReleaseID, ProcessType: in.ProcessType, Index: in.Index, Node: dest,
			Port: in.Port, CPUs: in.CPUs, MemoryMB: in.MemoryMB, Desired: store.DesiredRunning, Replaces: in.ID,
			Volumes: in.Volumes,
		}
		if err := c.Store.CreateInstance(ctx, &repl, subnet); err != nil {
			return err
		}
		what = in.Name() + " and its " + what
	}
	c.Store.AddEvent(ctx, "volume", vols[0].App, dest, "moving %s from %s to %s: %s", what, from, dest, reason)
	return nil
}

// moveInstance moves an instance and its volumes off its node (a drain).
func (c *Controller) moveInstance(ctx context.Context, in store.Instance, vols []store.Volume, reason string) error {
	for _, v := range vols {
		if v.Moving() {
			return nil // already on its way
		}
	}
	dest, subnet, err := c.Place(ctx, Placement{
		App: in.App, ProcessType: in.ProcessType, CPUs: in.CPUs, MemoryMB: in.MemoryMB, Exclude: in.Node,
		Volumes: true, DiskMB: diskNeeded(vols),
	})
	if err != nil {
		return err
	}
	return c.startMove(ctx, vols, &in, dest, subnet, reason)
}

// MoveVolume moves an app's volume to node (storage:move), with the instance
// using it and that instance's other volumes.
func (c *Controller) MoveVolume(ctx context.Context, app, name, node string) error {
	c.volMu.Lock()
	defer c.volMu.Unlock()
	v, err := c.Store.Volume(ctx, app, name)
	if err != nil {
		return err
	}
	target, err := c.Store.Node(ctx, node)
	if err != nil {
		return err
	}
	switch {
	case v.Type != types.VolumeLocal:
		return fmt.Errorf("only local volumes live on a node")
	case v.Moving():
		return fmt.Errorf("volume %s is already moving to %s", name, v.MovingTo)
	case v.Node == node:
		return fmt.Errorf("volume %s is already on %s", name, node)
	case target.Draining:
		return fmt.Errorf("%s is draining", node)
	case v.Node == "":
		// Never used, so there is no disk yet: make it there.
		return c.Store.PlaceVolume(ctx, v.ID, node)
	}
	src, err := c.Store.Node(ctx, v.Node)
	if err != nil {
		return fmt.Errorf("volume %s was on %s, which is no longer in the cluster", name, v.Node)
	}
	if !src.Ready(time.Now()) {
		return fmt.Errorf("volume %s is on %s, which is down: its disk can be copied once it is back", name, v.Node)
	}
	insts, err := c.Store.Instances(ctx, app)
	if err != nil {
		return err
	}
	vols, err := c.Store.Volumes(ctx, app)
	if err != nil {
		return err
	}
	for _, in := range insts {
		if in.Desired != store.DesiredRunning || !usesVolume(in, v.ID) {
			continue
		}
		mine := pinnedVolumes(in, vols)
		for _, o := range mine {
			if o.Moving() {
				return fmt.Errorf("volume %s, which %s also mounts, is already moving to %s", o.Name, in.Name(), o.MovingTo)
			}
		}
		_, subnet, err := c.Place(ctx, Placement{
			App: app, ProcessType: in.ProcessType, CPUs: in.CPUs, MemoryMB: in.MemoryMB,
			Pin: node, PinReason: "the move goes to " + node, Volumes: true, DiskMB: diskNeeded(mine),
		})
		if err != nil {
			return err
		}
		if err := c.startMove(ctx, mine, &in, node, subnet, "storage:move"); err != nil {
			return err
		}
		c.Changed()
		return nil
	}
	if _, _, err := c.Place(ctx, Placement{App: app, Pin: node, PinReason: "the move goes to " + node, Volumes: true, DiskMB: diskNeeded([]store.Volume{*v})}); err != nil {
		return err
	}
	if err := c.startMove(ctx, []store.Volume{*v}, nil, node, netip.Prefix{}, "storage:move"); err != nil {
		return err
	}
	c.Changed()
	return nil
}

// pinnedVolumes are the local volumes of in whose disk is on in's node.
func pinnedVolumes(in store.Instance, vols []store.Volume) []store.Volume {
	var out []store.Volume
	for _, m := range in.Volumes {
		for _, v := range vols {
			if v.ID == m.ID && v.Type == types.VolumeLocal && v.Node == in.Node && v.State != store.VolumeDestroying {
				out = append(out, v)
			}
		}
	}
	return out
}

func usesVolume(in store.Instance, id string) bool {
	for _, m := range in.Volumes {
		if m.ID == id {
			return true
		}
	}
	return false
}

// inUse reports whether a wanted (or retiring) instance mounts the volume.
func inUse(insts []store.Instance, id string) bool {
	now := time.Now()
	for _, in := range insts {
		if held(in, now) && usesVolume(in, id) {
			return true
		}
	}
	return false
}

// diskNeeded is the free space to ask of a node receiving vols.
func diskNeeded(vols []store.Volume) int {
	total := 0
	for _, v := range vols {
		total += v.UsedMB
	}
	return total
}

// warnPinned records, once per reason, that an instance cannot leave its
// node because its volumes are there.
func (c *Controller) warnPinned(ctx context.Context, in store.Instance, vols []store.Volume, reason string) {
	if c.pinned[in.ID] == reason {
		return
	}
	c.pinned[in.ID] = reason
	names := make([]string, len(vols))
	for i, v := range vols {
		names[i] = v.Name
	}
	c.Store.AddEvent(ctx, "instance", in.App, in.Node, "%s stays on %s (%s): its volume %s is there",
		in.Name(), in.Node, reason, strings.Join(names, ", "))
}
