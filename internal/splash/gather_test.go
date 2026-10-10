package splash

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wes/jokku/internal/daemon"
	"github.com/wes/jokku/internal/types"
)

// fakeServer is a machine: its /proc, jokku's data directory, and the
// jokku daemon's socket, answered by handler (nil: no daemon).
func fakeServer(t *testing.T, handler http.HandlerFunc) Sources {
	t.Helper()
	dir, err := os.MkdirTemp("", "jks") // unix socket paths must stay short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	src := Sources{Proc: filepath.Join(dir, "proc"), DataDir: filepath.Join(dir, "data"),
		Socket: filepath.Join(dir, "api.sock"), Updates: filepath.Join(dir, "updates-available")}
	write(t, filepath.Join(src.Proc, "loadavg"), "0.18 0.15 0.10 1/523 4242\n")
	write(t, filepath.Join(src.Proc, "uptime"), "165060.25 1200000.10\n")
	write(t, filepath.Join(src.Proc, "meminfo"), "MemTotal:       30408704 kB\nMemFree:         1000000 kB\nMemAvailable:   26738688 kB\n")
	write(t, src.Updates, "\n12 updates can be applied immediately.\n3 of these updates are standard security updates.\nTo see these additional updates run: apt list --upgradable\n\n")
	if err := os.MkdirAll(src.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if handler != nil {
		ln, err := net.Listen("unix", src.Socket)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: handler}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
	}
	return src
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func TestGatherReadsTheMachineWithoutJokku(t *testing.T) {
	src := fakeServer(t, nil)
	in := Gather(context.Background(), src, time.Second)
	if in.Running || in.Role != "" || in.Cluster {
		t.Errorf("no daemon, yet running=%v role=%q cluster=%v", in.Running, in.Role, in.Cluster)
	}
	if !slices.Equal(in.Load, []float64{0.18, 0.15, 0.10}) {
		t.Errorf("load %v", in.Load)
	}
	if in.MemoryMB != 29696 || in.MemoryUsedMB != 29696-26112 {
		t.Errorf("memory %d of %d MB", in.MemoryUsedMB, in.MemoryMB)
	}
	if in.Uptime != 165060*time.Second {
		t.Errorf("uptime %s", in.Uptime)
	}
	if in.Updates != 12 || in.SecurityUpdates != 3 {
		t.Errorf("updates %d (%d security)", in.Updates, in.SecurityUpdates)
	}
	if in.DiskMB == 0 || in.Host == "" || in.CPUs == 0 {
		t.Errorf("disk %d MB, host %q, %d cpus", in.DiskMB, in.Host, in.CPUs)
	}

	// A control node whose daemon is stopped is still a control node.
	write(t, filepath.Join(src.DataDir, "jokku.db"), "")
	if in := Gather(context.Background(), src, time.Second); in.Role != "control" || in.Running {
		t.Errorf("stopped control node: role %q, running %v", in.Role, in.Running)
	}
}

