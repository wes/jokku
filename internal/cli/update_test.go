package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/types"
)

// fakeRollout serves the cluster's status as an update moves through it, a
// step per status request.
type fakeRollout struct {
	mu     sync.Mutex
	steps  [][]types.NodeView
	asked  int // POST /v1/cluster/update
	served int
}

func (f *fakeRollout) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "POST /v1/cluster/update":
		f.asked++
		w.WriteHeader(http.StatusNoContent)
	case "GET /v1/cluster/status":
		step := f.steps[min(f.served, len(f.steps)-1)]
		f.served++
		json.NewEncoder(w).Encode(types.ClusterStatus{Version: "v0.9.1", Control: "control", Nodes: step})
	default:
		http.NotFound(w, r)
	}
}

func serveFake(t *testing.T, h http.Handler) *client.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "jk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: h}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })
	return client.New(client.Target{Socket: filepath.Join(dir, "s.sock")}, "")
}

func nodeAt(name, v, status string, u *types.NodeUpdate) types.NodeView {
	return types.NodeView{Node: types.Node{Name: name, Status: status}, Version: v, Update: u}
}

func TestUpdateFollowsTheNodes(t *testing.T) {
	defer func(d time.Duration) { updatePoll = d }(updatePoll)
	updatePoll = time.Millisecond
	to := func(state string) *types.NodeUpdate { return &types.NodeUpdate{To: "v0.9.1", State: state} }
	control := nodeAt("control", "v0.9.1", "ready", nil)
	old := nodeAt("old", "v0.8.4", "ready", to(types.UpdateByHand))
	away := nodeAt("away", "v0.9.0", "down", to(types.UpdateWaiting))
	f := &fakeRollout{steps: [][]types.NodeView{
		{control, nodeAt("w1", "v0.9.0", "ready", to(types.UpdateRunning)), nodeAt("w2", "v0.9.0", "ready", to(types.UpdateWaiting)), old, away},
		{control, nodeAt("w1", "v0.9.0", "down", to(types.UpdateRunning)), nodeAt("w2", "v0.9.0", "ready", to(types.UpdateWaiting)), old, away},
		{control, nodeAt("w1", "v0.9.1", "ready", nil), nodeAt("w2", "v0.9.0", "ready", to(types.UpdateRunning)), old, away},
		{control, nodeAt("w1", "v0.9.1", "ready", nil), nodeAt("w2", "v0.9.1", "ready", nil), old, away},
	}}
	var out bytes.Buffer
	c := &Context{Context: context.Background(), IO: IO{Stdout: &out, Stderr: &out}, flags: map[string]string{}}
	if err := updateNodes(c, serveFake(t, f)); err != nil {
		t.Fatal(err)
	}
	if f.asked != 1 {
		t.Errorf("asked the control node %d times, want once", f.asked)
	}
	got := out.String()
	for _, want := range []string{
		"-----> Updating the cluster's other nodes, one at a time\n",
		"       w1: updating from v0.9.0 to v0.9.1\n",
		"       w1: updated to v0.9.1 (",
		"       w2: updating from v0.9.0 to v0.9.1\n",
		"       w2: updated to v0.9.1 (",
		" !     old runs v0.8.4, which can't update itself: run sudo jokku update on old\n",
		"       away is down: it updates when it is back\n",
		"\n=====> 3 of 5 nodes run v0.9.1\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "w1: updating") != 1 {
		t.Errorf("w1's update is announced more than once:\n%s", got)
	}
}

func TestUpdateStopsAtAFailedNode(t *testing.T) {
	defer func(d time.Duration) { updatePoll = d }(updatePoll)
	updatePoll = time.Millisecond
	f := &fakeRollout{steps: [][]types.NodeView{
		{nodeAt("control", "v0.9.1", "ready", nil),
			nodeAt("w1", "v0.9.0", "ready", &types.NodeUpdate{To: "v0.9.1", State: types.UpdateFailed, Error: "update.sh failed, and jokku is still on v0.9.0"}),
			nodeAt("w2", "v0.9.0", "ready", &types.NodeUpdate{To: "v0.9.1", State: types.UpdateOnHold})},
	}}
	var out bytes.Buffer
	c := &Context{Context: context.Background(), IO: IO{Stdout: &out, Stderr: &out}, flags: map[string]string{}}
	err := updateNodes(c, serveFake(t, f))
	if e, ok := err.(*exitError); !ok || e.code != 1 {
		t.Fatalf("err = %v, want exit 1", err)
	}
	got := out.String()
	for _, want := range []string{
		" !     w1: the update failed: update.sh failed, and jokku is still on v0.9.0\n",
		" !     The update stopped at w1; the nodes after it wait. Once it is fixed, run sudo jokku update again to go on.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestNodeVersion(t *testing.T) {
	for _, c := range []struct {
		state, want string
	}{
		{"", "v0.9.0"},
		{types.UpdateRunning, "v0.9.0 (updating to v0.9.1)"},
		{types.UpdateWaiting, "v0.9.0 (v0.9.1 next)"},
		{types.UpdateFailed, "v0.9.0 (update to v0.9.1 failed)"},
		{types.UpdateOnHold, "v0.9.0 (update to v0.9.1 on hold)"},
		{types.UpdateByHand, "v0.9.0 (update to v0.9.1 by hand)"},
	} {
		n := types.NodeView{Version: "v0.9.0"}
		if c.state != "" {
			n.Update = &types.NodeUpdate{To: "v0.9.1", State: c.state}
		}
		if got := nodeVersion(n); got != c.want {
			t.Errorf("%s: %q, want %q", c.state, got, c.want)
		}
	}
}
