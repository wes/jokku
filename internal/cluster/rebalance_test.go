package cluster

import (
	"fmt"
	"testing"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// emptied is a cluster just after its workers were drained and undrained:
// five nodes, everything on jokku1, as on 2026-10-17. alechuxley's volume
// was once restored away from jokku5, which keeps the old copy.
type emptied struct {
	now   time.Time
	nodes []store.Node
	insts []store.Instance
	vols  []store.Volume
	apps  []types.App
}

func newEmptied() *emptied {
	e := &emptied{now: time.Now()}
	for i := 1; i <= 5; i++ {
		e.nodes = append(e.nodes, store.Node{
			Name: fmt.Sprintf("jokku%d", i), SubnetIndex: i, CPUs: 6, MemoryMB: 29359, Schedulable: true, LastSeen: e.now,
			Features: []string{types.FeatureVolumes}, Metrics: types.NodeMetrics{DiskMB: 200 << 10, DiskFreeMB: 150 << 10},
		})
	}
	e.nodes = append(e.nodes, store.Node{Name: "jokku-edge1", Role: store.RoleEdge, SubnetIndex: 6, CPUs: 1, MemoryMB: 956,
		Schedulable: true, LastSeen: e.now})
	for _, app := range []string{"alfordelectric", "elrey", "elreyliquors", "foundersabq", "jokkudev", "taosfriction", "wes-katie", "wunl"} {
		mem := 256
		if app == "elrey" {
			mem = 512
		}
		e.add(app, "web", 1, mem)
	}
	e.add("elreyrentals", "web", 1, 256)
	e.add("elreyrentals", "cron", 1, 256)
	for i := 1; i <= 3; i++ {
		e.add("joedesigns", "web", i, 512)
	}
	e.add("umami", "umami", 1, 2048)
	e.add("alechuxley", "wordpress", 1, 2048, "wp-core")
	e.add("beszel", "beszel", 1, 256, "beszel-data")
	e.add("duckhouse-status", "uptime-kuma", 1, 256, "kuma-data")
	e.add("famdash", "web", 1, 256, "photos")
	e.add("uptime-limehosting", "uptime-kuma", 1, 256, "uptime-kuma-data")
	e.add("mysql-alechuxley-db", "mysql", 1, 1024, "data")
	e.add("mysql-founders", "mysql", 1, 1024, "data")
	e.add("postgres-elrey-db", "postgres", 1, 512, "data")
	e.add("postgres-umami-db", "postgres", 1, 512, "data")
	asleep := e.now.Add(-time.Hour)
	for i := range e.apps {
		if e.apps[i].Name == "alfordelectric" {
			e.apps[i].AsleepSince = &asleep
		}
	}
	for i := range e.insts {
		switch e.insts[i].App {
		case "alfordelectric":
			e.insts[i].State = store.StateStopped
		case "joedesigns":
			e.insts[i].CPUs = 2
		}
	}
	for i := range e.vols {
		if e.vols[i].App == "alechuxley" {
			e.vols[i].StaleNode = "jokku5"
		}
	}
	return e
}

// add puts an instance on jokku1, with its volume if one is named.
func (e *emptied) add(app, proc string, index, memMB int, volume ...string) {
	found := false
	for _, a := range e.apps {
		found = found || a.Name == app
	}
	if !found {
		e.apps = append(e.apps, types.App{Name: app})
	}
	in := store.Instance{
		ID: fmt.Sprintf("%s-%s-%d", app, proc, index), App: app, ProcessType: proc, Index: index, Node: "jokku1",
		CPUs: 1, MemoryMB: memMB, Desired: store.DesiredRunning, State: store.StateHealthy,
	}
	for _, name := range volume {
		e.vols = append(e.vols, store.Volume{ID: app + "-" + name, App: app, Name: name, Type: types.VolumeLocal,
			Node: "jokku1", State: store.VolumeReady, UsedMB: 1000})
		in.Volumes = append(in.Volumes, types.InstanceVolume{ID: app + "-" + name, Path: "/data"})
	}
	e.insts = append(e.insts, in)
}

func (e *emptied) plan(withVolumes bool) []rebalanceMove {
	return planRebalance(e.now, e.nodes, e.insts, e.vols, e.apps, withVolumes)
}

// apply makes the moves, as if they had finished.
func (e *emptied) apply(moves []rebalanceMove) {
	for _, m := range moves {
		for i := range e.insts {
			if e.insts[i].ID == m.in.ID {
				e.insts[i].Node = m.to
			}
		}
		for _, mv := range m.vols {
			for i := range e.vols {
				if e.vols[i].ID == mv.ID {
					e.vols[i].Node = m.to
				}
			}
		}
	}
}

// used is the memory each node's instances hold.
func (e *emptied) used() map[string]int {
	out := map[string]int{}
	for _, in := range e.insts {
		if held(in, e.now) {
			out[in.Node] += in.MemoryMB
		}
	}
	return out
}

func (e *emptied) on(app string) map[string]int {
	out := map[string]int{}
	for _, in := range e.insts {
		if in.App == app && in.Desired == store.DesiredRunning {
			out[in.Node]++
		}
	}
	return out
}

func TestRebalanceLeavesVolumesUnlessAsked(t *testing.T) {
	e := newEmptied()
	moves := e.plan(false)
	for _, m := range moves {
		if len(m.in.Volumes) > 0 || len(m.vols) > 0 {
			t.Errorf("%s %s moved with its volumes, unasked", m.in.App, m.in.Name())
		}
		if m.to == "jokku1" || m.to == "jokku-edge1" {
			t.Errorf("%s %s moved to %s", m.in.App, m.in.Name(), m.to)
		}
	}
	e.apply(moves)
	if got := e.on("joedesigns"); len(got) != 3 {
		t.Errorf("joedesigns' three web instances share nodes: %v", got)
	}
	// Only what the volumes hold stays on jokku1; the rest is shared out.
	for _, in := range e.insts {
		if in.Node == "jokku1" && len(in.Volumes) == 0 {
			t.Errorf("%s %s stayed on jokku1", in.App, in.Name())
		}
	}
	used := e.used()
	if used["jokku-edge1"] != 0 {
		t.Errorf("the edge got %d MiB", used["jokku-edge1"])
	}
	lo, hi := 1<<30, 0
	for _, n := range []string{"jokku2", "jokku3", "jokku4", "jokku5"} {
		lo, hi = min(lo, used[n]), max(hi, used[n])
	}
	// umami alone holds 2 GiB; the others share out the rest.
	if hi-lo > 1024 {
		t.Errorf("the workers are uneven: %v", used)
	}
	if again := e.plan(false); len(again) > 0 {
		t.Errorf("a second rebalance moves %d more, the first of them %s %s to %s", len(again), again[0].in.App, again[0].in.Name(), again[0].to)
	}
}

func TestRebalanceWithVolumes(t *testing.T) {
	e := newEmptied()
	moves := e.plan(true)
	withDisks := 0
	for _, m := range moves {
		if len(m.vols) > 0 {
			withDisks++
		}
		if m.in.App == "alechuxley" && m.to == "jokku5" {
			t.Error("alechuxley's volume went to jokku5, which keeps an old copy of it")
		}
	}
	if withDisks == 0 {
		t.Fatal("nothing with volumes moved")
	}
	e.apply(moves)
	used := e.used()
	lo, hi := 1<<30, 0
	for _, n := range []string{"jokku1", "jokku2", "jokku3", "jokku4", "jokku5"} {
		lo, hi = min(lo, used[n]), max(hi, used[n])
	}
	if hi-lo > 1024 {
		t.Errorf("the nodes are uneven: %v", used)
	}
	if again := e.plan(true); len(again) > 0 {
		t.Errorf("a second rebalance moves %d more", len(again))
	}
	// Fewer stops beat a closer balance: instances without volumes move
	// first, so most with volumes stay.
	if withDisks > 5 {
		t.Errorf("%d instances with volumes moved", withDisks)
	}
}

func TestRebalanceLeavesBusyAppsAndUnavailableNodes(t *testing.T) {
	e := newEmptied()
	e.nodes[1].Draining = true                        // jokku2
	e.nodes[2].Schedulable = false                    // jokku3
	e.nodes[3].LastSeen = e.now.Add(-5 * time.Minute) // jokku4 is down
	e.nodes[4].CanRun = "no KVM"                      // jokku5
	if moves := e.plan(true); len(moves) > 0 {
		t.Fatalf("moved %s %s to %s with no other node available", moves[0].in.App, moves[0].in.Name(), moves[0].to)
	}

	e = newEmptied()
	// joedesigns is deploying; umami's volume-less move is under way.
	e.insts = append(e.insts, store.Instance{ID: "joedesigns-new", App: "joedesigns", ProcessType: "web", Index: 1, Node: "jokku2",
		MemoryMB: 512, CPUs: 2, Desired: store.DesiredRunning, State: store.StateStarting})
	e.insts = append(e.insts, store.Instance{ID: "umami-new", App: "umami", ProcessType: "umami", Index: 1, Node: "jokku3",
		MemoryMB: 2048, CPUs: 1, Desired: store.DesiredRunning, State: store.StatePending, Replaces: "umami-umami-1"})
	for _, m := range e.plan(false) {
		if m.in.App == "joedesigns" || m.in.App == "umami" {
			t.Errorf("%s %s moved while its app is busy", m.in.App, m.in.Name())
		}
	}
}

func TestRebalanceMovesAsleepApps(t *testing.T) {
	e := newEmptied()
	for _, m := range e.plan(false) {
		if m.in.App == "alfordelectric" {
			return
		}
	}
	t.Fatal("alfordelectric, asleep, stayed on jokku1")
}

func TestRebalanceFitsMemory(t *testing.T) {
	e := newEmptied()
	// jokku2 has room for one 2 GiB instance, no more.
	e.nodes[1].MemoryMB = 3000
	for i := 2; i < 5; i++ {
		e.nodes[i].Schedulable = false
	}
	e.apply(e.plan(false))
	if used := e.used(); used["jokku2"] > 3000-reserveMB(3000) {
		t.Fatalf("jokku2 holds %d MiB of %d", used["jokku2"], 3000-reserveMB(3000))
	}
}
