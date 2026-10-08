package cluster

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/wes/jokku/internal/store"
)

// Placement asks for a node for one instance.
type Placement struct {
	App         string
	ProcessType string
	CPUs        int
	MemoryMB    int
	Exclude     string // a node to avoid (the one being moved off)
}

// reserveMB is memory each node keeps for itself (the host, jokku, BuildKit
// on the control node): a tenth of its memory, at least 512 MiB.
func reserveMB(total int) int { return max(512, total/10) }

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
		reason := ""
		switch {
		case n.Name == p.Exclude:
			continue
		case !n.Ready(now):
			reason = "down"
		case n.Draining:
			reason = "draining"
		case !n.Schedulable:
			reason = "not schedulable"
		case n.CanRun != "":
			reason = n.CanRun
		case p.CPUs > n.CPUs:
			reason = fmt.Sprintf("has %d CPUs, needs %d", n.CPUs, p.CPUs)
		case free < p.MemoryMB:
			reason = fmt.Sprintf("%d MiB free, needs %d", max(free, 0), p.MemoryMB)
		}
		if reason != "" {
			why = append(why, n.Name+": "+reason)
			continue
		}
		ok = append(ok, candidate{n, free})
	}
	if len(ok) == 0 {
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
