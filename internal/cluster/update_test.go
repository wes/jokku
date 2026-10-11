package cluster_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/types"
)

// updates watches the cluster's fake update.sh runs.
type updates struct {
	mu     sync.Mutex
	order  []string // node@version, in the order they started
	active atomic.Int32
	most   atomic.Int32 // the most at once
}

func (u *updates) started() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.order)
}

// fakeUpdater stands in for update.sh: it stops its node's agent for a
// moment and starts it again on the new version, or, while fail is above
// zero, on the old one, as a rollback would.
type fakeUpdater struct {
	h        *harness
	node     string
	all      *updates
	down     time.Duration // how long jokku is stopped
	fail     atomic.Int32  // how many updates to fail
	startErr error         // Start fails with it
	hang     bool          // never finish
	busy     atomic.Bool
}

func (u *fakeUpdater) Start(_ context.Context, v string) error {
	if u.startErr != nil {
		return u.startErr
	}
	u.all.mu.Lock()
	u.all.order = append(u.all.order, u.node+"@"+v)
	u.all.mu.Unlock()
	u.busy.Store(true)
	if n := u.all.active.Add(1); n > u.all.most.Load() {
		u.all.most.Store(n)
	}
	if u.hang {
		return nil
	}
	go func() {
		defer u.busy.Store(false)
		defer u.all.active.Add(-1)
		u.h.kill(u.node)
		time.Sleep(u.down)
		if u.fail.Add(-1) < 0 {
			u.h.nodes[u.node].version = v
		}
		u.h.revive(u.node)
	}()
	return nil
}

func (u *fakeUpdater) Running(context.Context) bool { return u.busy.Load() }

func (h *harness) updater(node string, all *updates) *fakeUpdater {
	return &fakeUpdater{h: h, node: node, all: all, down: 300 * time.Millisecond}
}

// nodeViews is the status's nodes by name.
func (h *harness) nodeViews() map[string]types.NodeView {
	h.t.Helper()
	st, err := h.ctl.Status(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]types.NodeView{}
	for _, n := range st.Nodes {
		out[n.Name] = n
	}
	return out
}

func TestNodesUpdateOneAtATime(t *testing.T) {
	h := newHarnessAt(t, "v0.9.1")
	all := &updates{}
	h.joinAt("w2", "v0.9.0", h.updater("w2", all))
	h.joinAt("w1", "v0.9.0", h.updater("w1", all))
	h.joinAt("old", "v0.8.4", nil) // from before nodes updated themselves
	h.joinAt("new", "v0.9.1", h.updater("new", all))
	h.waitReady(5)

	// Nothing happens until the cluster is asked to update.
	time.Sleep(500 * time.Millisecond)
	if got := all.started(); len(got) != 0 {
		t.Fatalf("updates started unasked: %v", got)
	}
	if err := h.ctl.UpdateNodes(h.ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "w1 and w2 updated", func() error {
		v := h.nodeViews()
		for _, name := range []string{"w1", "w2"} {
			if v[name].Version != "v0.9.1" || v[name].Update != nil {
				return fmt.Errorf("%s runs %s (update %+v)", name, v[name].Version, v[name].Update)
			}
		}
		return nil
	})
	if got, want := all.started(), []string{"w1@v0.9.1", "w2@v0.9.1"}; !slices.Equal(got, want) {
		t.Errorf("updates %v, want %v", got, want)
	}
	if n := all.most.Load(); n != 1 {
		t.Errorf("%d nodes updated at once, want 1", n)
	}
	v := h.nodeViews()
	if u := v["old"].Update; u == nil || u.State != types.UpdateByHand || u.To != "v0.9.1" {
		t.Errorf("old: update %+v, want by hand to v0.9.1", u)
	}
	if v["new"].Update != nil || v["control"].Update != nil {
		t.Errorf("nodes already on v0.9.1 have an update: %+v, %+v", v["new"].Update, v["control"].Update)
	}
	for _, e := range []string{"updating w1 from v0.9.0 to v0.9.1", "w1 is updated to v0.9.1"} {
		if !h.hasEvent("", e) {
			t.Errorf("no event %q", e)
		}
	}
	// Each node got its order once.
	time.Sleep(time.Second)
	if got := all.started(); len(got) != 2 {
		t.Errorf("updates %v, want two", got)
	}
}

