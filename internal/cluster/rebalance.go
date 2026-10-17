package cluster

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// Rebalance spreads instances over the nodes that take them (up,
// schedulable, not draining). Nothing moves back on its own once a drained
// node accepts instances again or a dead one returns; this is how it gets
// its share. Instances of one app and process type go to different nodes
// first, then memory is evened out. Each instance moves as a drain moves it:
// a copy starts on the new node and the original stops once that passes
// checks. One with volumes stays where their disks are unless volumes is
// set; then they move along, and it stops for a few seconds for the final
// copy. With dryRun, nothing moves: the result says what would.
func (c *Controller) Rebalance(ctx context.Context, volumes, dryRun bool) (*types.Rebalance, error) {
	c.volMu.Lock()
	defer c.volMu.Unlock()
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	insts, err := c.Store.Instances(ctx, "")
	if err != nil {
		return nil, err
	}
	vols, err := c.Store.Volumes(ctx, "")
	if err != nil {
		return nil, err
	}
	apps, err := c.Store.Apps(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	moves := planRebalance(now, nodes, insts, vols, apps, volumes)
	out := &types.Rebalance{Moves: []types.RebalanceMove{}}
	if !volumes {
		for _, m := range planRebalance(now, nodes, insts, vols, apps, true) {
			if len(m.vols) > 0 {
				out.Pinned++
			}
		}
	}
	for _, m := range moves {
		mv := types.RebalanceMove{App: m.in.App, Instance: m.in.Name(), From: m.in.Node, To: m.to, MemoryMB: m.in.MemoryMB}
		for _, v := range m.vols {
			mv.Volumes = append(mv.Volumes, v.Name)
		}
		out.Moves = append(out.Moves, mv)
	}
	if dryRun || len(moves) == 0 {
		return out, nil
	}
	byName := map[string]store.Node{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	defer c.Changed()
	for _, m := range moves {
		subnet, _ := NodeSubnet(c.ClusterCIDR, byName[m.to].SubnetIndex)
		if len(m.vols) > 0 {
			err = c.startMove(ctx, m.vols, &m.in, m.to, subnet, "nodes:rebalance")
		} else {
			err = c.moveTo(ctx, m.in, m.to, subnet, "nodes:rebalance", false)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// rebalanceMove is an instance going to another node, with the volumes whose
// disks go along.
type rebalanceMove struct {
	in   store.Instance
	vols []store.Volume
	to   string
}

// planRebalance decides what Rebalance moves. It works on where instances
// would be, moving one at a time while that helps: to a node with fewer
// instances of its app and process type, or, without crowding them, from a
// fuller node (by the share of its memory instances hold) to an emptier one
// that ends up no fuller than the source, so nothing could move back. Of
// the moves that help, instances without volumes go first, since they move
// without stopping. An instance can move again within the plan; only where
// it ends up counts. Apps with something under way (a deploy, a move, a
// restore) stay as they are.
func planRebalance(now time.Time, nodes []store.Node, insts []store.Instance, vols []store.Volume,
	apps []types.App, withVolumes bool) []rebalanceMove {
	type pnode struct {
		name      string
		cpus      int
		capMB     int // memory for instances
		usedMB    int // held by instances, with the plan's moves made
		freeMB    int // free now, less what the plan brings: what leaves frees memory only once it's gone
		diskMB    int // disk free for volumes moving in, less what the plan brings
		diskKnown bool
		volumes   bool
	}
	nodes = slices.Clone(nodes)
	slices.SortFunc(nodes, func(a, b store.Node) int { return cmp.Compare(a.SubnetIndex, b.SubnetIndex) })
	pool := map[string]*pnode{}
	var order []*pnode
	for _, n := range nodes {
		total := n.MemoryMB - reserveMB(n.MemoryMB)
		if n.Edge() || !n.Ready(now) || !n.Schedulable || n.Draining || n.CanRun != "" || total <= 0 {
			continue
		}
		p := &pnode{name: n.Name, cpus: n.CPUs, capMB: total, freeMB: total, volumes: n.Has(types.FeatureVolumes)}
		if n.Metrics.DiskMB > 0 {
			p.diskMB, p.diskKnown = n.Metrics.DiskFreeMB-diskReserveMB(n.Metrics.DiskMB), true
		}
		pool[n.Name] = p
		order = append(order, p)
	}
	if len(order) < 2 {
		return nil
	}

	resting := map[string]bool{} // apps asleep or stopped: their instances are off
	for _, a := range apps {
		resting[a.Name] = a.Stopped || a.AsleepSince != nil
	}
	byID := map[string]store.Instance{}
	for _, in := range insts {
		byID[in.ID] = in
	}
	busy := map[string]bool{}
	same := map[string]map[string]int{} // app/process type -> node -> wanted instances
	for _, in := range insts {
		if p := pool[in.Node]; p != nil && held(in, now) {
			p.usedMB += in.MemoryMB
			p.freeMB -= in.MemoryMB
		}
		if in.Desired != store.DesiredRunning {
			continue
		}
		key := in.App + "/" + in.ProcessType
		if same[key] == nil {
			same[key] = map[string]int{}
		}
		same[key][in.Node]++
		switch in.State {
		case store.StatePending, store.StatePulling, store.StateSyncing, store.StateStarting:
			busy[in.App] = true
		}
		if in.Replaces != "" && byID[in.Replaces].Desired == store.DesiredRunning {
			busy[in.App] = true
		}
	}
	for _, v := range vols {
		if v.Moving() || v.Restore != "" {
			busy[v.App] = true
		}
	}

	type cand struct {
		in     store.Instance
		vols   []store.Volume
		diskMB int
		key    string
		home   *pnode // where it runs
		at     *pnode // where the plan has it
	}
	var cands []*cand
	for _, in := range insts {
		home := pool[in.Node]
		if in.Desired != store.DesiredRunning || home == nil || busy[in.App] || (in.State != store.StateHealthy && !resting[in.App]) {
			continue
		}
		cd := &cand{in: in, key: in.App + "/" + in.ProcessType, home: home, at: home}
		if len(in.Volumes) > 0 {
			cd.vols = pinnedVolumes(in, vols)
			if !withVolumes || len(cd.vols) == 0 {
				continue
			}
			if slices.ContainsFunc(cd.vols, func(v store.Volume) bool { return v.State != store.VolumeReady }) {
				continue
			}
			cd.diskMB = diskNeeded(cd.vols)
		}
		cands = append(cands, cd)
	}
	slices.SortFunc(cands, func(a, b *cand) int {
		return cmp.Or(cmp.Compare(a.in.App, b.in.App), cmp.Compare(a.in.ProcessType, b.in.ProcessType), cmp.Compare(a.in.Index, b.in.Index))
	})

	fits := func(cd *cand, to *pnode) bool {
		switch {
		case to == cd.home:
			return true // it is there already
		case cd.in.CPUs > to.cpus || cd.in.MemoryMB > to.freeMB:
			return false
		case len(cd.vols) == 0:
			return true
		case !to.volumes || (to.diskKnown && cd.diskMB > to.diskMB):
			return false
		}
		// A node keeping an old copy of a volume can't receive it (see
		// storage:discard-old-copy).
		return !slices.ContainsFunc(cd.vols, func(v store.Volume) bool { return v.StaleNode == to.name })
	}
	put := func(cd *cand, to *pnode) {
		from, mem := cd.at, cd.in.MemoryMB
		from.usedMB -= mem
		to.usedMB += mem
		if from != cd.home {
			from.freeMB += mem
			from.diskMB += cd.diskMB
		}
		if to != cd.home {
			to.freeMB -= mem
			to.diskMB -= cd.diskMB
		}
		same[cd.key][from.name]--
		same[cd.key][to.name]++
		cd.at = to
	}
	load := func(p *pnode, deltaMB int) float64 { return float64(p.usedMB+deltaMB) / float64(p.capMB) }

	// Each step lowers how many instances share a node with their own kind,
	// or keeps that and lowers the sum of the squared loads, so this ends;
	// the bound only guards against a mistake.
	for range 4*len(cands) + 1 {
		var best *cand
		var bestTo *pnode
		var bestSpread int
		var bestGain float64
		for _, cd := range cands {
			from, mem := cd.at, cd.in.MemoryMB
			for _, to := range order {
				if to == from || !fits(cd, to) {
					continue
				}
				spread := same[cd.key][from.name] - 1 - same[cd.key][to.name]
				if spread < 0 {
					continue
				}
				fromAfter, toAfter := load(from, -mem), load(to, mem)
				gain := load(from, 0)*load(from, 0) + load(to, 0)*load(to, 0) - fromAfter*fromAfter - toAfter*toAfter
				if spread == 0 && (toAfter > fromAfter || gain <= 0) {
					continue
				}
				better := best == nil || spread > bestSpread
				if !better && spread == bestSpread {
					stateless, bestStateless := len(cd.vols) == 0, len(best.vols) == 0
					better = stateless && !bestStateless || stateless == bestStateless && gain > bestGain
				}
				if better {
					best, bestTo, bestSpread, bestGain = cd, to, spread, gain
				}
			}
		}
		if best == nil {
			break
		}
		put(best, bestTo)
	}

	var moves []rebalanceMove
	for _, cd := range cands {
		if cd.at != cd.home {
			moves = append(moves, rebalanceMove{in: cd.in, vols: cd.vols, to: cd.at.name})
		}
	}
	return moves
}
