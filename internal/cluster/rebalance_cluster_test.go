package cluster_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
	_ "modernc.org/sqlite"
)

func TestRebalanceAfterAnUndrain(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.join("w2")
	h.waitReady(3)
	// Everything on the control node, as after draining the workers.
	undo := h.onlyOn("control")
	h.deploy("shop", 3)
	h.deployWith("db1", 1, h.withVolume("db1", "data"))
	h.deployWith("db2", 1, h.withVolume("db2", "data"))
	h.deploy("nap", 1)
	undo()
	if got := h.placement("shop"); got["control"] != 3 {
		t.Fatalf("shop placed %v, want all on control", got)
	}
	if err := h.client.SetProperty(h.ctx, "nap", "ps", "sleep-after", "1h"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.SetAppAsleep(h.ctx, "nap", true); err != nil {
		t.Fatal(err)
	}
	h.ctl.Changed()
	eventually(t, 5*time.Second, "nap goes to sleep", func() error {
		if n := h.nodes["control"].rt.apps()["nap"]; n != 0 {
			return fmt.Errorf("control runs %d nap VMs", n)
		}
		return nil
	})

	// A dry run says what would move, and moves nothing.
	dry, err := h.client.Rebalance(h.ctx, types.RebalanceRequest{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Moves) != 3 || dry.Pinned != 1 {
		t.Fatalf("dry run: %d moves, %d pinned; want two of shop and nap, and one database with --volumes: %+v", len(dry.Moves), dry.Pinned, dry)
	}
	insts, _ := h.st.Instances(h.ctx, "")
	for _, in := range insts {
		if in.Replaces != "" || in.Node != "control" {
			t.Fatalf("the dry run moved %s %s to %s", in.App, in.Name(), in.Node)
		}
	}

	r, err := h.client.Rebalance(h.ctx, types.RebalanceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(r.Moves) != fmt.Sprint(dry.Moves) {
		t.Errorf("moved %v, the dry run said %v", r.Moves, dry.Moves)
	}
	eventually(t, 15*time.Second, "shop spreads one per node", func() error {
		if got := h.placement("shop"); got["control"] != 1 || got["w1"] != 1 || got["w2"] != 1 {
			return fmt.Errorf("placement %v", got)
		}
		if n := h.nodes["control"].rt.apps()["shop"]; n != 1 {
			return fmt.Errorf("control still runs %d shop VMs", n)
		}
		return nil
	})
	for _, app := range []string{"db1", "db2"} {
		if in := h.web1(app); in == nil || in.Node != "control" {
			t.Errorf("%s left its volume's node without --volumes: %+v", app, in)
		}
	}
	// nap moves while asleep: its new instance waits for it to wake, and the
	// old one is let go at once.
	eventually(t, 5*time.Second, "nap moves", func() error {
		ids := h.instanceIDs("nap")
		if len(ids) != 1 || strings.HasSuffix(ids[0], "@control") {
			return fmt.Errorf("nap's wanted instances: %v", ids)
		}
		return nil
	})
	for _, n := range h.nodes {
		if got := n.rt.apps()["nap"]; got != 0 {
			t.Errorf("%s runs %d nap VMs while it sleeps", n.name, got)
		}
	}

	// With volumes, one database follows its disk to the emptiest node.
	r, err = h.client.Rebalance(h.ctx, types.RebalanceRequest{Volumes: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Moves) != 1 || len(r.Moves[0].Volumes) != 1 || r.Moves[0].To != "w2" {
		t.Fatalf("moves with volumes: %+v", r.Moves)
	}
	m := r.Moves[0]
	eventually(t, 15*time.Second, "the database moves with its disk", func() error {
		in := h.web1(m.App)
		if in == nil || in.Node != m.To || in.State != store.StateHealthy {
			return fmt.Errorf("web.1: %+v", in)
		}
		if v := h.volume(m.App, "data"); v.Node != m.To || v.Moving() {
			return fmt.Errorf("volume on %s, moving to %q", v.Node, v.MovingTo)
		}
		return nil
	})
	h.noDoubleAttach()

	r, err = h.client.Rebalance(h.ctx, types.RebalanceRequest{Volumes: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Moves) != 0 {
		t.Errorf("balanced, yet a rebalance would move %+v", r.Moves)
	}
}

func TestMovesAvoidANodeKeepingAnOldCopy(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.waitReady(2)
	undo := h.onlyOn("control")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()
	// As if it had been restored onto the control node while w1 was down.
	db, err := sql.Open("sqlite", filepath.Join(h.dir, "jokku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(h.ctx, "UPDATE volumes SET stale_node = 'w1' WHERE id = ?", h.volume("db", "data").ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.MoveVolume(h.ctx, "db", "data", "w1"); err == nil || !strings.Contains(err.Error(), "old copy") {
		t.Errorf("storage:move onto the node keeping an old copy: %v", err)
	}
	if r, err := h.client.Rebalance(h.ctx, types.RebalanceRequest{Volumes: true, DryRun: true}); err != nil || len(r.Moves) != 0 {
		t.Errorf("rebalance: %+v, %v", r, err)
	}
	// Draining the control node finds nowhere for it.
	if err := h.st.SetNodeFlag(h.ctx, "control", "draining", true); err != nil {
		t.Fatal(err)
	}
	h.ctl.Changed()
	eventually(t, 5*time.Second, "the drain says why it can't move db", func() error {
		if !h.hasEvent("db", "keeps an old copy") {
			return fmt.Errorf("no event")
		}
		return nil
	})
	if v := h.volume("db", "data"); v.Moving() {
		t.Errorf("the drain moved the volume to %s", v.MovingTo)
	}
}
