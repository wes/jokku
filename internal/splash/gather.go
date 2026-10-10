package splash

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/daemon"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/version"
)

// Sources are where Gather looks.
type Sources struct {
	Proc    string // /proc
	DataDir string // jokku's state, and the filesystem whose disk is shown
	Socket  string // the jokku daemon's API
	Updates string // update-notifier's note of waiting packages
}

var DefaultSources = Sources{
	Proc:    "/proc",
	DataDir: daemon.DefaultDataDir,
	Socket:  client.DefaultSocket,
	Updates: "/var/lib/update-notifier/updates-available",
}

// Gather reads this machine, and asks the jokku daemon about the cluster.
// It gives the daemon at most timeout, so a login is never held up for long;
// whatever it can't find out is left out.
func Gather(ctx context.Context, src Sources, timeout time.Duration) *Info {
	in := &Info{Now: time.Now(), CPUs: runtime.NumCPU(), Version: version.Version}
	host, _ := os.Hostname()
	in.Host, _, _ = strings.Cut(host, ".")
	in.IP = daemon.OutboundIP()
	readMachine(in, src)

	nf, err := daemon.LoadNodeFile(src.DataDir)
	if err == nil && (nf.Role == store.RoleWorker || nf.Role == store.RoleEdge) {
		in.Role, in.Mesh = nf.Role, nf.Node.MeshIP
		in.Control, _, _ = net.SplitHostPort(nf.Control)
		readNodeState(in, filepath.Join(src.DataDir, "agent-state.json"))
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	api := client.New(client.Target{Socket: src.Socket}, "splash")
	v, err := api.Version(ctx)
	if err != nil {
		if _, err := os.Stat(filepath.Join(src.DataDir, "jokku.db")); err == nil && in.Role == "" {
			in.Role = "control"
		}
		return in
	}
	in.Running, in.Version = true, v.Version
	if in.Role != "" {
		return in
	}
	in.Role = "control"
	if st, err := api.ClusterStatus(ctx); err == nil {
		readCluster(in, st)
	}
	return in
}

// readCluster takes the figures from the control node's status.
func readCluster(in *Info, st *types.ClusterStatus) {
	in.Cluster = true
	for _, n := range st.Nodes {
		if n.Name == st.Control {
			in.Status, in.Mesh, in.Here = n.Status, n.MeshIP, n.Instances
		}
		if n.Status == "down" {
			in.Down = append(in.Down, n.Name)
		}
		if n.Role == store.RoleEdge {
			in.Edges++
			continue
		}
		in.Nodes++
		if n.Status != "down" {
			in.NodesUp++
		}
	}
	for _, a := range st.Apps {
		if a.Kind != "" {
			in.Databases++
		} else {
			in.Apps++
			if a.Stopped {
				in.Stopped++
			}
		}
		if !a.Stopped && a.Healthy < a.Wanted {
			in.Unhealthy = append(in.Unhealthy, AppHealth{Name: a.Name, Healthy: a.Healthy, Wanted: a.Wanted})
		}
		if d := a.Deploy; d != nil && a.Kind == "" && (in.LastDeploy == nil || d.CreatedAt.After(in.LastDeploy.CreatedAt)) {
			in.LastDeploy = d
		}
	}
	// Every instance the nodes hold, counted the way each node's own count
	// is (and nodes:list's), so this node's share is never more than all:
	// an old release's instances winding down after a deploy count too.
	in.Instances = len(st.Instances)
}

// readNodeState counts, on a worker or an edge, what the control node last
// told it: its instances, the other nodes, and the apps it routes.
func readNodeState(in *Info, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var st types.NodeState
	if json.Unmarshal(b, &st) != nil {
		return
	}
	for _, s := range st.Instances {
		if s.Run {
			in.Here++
		}
	}
	for _, p := range st.Peers {
		if p.Role != store.RoleEdge {
			in.Peers++
		}
	}
	if st.Proxy != nil {
		var apps []string
		for _, r := range st.Proxy.Routes {
			if !slices.Contains(apps, r.App) {
				apps = append(apps, r.App)
			}
		}
		in.Routes = len(apps)
	}
}

func readMachine(in *Info, src Sources) {
	if b, err := os.ReadFile(filepath.Join(src.Proc, "loadavg")); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 3 {
			for _, s := range f[:3] {
				v, _ := strconv.ParseFloat(s, 64)
				in.Load = append(in.Load, v)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(src.Proc, "meminfo")); err == nil {
		var total, avail int
		for _, line := range strings.Split(string(b), "\n") {
			k, v, _ := strings.Cut(line, ":")
			kb, _ := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")))
			switch k {
			case "MemTotal":
				total = kb / 1024
			case "MemAvailable":
				avail = kb / 1024
			}
		}
		in.MemoryMB, in.MemoryUsedMB = total, total-avail
	}
	if b, err := os.ReadFile(filepath.Join(src.Proc, "uptime")); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			s, _ := strconv.ParseFloat(f[0], 64)
			in.Uptime = time.Duration(s) * time.Second
		}
	}
	dir := src.DataDir
	if _, err := os.Stat(dir); err != nil {
		dir = "/"
	}
	in.DiskMB, in.DiskUsedMB, in.DiskAvailMB = disk(dir)
	if b, err := os.ReadFile(src.Updates); err == nil {
		in.Updates, in.SecurityUpdates = updates(string(b))
	}
}

var (
	updatesRe  = regexp.MustCompile(`(?m)^(\d+) updates? can be applied`)
	securityRe = regexp.MustCompile(`(?m)^(\d+) of these updates? (?:is|are)(?: a)? standard security`)
)

// updates reads Ubuntu's update-notifier note, which apt refreshes:
//
//	12 updates can be applied immediately.
//	3 of these updates are standard security updates.
func updates(note string) (all, security int) {
	if m := updatesRe.FindStringSubmatch(note); m != nil {
		all, _ = strconv.Atoi(m[1])
	}
	if m := securityRe.FindStringSubmatch(note); m != nil {
		security, _ = strconv.Atoi(m[1])
	}
	return all, security
}