func TestAFailedNodeUpdateStopsTheRest(t *testing.T) {
	h := newHarnessAt(t, "v0.9.1")
	all := &updates{}
	u1 := h.updater("w1", all)
	u1.fail.Store(1)
	h.joinAt("w1", "v0.9.0", u1)
	h.joinAt("w2", "v0.9.0", h.updater("w2", all))
	h.waitReady(3)

	if err := h.ctl.UpdateNodes(h.ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "w1's update failed", func() error {
		v := h.nodeViews()
		if u := v["w1"].Update; u == nil || u.State != types.UpdateFailed {
			return fmt.Errorf("w1: update %+v", u)
		}
		return nil
	})
	v := h.nodeViews()
	if !strings.Contains(v["w1"].Update.Error, "still on v0.9.0") {
		t.Errorf("w1's error %q should say it is still on v0.9.0", v["w1"].Update.Error)
	}
	if u := v["w2"].Update; u == nil || u.State != types.UpdateOnHold {
		t.Errorf("w2: update %+v, want on hold", u)
	}
	time.Sleep(time.Second)
	if got := all.started(); !slices.Equal(got, []string{"w1@v0.9.1"}) {
		t.Fatalf("updates %v: w1 should have been tried once, and w2 not at all", got)
	}

	// The next "jokku update" tries w1 again, then goes on.
	if err := h.ctl.UpdateNodes(h.ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "both updated", func() error {
		for name, n := range h.nodeViews() {
			if n.Version != "v0.9.1" {
				return fmt.Errorf("%s runs %s", name, n.Version)
			}
		}
		return nil
	})
	if got, want := all.started(), []string{"w1@v0.9.1", "w1@v0.9.1", "w2@v0.9.1"}; !slices.Equal(got, want) {
		t.Errorf("updates %v, want %v", got, want)
	}
}

func TestANodeThatCannotStartItsUpdateSaysWhy(t *testing.T) {
	h := newHarnessAt(t, "v0.9.1")
	u := h.updater("w1", &updates{})
	u.startErr = errors.New("downloading the v0.9.1 updater: no route to host")
	h.joinAt("w1", "v0.9.0", u)
	h.waitReady(2)
	if err := h.ctl.UpdateNodes(h.ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "w1's update failed", func() error {
		if u := h.nodeViews()["w1"].Update; u == nil || u.State != types.UpdateFailed || !strings.Contains(u.Error, "no route to host") {
			return fmt.Errorf("w1: update %+v", u)
		}
		return nil
	})
}

func TestANodeThatNeverComesBackTimesOut(t *testing.T) {
	defer func(d time.Duration) { cluster.UpdateTimeout = d }(cluster.UpdateTimeout)
	cluster.UpdateTimeout = time.Second
	h := newHarnessAt(t, "v0.9.1")
	u := h.updater("w1", &updates{})
	u.hang = true
	h.joinAt("w1", "v0.9.0", u)
	h.waitReady(2)
	if err := h.ctl.UpdateNodes(h.ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "w1's update timed out", func() error {
		if u := h.nodeViews()["w1"].Update; u == nil || u.State != types.UpdateFailed || !strings.Contains(u.Error, "did not come back on v0.9.1") {
			return fmt.Errorf("w1: update %+v", u)
		}
		return nil
	})
}

func TestAnUpdatingNodeKeepsItsInstances(t *testing.T) {
	h := newHarnessAt(t, "v0.9.1")
	u := h.updater("w1", &updates{})
	u.down = cluster.RescheduleAfter + 2*time.Second // long enough that a down node's instances would move
	w1 := h.joinAt("w1", "v0.9.0", u)
	h.waitReady(2)
	h.deploy("api", 2)
	if got := h.placement("api"); got["w1"] != 1 {
		t.Fatalf("placement %v, want one instance on w1", got)
	}
	if err := h.ctl.UpdateNodes(h.ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "w1 updated", func() error {
		if v := h.nodeViews()["w1"]; v.Version != "v0.9.1" {
			return fmt.Errorf("w1 runs %s", v.Version)
		}
		return nil
	})
	if h.hasEvent("api", "moving api") {
		t.Error("api's instance moved off w1 while it updated")
	}
	if !h.hasEvent("", "w1 is restarting to update jokku") {
		t.Error("no event saying w1 went quiet to update")
	}
	if n := w1.rt.apps()["api"]; n != 1 {
		t.Errorf("w1 runs %d api VMs after its update, want 1", n)
	}
}

func TestNodesOnAnotherProtocolAreToldToUpdate(t *testing.T) {
	h := newHarnessAt(t, "v0.9.1")
	h.join("w1")
	h.waitReady(2)
	err := h.ctl.Report(h.ctx, "w1", &types.NodeStatus{Protocol: types.ProtocolVersion + 1, Version: "v0.9.0", Features: []string{types.FeatureUpdate}})
	if err == nil || !strings.Contains(err.Error(), "told to update itself to v0.9.1") {
		t.Fatalf("report: %v", err)
	}
	st, err := h.ctl.ComputeState(h.ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Update == nil || st.Update.Version != "v0.9.1" {
		t.Errorf("w1's state has update %+v, want an order for v0.9.1", st.Update)
	}
}

func TestADevelopmentBuildDoesNotUpdateTheCluster(t *testing.T) {
	h := newHarnessAt(t, "v0.9.1-3-gabc1234")
	if err := h.ctl.UpdateNodes(h.ctx); err == nil || !strings.Contains(err.Error(), "development build") {
		t.Fatalf("UpdateNodes: %v", err)
	}
}