func TestGatherAsksTheControlNode(t *testing.T) {
	deployed := now.Add(-2 * time.Hour)
	src := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/version":
			answer(w, types.Version{Version: "v0.8.3"})
		case "/v1/cluster/status":
			node := func(name, role, status string, instances int) types.NodeView {
				return types.NodeView{Node: types.Node{Name: name, Role: role, Status: status, MeshIP: "10.210.1.1"}, Instances: instances}
			}
			answer(w, types.ClusterStatus{
				Control: "jokku1",
				Nodes: []types.NodeView{node("jokku1", "control", "ready", 8), node("jokku2", "worker", "draining", 3),
					node("jokku4", "worker", "down", 0), node("edge1", "edge", "ready", 0)},
				Apps: []types.AppView{
					{Name: "blog", Healthy: 1, Wanted: 1, Deploy: &types.Deploy{App: "blog", Status: "succeeded", CreatedAt: deployed}},
					{Name: "api", Healthy: 0, Wanted: 2, Deploy: &types.Deploy{App: "api", CreatedAt: deployed.Add(-time.Hour)}},
					{Name: "old", Stopped: true},
					{Name: "pg-main", Kind: "postgres", Healthy: 1, Wanted: 1,
						Deploy: &types.Deploy{App: "pg-main", CreatedAt: deployed.Add(time.Hour)}},
				},
				// Two of jokku1's instances are an old release's, retiring.
				Instances: make([]types.InstanceView, 11),
				Totals:    types.ClusterTotals{Instances: 9},
			})
		default:
			http.NotFound(w, r)
		}
	})
	in := Gather(context.Background(), src, time.Second)
	if !in.Running || in.Role != "control" || in.Version != "v0.8.3" || !in.Cluster {
		t.Fatalf("running=%v role=%q version=%q cluster=%v", in.Running, in.Role, in.Version, in.Cluster)
	}
	if in.Status != "ready" || in.Mesh != "10.210.1.1" || in.Here != 8 || in.Instances != 11 {
		t.Errorf("this node: %q %q, %d of %d instances", in.Status, in.Mesh, in.Here, in.Instances)
	}
	// Draining is up; edges count apart.
	if in.Nodes != 3 || in.NodesUp != 2 || in.Edges != 1 || !slices.Equal(in.Down, []string{"jokku4"}) {
		t.Errorf("nodes %d of %d up, %d edges, down %v", in.NodesUp, in.Nodes, in.Edges, in.Down)
	}
	if in.Apps != 3 || in.Databases != 1 || in.Stopped != 1 {
		t.Errorf("%d apps, %d databases, %d stopped", in.Apps, in.Databases, in.Stopped)
	}
	if fmt.Sprint(in.Unhealthy) != "[{api 0 2}]" {
		t.Errorf("unhealthy %v", in.Unhealthy)
	}
	// The newest app deploy, not a database's.
	if in.LastDeploy == nil || in.LastDeploy.App != "blog" {
		t.Errorf("last deploy %+v", in.LastDeploy)
	}
}

func TestGatherOnAWorker(t *testing.T) {
	src := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/version" {
			w.WriteHeader(http.StatusConflict) // as a worker's daemon does
			return
		}
		answer(w, types.Version{Version: "v0.8.3"})
	})
	err := daemon.SaveNodeFile(src.DataDir, &daemon.NodeFile{Role: "worker", Control: "192.168.86.113:7443",
		Node: types.NodeIdentity{Name: "jokku3", MeshIP: "10.210.3.1"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(types.NodeState{
		Instances: []types.InstanceSpec{{ID: "a", Run: true}, {ID: "b", Run: true}, {ID: "c"}},
		Peers:     []types.Peer{{Name: "jokku1"}, {Name: "jokku2"}, {Name: "edge1", Role: "edge"}},
	})
	write(t, filepath.Join(src.DataDir, "agent-state.json"), string(b))

	in := Gather(context.Background(), src, time.Second)
	if in.Role != "worker" || !in.Running || in.Cluster || in.Control != "192.168.86.113" || in.Mesh != "10.210.3.1" {
		t.Errorf("role %q running %v cluster %v control %q mesh %q", in.Role, in.Running, in.Cluster, in.Control, in.Mesh)
	}
	if in.Here != 2 || in.Peers != 2 {
		t.Errorf("%d instances here, %d peers", in.Here, in.Peers)
	}
}

// TestASlowDaemonDoesntHoldUpTheLogin: the daemon gets the time it's given,
// then the splash goes on without it.
func TestASlowDaemonDoesntHoldUpTheLogin(t *testing.T) {
	src := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	start := time.Now()
	in := Gather(context.Background(), src, 200*time.Millisecond)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s", took)
	}
	if in.Running || in.Load == nil {
		t.Errorf("running %v, load %v", in.Running, in.Load)
	}
}
