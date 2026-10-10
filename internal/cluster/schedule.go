package cluster

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// Placement asks for a node for one instance.
type Placement struct {
	App         string
	ProcessType string
	CPUs        int
	MemoryMB    int
	Exclude     string // a node to avoid (the one being moved off)

	// Pin is the only node the instance can run on: its volumes' disks are
	// there. It may be draining or unschedulable (a drain moves it later),
	// but it must be up. PinReason says why, for errors.
	Pin, PinReason string
	// Volumes asks for nodes whose agent can hold volumes, and DiskMB for
	// free disk space to copy them to.
	Volumes bool
	DiskMB  int
}

// reserveMB is memory each node keeps for itself (the host, jokku, BuildKit
// on the control node): a tenth of its memory, at least 512 MiB.
func reserveMB(total int) int { return max(512, total/10) }

// diskReserveMB is disk space a volume moving in must leave free: a twentieth
// of the disk, at least 1 GiB.
func diskReserveMB(total int) int { return max(1024, total/20) }

// Place picks the node for an instance: among nodes that are up, schedulable,
// not draining, able to run microVMs and with the memory free, it prefers the
// one running the fewest instances of the same app and process type (spread),
// then the one with the most free memory. It returns the node and its
// instance subnet.
func (c *Controller) Place(ctx context.Context, p Placement) (string, netip.Prefix, error) {
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return "", netip.Prefix{}, err
	}
	insts, err := c.Store.Instances(ctx, "")
	if err != nil {
		return "", netip.Prefix{}, err
	}
	now := time.Now()
	allocated := map[string]int{}
	same := map[string]int{}
	for _, in := range insts {
		if !held(in, now) {
			continue
		}
		allocated[in.Node] += in.MemoryMB
		if in.App == p.App && in.ProcessType == p.ProcessType && in.Desired == store.DesiredRunning {
			same[in.Node]++
		}
	}

	type candidate struct {
		node store.Node
		free int
	}
	var ok []candidate
	var why []string
	for _, n := range nodes {
		free := n.MemoryMB - reserveMB(n.MemoryMB) - allocated[n.Name]
		diskFree := n.Metrics.DiskFreeMB - diskReserveMB(n.Metrics.DiskMB)
		reason := ""
		switch {
		case n.Name == p.Exclude || (p.Pin != "" && n.Name != p.Pin) || n.Edge():
			continue
		case !n.Ready(now):
			reason = "down"
		case n.Draining && p.Pin == "":
			reason = "draining"
		case !n.Schedulable && p.Pin == "":
			reason = "not schedulable"
		case n.CanRun != "":
			reason = n.CanRun
		case p.Volumes && !n.Has(types.FeatureVolumes):
			reason = "its jokku is too old for volumes; update it with sudo jokku update"
		case p.CPUs > n.CPUs:
			reason = fmt.Sprintf("has %d CPUs, needs %d", n.CPUs, p.CPUs)
		case free < p.MemoryMB:
			reason = fmt.Sprintf("%d MiB free, needs %d", max(free, 0), p.MemoryMB)
		case p.DiskMB > 0 && n.Metrics.DiskMB > 0 && diskFree < p.DiskMB:
			reason = fmt.Sprintf("%d MiB of disk free, needs %d", max(diskFree, 0), p.DiskMB)
		}
		if reason != "" {
			why = append(why, n.Name+": "+reason)
			continue
		}
		ok = append(ok, candidate{n, free})
	}
	if len(ok) == 0 {
		if p.Pin != "" {
			if len(why) == 0 {
				why = append(why, p.Pin+" is no longer in the cluster")
			}
			return "", netip.Prefix{}, fmt.Errorf("cannot run %s %s: %s, and %s", p.App, p.ProcessType, p.PinReason, strings.Join(why, "; "))
		}
		if len(why) == 0 {
			why = append(why, "there are no other nodes")
		}
		return "", netip.Prefix{}, fmt.Errorf("no node can run %s %s (%s)", p.App, p.ProcessType, strings.Join(why, "; "))
	}
	sort.SliceStable(ok, func(i, j int) bool {
		a, b := ok[i], ok[j]
		if same[a.node.Name] != same[b.node.Name] {
			return same[a.node.Name] < same[b.node.Name]
		}
		if a.free != b.free {
			return a.free > b.free
		}
		return a.node.SubnetIndex < b.node.SubnetIndex
	})
	best := ok[0].node
	subnet, _ := NodeSubnet(c.ClusterCIDR, best.SubnetIndex)
	return best.Name, subnet, nil
}

// CanSchedule explains why no node can run microVMs right now, or returns
// nil, so a deploy can fail before building.
func (c *Controller) CanSchedule(ctx context.Context) error {
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	var why []string
	for _, n := range nodes {
		switch {
		case n.Edge():
			continue
		case !n.Ready(now):
			why = append(why, n.Name+" is down")
		case n.CanRun != "":
			why = append(why, n.CanRun)
		case n.Schedulable && !n.Draining:
			return nil
		default:
			why = append(why, n.Name+" is not accepting instances")
		}
	}
	if len(why) == 1 {
		return fmt.Errorf("%s", why[0])
	}
	return fmt.Errorf("no node can run microVMs: %s", strings.Join(why, "; "))
}
