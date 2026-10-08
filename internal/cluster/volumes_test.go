package cluster_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// withVolume is a deployWith setup that mounts a fresh volume at /data in
// web.
func (h *harness) withVolume(app, name string) func() {
	return func() {
		v := &store.Volume{App: app, Name: name, Type: types.VolumeLocal, SizeMB: 64}
		if err := h.st.CreateVolume(h.ctx, v); err != nil {
			h.t.Fatal(err)
		}
		if err := h.st.AddVolumeMount(h.ctx, v.ID, types.VolumeMount{ProcessType: "web", Path: "/data"}); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) volume(app, name string) *store.Volume {
	h.t.Helper()
	v, err := h.st.Volume(h.ctx, app, name)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

func (h *harness) disk(node string, v *store.Volume) string {
	return filepath.Join(h.nodes[node].dir, "volumes", v.ID+".ext4")
}

// web1 is the app's wanted web.1 instance.
func (h *harness) web1(app string) *store.Instance {
	h.t.Helper()
	insts, _ := h.st.Instances(h.ctx, app)
	for _, in := range insts {
		if in.Desired == store.DesiredRunning && in.ProcessType == "web" {
			return &in
		}
	}
	return nil
}

func (h *harness) waitReady(n int) {
	h.t.Helper()
	eventually(h.t, 5*time.Second, "nodes ready", func() error {
		if st, _ := h.ctl.Status(h.ctx); st.Totals.NodesReady != n {
			return fmt.Errorf("%d of %d nodes ready", st.Totals.NodesReady, n)
		}
		return nil
	})
}

// onlyOn makes deploys land on node, by marking the others unschedulable.
func (h *harness) onlyOn(node string) (undo func()) {
	var others []string
	for name := range h.nodes {
		if name != node {
			others = append(others, name)
			h.st.SetNodeFlag(h.ctx, name, "schedulable", false)
		}
	}
	return func() {
		for _, name := range others {
			h.st.SetNodeFlag(h.ctx, name, "schedulable", true)
		}
	}
}

// writeData puts recognizable blocks into a volume's disk, as an app would.
func writeData(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i, off := range []int64{0, 5 << 20, 40 << 20} {
		if _, err := f.WriteAt(bytes.Repeat([]byte{byte(i + 1)}, 300<<10), off); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(path)
	return b
}

func (h *harness) noDoubleAttach() {
	h.t.Helper()
	if p := h.disks.problems(); len(p) > 0 {
		h.t.Fatalf("a volume was attached to two VMs at once:\n%s", strings.Join(p, "\n"))
	}
}

func TestVolumePinsItsInstanceAcrossRestarts(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.waitReady(2)
	undo := h.onlyOn("w1")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()

	v := h.volume("db", "data")
	if v.Node != "w1" {
		t.Fatalf("volume placed on %q, want w1 with its instance", v.Node)
	}
	if in := h.web1("db"); in == nil || in.Node != "w1" || len(in.Volumes) != 1 || in.Volumes[0].Path != "/data" {
		t.Fatalf("web.1: %+v", in)
	}
	eventually(t, 3*time.Second, "w1 makes the disk and reports it", func() error {
		if v := h.volume("db", "data"); v.State != store.VolumeReady {
			return fmt.Errorf("state %s", v.State)
		}
		return nil
	})
	want := writeData(t, h.disk("w1", v))

	// Restarts keep it on w1 even though the control node is emptier, and
	// never run two VMs on the disk.
	for range 3 {
		if err := h.pipe.PS(h.ctx, "db", "restart", "test", func(string) {}); err != nil {
			t.Fatal(err)
		}
		if in := h.web1("db"); in.Node != "w1" {
			t.Fatalf("restart moved web.1 to %s", in.Node)
		}
	}
	h.noDoubleAttach()
	if got, _ := os.ReadFile(h.disk("w1", v)); !bytes.Equal(got, want) {
		t.Fatal("the disk changed across restarts")
	}
}

func TestDrainMovesAVolumeWithItsInstance(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.waitReady(2)
	undo := h.onlyOn("w1")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()
	v := h.volume("db", "data")
	eventually(t, 3*time.Second, "disk ready on w1", func() error {
		if _, err := os.Stat(h.disk("w1", v)); err != nil {
			return err
		}
		return nil
	})
	want := writeData(t, h.disk("w1", v))

	if err := h.st.SetNodeFlag(h.ctx, "w1", "draining", true); err != nil {
		t.Fatal(err)
	}
	h.ctl.Changed()
	eventually(t, 15*time.Second, "web.1 healthy on the control node with its volume", func() error {
		in := h.web1("db")
		if in == nil || in.Node != "control" || in.State != store.StateHealthy {
			return fmt.Errorf("web.1: %+v", in)
		}
		if v := h.volume("db", "data"); v.Node != "control" || v.Moving() || v.PreviousNode != "" {
			return fmt.Errorf("volume: node %s, moving to %q, previous %q", v.Node, v.MovingTo, v.PreviousNode)
		}
		return nil
	})
	if got, err := os.ReadFile(h.disk("control", v)); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the moved disk differs from the original (%v)", err)
	}
	eventually(t, 5*time.Second, "w1 deletes its old copy", func() error {
		entries, _ := os.ReadDir(filepath.Join(h.nodes["w1"].dir, "volumes"))
		if len(entries) > 0 {
			return fmt.Errorf("w1 still has %v", entries[0].Name())
		}
		return nil
	})
	if n := h.nodes["w1"].rt.apps()["db"]; n != 0 {
		t.Fatalf("w1 still runs %d db VMs", n)
	}
	h.noDoubleAttach()
	// A drained node holding nothing can go.
	vols, _ := h.st.Volumes(h.ctx, "")
	for _, v := range vols {
		if v.Node == "w1" {
			t.Fatalf("w1 still holds %s", v.Name)
		}
	}
}

func TestDrainMovesAnUnusedVolume(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.waitReady(2)
	undo := h.onlyOn("w1")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()
	v := h.volume("db", "data")
	eventually(t, 3*time.Second, "disk ready", func() error {
		if v := h.volume("db", "data"); v.State != store.VolumeReady {
			return fmt.Errorf("state %s", v.State)
		}
		return nil
	})
	want := writeData(t, h.disk("w1", v))
	// Scaled to zero: no instance uses the volume, but it must still leave.
	h.st.Scale(h.ctx, "db", map[string]int{"web": 0})
	if err := h.pipe.PS(h.ctx, "db", "restart", "test", func(string) {}); err != nil {
		t.Fatal(err)
	}
	h.st.SetNodeFlag(h.ctx, "w1", "draining", true)
	h.ctl.Changed()
	eventually(t, 15*time.Second, "volume moved to the control node", func() error {
		if v := h.volume("db", "data"); v.Node != "control" || v.Moving() {
			return fmt.Errorf("node %s, moving to %q", v.Node, v.MovingTo)
		}
		return nil
	})
	eventually(t, 3*time.Second, "disk arrived", func() error {
		if got, err := os.ReadFile(h.disk("control", v)); err != nil || !bytes.Equal(got, want) {
			return fmt.Errorf("disk differs (%v)", err)
		}
		return nil
	})
}

func TestDeadNodeKeepsItsVolumeInstance(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.waitReady(2)
	undo := h.onlyOn("w1")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()
	before := h.web1("db")

	h.kill("w1")
	time.Sleep(store.NodeDownAfter + cluster.RescheduleAfter + 2*time.Second)
	if in := h.web1("db"); in == nil || in.ID != before.ID || in.Node != "w1" {
		t.Fatalf("web.1 was moved off the dead node away from its data: %+v", in)
	}
	insts, _ := h.st.Instances(h.ctx, "db")
	if len(insts) != 1 {
		t.Fatalf("%d instances, want just the original", len(insts))
	}
	events, _ := h.st.Events(h.ctx, "db", 20)
	found := false
	for _, e := range events {
		found = found || strings.Contains(e.Message, "stays on w1")
	}
	if !found {
		t.Error("no event says why web.1 stays")
	}

	h.revive("w1")
	eventually(t, 5*time.Second, "web.1 healthy again on w1", func() error {
		if in := h.web1("db"); in == nil || in.Node != "w1" || in.State != store.StateHealthy {
			return fmt.Errorf("web.1: %+v", in)
		}
		return nil
	})
}

func TestMoveIsCalledOffWhenTheTargetDies(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.join("w2")
	h.waitReady(3)
	undo := h.onlyOn("w1")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()
	v := h.volume("db", "data")
	eventually(t, 3*time.Second, "disk ready", func() error {
		if v := h.volume("db", "data"); v.State != store.VolumeReady {
			return fmt.Errorf("state %s", v.State)
		}
		return nil
	})
	want := writeData(t, h.disk("w1", v))

	// The VM on w1 hangs on shutdown, so the final copy can't happen.
	h.nodes["w1"].rt.ignoreStops.Store(true)
	if err := h.ctl.MoveVolume(h.ctx, "db", "data", "w2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "w2 copied what it could and the instance was told to stop", func() error {
		if v := h.volume("db", "data"); v.Transfer != types.VolumeSynced {
			return fmt.Errorf("transfer %q", v.Transfer)
		}
		insts, _ := h.st.Instances(h.ctx, "db")
		for _, in := range insts {
			if in.Node == "w1" && in.Desired == store.DesiredRunning {
				return fmt.Errorf("the original still runs")
			}
		}
		return nil
	})
	h.kill("w2")
	eventually(t, 10*time.Second, "the move is called off and web.1 is back on w1", func() error {
		if v := h.volume("db", "data"); v.Moving() || v.Node != "w1" {
			return fmt.Errorf("volume on %s, moving to %q", v.Node, v.MovingTo)
		}
		if in := h.web1("db"); in == nil || in.Node != "w1" {
			return fmt.Errorf("web.1: %+v", in)
		}
		return nil
	})
	// The hung VM finally lets go; the restored instance starts on the disk.
	h.nodes["w1"].rt.ignoreStops.Store(false)
	eventually(t, 10*time.Second, "web.1 healthy on w1", func() error {
		if in := h.web1("db"); in == nil || in.Node != "w1" || in.State != store.StateHealthy {
			return fmt.Errorf("web.1: %+v", in)
		}
		return nil
	})
	if got, _ := os.ReadFile(h.disk("w1", v)); !bytes.Equal(got, want) {
		t.Fatal("the disk changed")
	}

	// A move to a node that stays up goes through.
	if err := h.ctl.MoveVolume(h.ctx, "db", "data", "control"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "moved to the control node", func() error {
		in := h.web1("db")
		if v := h.volume("db", "data"); v.Node != "control" || v.Moving() || in == nil || in.Node != "control" || in.State != store.StateHealthy {
			return fmt.Errorf("volume on %s (moving to %q), web.1 %+v", v.Node, v.MovingTo, in)
		}
		return nil
	})
	if got, _ := os.ReadFile(h.disk("control", v)); !bytes.Equal(got, want) {
		t.Fatal("the moved disk differs")
	}
	h.noDoubleAttach()
}

func TestDestroyedVolumesAreDeleted(t *testing.T) {
	h := newHarness(t)
	h.deployWith("db", 1, h.withVolume("db", "data"))
	v := h.volume("db", "data")
	eventually(t, 3*time.Second, "disk ready", func() error {
		_, err := os.Stat(h.disk("control", v))
		return err
	})

	// Unmount, restart without it, destroy it.
	if err := h.st.RemoveVolumeMount(h.ctx, v.ID, v.Mounts[0]); err != nil {
		t.Fatal(err)
	}
	if err := h.pipe.PS(h.ctx, "db", "restart", "test", func(string) {}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.DestroyVolume(h.ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	h.ctl.Changed()
	eventually(t, 5*time.Second, "disk deleted and volume forgotten", func() error {
		if _, err := os.Stat(h.disk("control", v)); !os.IsNotExist(err) {
			return fmt.Errorf("disk still there (%v)", err)
		}
		if _, err := h.st.VolumeByID(h.ctx, v.ID); err == nil {
			return fmt.Errorf("volume still recorded")
		}
		return nil
	})

	// Destroying an app deletes its volumes' disks too.
	h.deployWith("blog", 1, h.withVolume("blog", "uploads"))
	u := h.volume("blog", "uploads")
	eventually(t, 3*time.Second, "disk ready", func() error {
		_, err := os.Stat(h.disk("control", u))
		return err
	})
	if err := h.st.DeleteApp(h.ctx, "blog"); err != nil {
		t.Fatal(err)
	}
	h.ctl.Changed()
	eventually(t, 5*time.Second, "the destroyed app's disk is deleted", func() error {
		if _, err := os.Stat(h.disk("control", u)); !os.IsNotExist(err) {
			return fmt.Errorf("disk still there (%v)", err)
		}
		if _, err := h.st.VolumeByID(h.ctx, u.ID); err == nil {
			return fmt.Errorf("volume still recorded")
		}
		return nil
	})
}
