// Package agent runs on every node, the control node included. It makes the
// node match the desired state the control node computes for it: it boots the
// instances it should hold, health-checks them, restarts crashes per the
// app's restart policy, stops and removes what it should no longer hold,
// keeps the WireGuard mesh and the proxy's routes current, and reports what
// it observes.
//
// The desired state is cached on disk, so a node keeps running (and serving
// traffic for) its last known state while the control node is unreachable.
// Every pass compares the desired state with the VMs actually running and
// fixes the difference, so crashes, restarts and reboots converge the same
// way.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wes/jokku/internal/dns"
	"github.com/wes/jokku/internal/guest"
	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/vm"
)

// ErrRemoved means the control node no longer knows this node.
var ErrRemoved = errors.New("this node was removed from the cluster")

// ControlPlane is how an agent reaches the control node: in-process on the
// control node, HTTPS everywhere else.
type ControlPlane interface {
	// State long-polls for this node's desired state, returning once it
	// differs from etag (or after a timeout).
	State(ctx context.Context, etag string) (*types.NodeState, error)
	Report(ctx context.Context, st *types.NodeStatus) error
	// Artifact downloads a root filesystem image.
	Artifact(ctx context.Context, name string, w io.Writer) error
}

// Runtime runs microVMs: vm.Host in production, a fake in tests.
type Runtime interface {
	Available() error
	EnsureNetwork(ctx context.Context) error
	Start(ctx context.Context, s vm.Spec) error
	Stop(ctx context.Context, id string, grace time.Duration) error
	Remove(ctx context.Context, id string) error
	Units(ctx context.Context) (map[string]string, error)
	Usage(ctx context.Context, ids []string) (map[string]vm.Usage, error)
	CheckTCP(addr string) bool

	// Volume disks: create an empty one, grow one that no VM has attached,
	// and list those attached to running VMs.
	CreateVolume(ctx context.Context, path string, sizeMB int) error
	GrowVolume(ctx context.Context, path string, sizeMB int) error
	Attached(ctx context.Context) (map[string]bool, error)

	// Session connects to a running VM's guest agent, returning the token
	// it expects.
	Session(ctx context.Context, id string) (io.ReadWriteCloser, string, error)
}

// Mesh keeps the WireGuard mesh matching the peer list.
type Mesh interface {
	Sync(ctx context.Context, self types.NodeIdentity, peers []types.Peer) error
}

// Proxy loads routes into this node's proxy.
type Proxy interface {
	Apply(ctx context.Context, ps *types.ProxyState) error
}

// Resolver hands the cluster's internal names to this node's DNS service,
// and says whether it answers at addr, the node's bridge address.
type Resolver interface {
	Apply(ctx context.Context, z dns.Zone) error
	Ready(addr string) bool
}

type Config struct {
	Control  ControlPlane
	Runtime  Runtime
	Mesh     Mesh     // nil: no mesh
	Proxy    Proxy    // nil: no proxy
	Resolver Resolver // nil: VMs use the host's resolvers
	DataDir  string
	DNS      []string // resolvers handed to guests
	Version  string
	Log      *slog.Logger
	// GCArtifacts deletes root filesystems no instance here uses. Off on
	// the control node, which keeps recent releases for rollbacks.
	GCArtifacts bool
	// Metrics measures the host; nil reads /proc.
	Metrics func() types.NodeMetrics
	// ReconcileEvery and ReportEvery default to 2s and 5s.
	ReconcileEvery, ReportEvery time.Duration
	// UpFor is how long an instance checked by staying up (no port) must
	// run to count as healthy; default 5s.
	UpFor time.Duration
}

// stopGrace is how long an app gets to exit after its stop signal, unless
// its spec says otherwise.
const stopGrace = 10 * time.Second

// stopMargin is how long the host waits beyond that, for the guest to
// unmount its volumes and power off.
const stopMargin = 5 * time.Second

type Agent struct {
	Config

	kick     chan struct{}
	report   chan struct{}
	stateMu  sync.Mutex
	desired  *types.NodeState
	mu       sync.Mutex // one reconcile pass at a time
	local    map[string]*local
	stopping sync.Map // instance ID -> struct{} while a graceful stop runs
	pulling  sync.Map // artifact name -> struct{} while downloading
	lastMesh string
	lastSync time.Time
	lastProx string
	lastGC   time.Time
	canRun   string
	errMu    sync.Mutex
	lastErr  map[string]string
	host     *hostSampler

	volMu        sync.Mutex
	vols         map[string]*vol
	exporting    sync.Map // volume ID -> struct{} while its final pass is being served
	backingUp    sync.Map // volume ID -> struct{} while it is being backed up
	volumeClient *http.Client
}

// local is what this node knows about an instance beyond the desired state.
type local struct {
	state       string
	healthyOnce bool
	restarts    int
	startedAt   time.Time
	removed     bool // files deleted
	cpuNanos    uint64
	cpuAt       time.Time
	cpuPercent  float64
	memoryMB    int
	stopWait    time.Duration // how long a stop may take, from the spec it started with
}

func New(c Config) *Agent {
	if c.ReconcileEvery == 0 {
		c.ReconcileEvery = 2 * time.Second
	}
	if c.ReportEvery == 0 {
		c.ReportEvery = 5 * time.Second
	}
	if c.UpFor == 0 {
		c.UpFor = 5 * time.Second
	}
	return &Agent{
		Config: c, kick: make(chan struct{}, 1), report: make(chan struct{}, 1),
		local: map[string]*local{}, lastErr: map[string]string{}, host: &hostSampler{},
		vols: map[string]*vol{},
		// No overall timeout: a first copy of a large disk takes a while.
		volumeClient: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: time.Minute}},
	}
}

// Run starts the agent and blocks until ctx is done.
func (a *Agent) Run(ctx context.Context) {
	if st, err := a.loadCachedState(); err == nil {
		a.setDesired(st, false)
		a.Log.Info("loaded the cached desired state", "instances", len(st.Instances))
	}
	if err := a.Runtime.Available(); err != nil {
		a.canRun = err.Error()
		a.Log.Warn("this node cannot run microVMs", "reason", err)
	}
	// The bridge carries the node's mesh address too, so set it up even
	// where VMs can't run.
	if err := a.Runtime.EnsureNetwork(ctx); err != nil {
		a.Log.Error("setting up the VM network", "err", err)
	}
	go a.watch(ctx)
	go a.reportLoop(ctx)
	t := time.NewTicker(a.ReconcileEvery)
	defer t.Stop()
	for {
		a.Reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.kick:
		}
	}
}

// Kick asks for a reconcile pass soon.
func (a *Agent) Kick() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

func (a *Agent) reportSoon() {
	select {
	case a.report <- struct{}{}:
	default:
	}
}

// watch long-polls the control node and applies every new desired state.
func (a *Agent) watch(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		etag := ""
		if st := a.Desired(); st != nil {
			etag = st.ETag
		}
		st, err := a.Control.State(ctx, etag)
		switch {
		case errors.Is(err, ErrRemoved):
			a.logOnce("state", err)
			a.setDesired(&types.NodeState{ETag: "removed"}, true)
			sleep(ctx, 30*time.Second)
			continue
		case err != nil:
			if ctx.Err() == nil {
				a.logOnce("state", fmt.Errorf("cannot reach the control node, running the last known state: %w", err))
			}
			sleep(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		a.clearErr("state")
		backoff = time.Second
		if st.ETag != etag {
			a.setDesired(st, true)
		}
	}
}

func (a *Agent) setDesired(st *types.NodeState, persist bool) {
	a.stateMu.Lock()
	a.desired = st
	a.stateMu.Unlock()
	if persist {
		if err := a.saveCachedState(st); err != nil {
			a.logOnce("cache", err)
		}
	}
	a.Kick()
}

// Desired is the state the agent is applying, or nil before the first one.
func (a *Agent) Desired() *types.NodeState {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.desired
}

func (a *Agent) cachePath() string { return filepath.Join(a.DataDir, "agent-state.json") }

func (a *Agent) loadCachedState() (*types.NodeState, error) {
	b, err := os.ReadFile(a.cachePath())
	if err != nil {
		return nil, err
	}
	var st types.NodeState
	return &st, json.Unmarshal(b, &st)
}

func (a *Agent) saveCachedState(st *types.NodeState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := a.cachePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.cachePath())
}

// Reconcile runs one pass.
func (a *Agent) Reconcile(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.reconcile(ctx); err != nil {
		a.logOnce("reconcile", err)
	} else {
		a.clearErr("reconcile")
	}
}

func (a *Agent) reconcile(ctx context.Context) error {
	st := a.Desired()
	if st == nil {
		return nil // nothing known yet
	}
	if st.Node.Name != "" && a.Mesh != nil {
		if err := a.syncMesh(ctx, st); err != nil {
			a.logOnce("mesh", err)
		} else {
			a.clearErr("mesh")
		}
	}
	if a.Proxy != nil && st.Proxy != nil {
		if err := a.syncProxy(ctx, st.Proxy); err != nil {
			a.logOnce("proxy", err)
		} else {
			a.clearErr("proxy")
		}
	}
	if a.Resolver != nil && st.Node.MeshIP != "" {
		if err := a.Resolver.Apply(ctx, dns.Zone{Listen: st.Node.MeshIP, Records: st.DNS}); err != nil {
			a.logOnce("dns", err)
		} else {
			a.clearErr("dns")
		}
	}
	if a.canRun != "" {
		return nil
	}

	units, err := a.Runtime.Units(ctx)
	if err != nil {
		return err
	}
	attached, err := a.Runtime.Attached(ctx)
	if err != nil {
		return err
	}
	a.syncVolumes(ctx, st, attached)
	now := time.Now()
	specs := map[string]types.InstanceSpec{}
	changed := false
	for _, spec := range st.Instances {
		specs[spec.ID] = spec
		l := a.local[spec.ID]
		if l == nil {
			l = &local{state: "pending"}
			// A VM that is already running (this agent restarted, or the
			// node's jokku was updated) is adopted, not started again:
			// it goes through checks like a fresh one.
			if _, running := units[spec.ID]; running {
				l.state, l.startedAt = "starting", now
			}
			a.local[spec.ID] = l
		}
		before := l.state
		a.step(ctx, spec, l, units, attached, now)
		if l.state != before {
			changed = true
		}
	}

	// Running VMs this node should no longer hold.
	for id := range units {
		if _, want := specs[id]; !want {
			a.stopAsync(id, true)
		}
	}
	// Forget instances that are gone and stopped.
	for id, l := range a.local {
		if _, want := specs[id]; want {
			continue
		}
		if _, running := units[id]; running {
			continue
		}
		if _, busy := a.stopping.Load(id); busy {
			continue
		}
		if !l.removed {
			if err := a.Runtime.Remove(ctx, id); err != nil {
				a.Log.Error("removing instance files", "id", id, "err", err)
				continue
			}
		}
		delete(a.local, id)
		changed = true
	}
	a.sampleUsage(ctx, units, now)
	if a.GCArtifacts && now.Sub(a.lastGC) > 10*time.Minute {
		a.lastGC = now
		a.gcArtifacts(st)
	}
	if changed {
		a.reportSoon()
	}
	return nil
}

// step advances one instance's state machine.
func (a *Agent) step(ctx context.Context, spec types.InstanceSpec, l *local, units map[string]string, attached map[string]bool, now time.Time) {
	_, running := units[spec.ID]
	if _, busy := a.stopping.Load(spec.ID); busy {
		return
	}
	if !spec.Run {
		if running {
			a.stopAsync(spec.ID, false)
		}
		l.state = "stopped"
		return
	}
	if l.state == "stopped" {
		l.state = "pending" // the app was started again
	}
	if !a.haveArtifact(spec) {
		l.state = "pulling"
		a.pull(spec)
		return
	}
	if l.state == "pulling" {
		l.state = "pending"
	}

	switch l.state {
	case "pending", "syncing":
		if !a.volumesReady(spec, l, attached) {
			return
		}
		a.start(ctx, spec, l, false)
	case "starting":
		switch {
		case !running && l.healthyOnce:
			a.Log.Warn("instance exited", "app", spec.App, "process", spec.Process)
			l.state = "crashed"
		case !running:
			a.Log.Warn("instance exited before passing checks", "app", spec.App, "process", spec.Process)
			l.state = "failed"
		case a.healthy(spec, l, now):
			l.state, l.healthyOnce = "healthy", true
		}
	case "healthy":
		if !running {
			a.Log.Warn("instance exited", "app", spec.App, "process", spec.Process)
			l.state = "crashed"
		}
	case "crashed":
		backoff := time.Duration(min(1<<min(l.restarts, 6), 60)) * time.Second
		switch {
		case spec.MaxRestarts == 0 || (spec.MaxRestarts > 0 && l.restarts >= spec.MaxRestarts):
			a.Log.Warn("not restarting instance (restart policy)", "app", spec.App, "process", spec.Process, "restarts", l.restarts)
			l.state = "failed"
		case now.Sub(l.startedAt) >= backoff:
			if wait, err := a.volumeGate(spec, attached); err != nil {
				a.Log.Error("cannot restart instance", "app", spec.App, "process", spec.Process, "err", err)
				l.state = "failed"
				return
			} else if wait != "" {
				return // its volume is being handed to another node; the control node decides what's next
			}
			a.start(ctx, spec, l, true)
		}
	case "failed":
		if running {
			a.stopAsync(spec.ID, false)
		}
	}
}

// volumesReady holds an instance back while one of its volumes is still
// being copied here (state syncing) or attached to another VM (pending, as
// when a deploy replaces the instance on the same node).
func (a *Agent) volumesReady(spec types.InstanceSpec, l *local, attached map[string]bool) bool {
	wait, err := a.volumeGate(spec, attached)
	switch {
	case err != nil:
		a.Log.Error("cannot start instance", "app", spec.App, "process", spec.Process, "err", err)
		l.state = "failed"
		return false
	case wait != "":
		l.state = wait
		return false
	}
	return true
}

func (a *Agent) healthy(spec types.InstanceSpec, l *local, now time.Time) bool {
	if spec.Check == types.CheckTCP || (spec.Check == "" && spec.ProcessType == "web") {
		return a.Runtime.CheckTCP(fmt.Sprintf("%s:%d", spec.IP, spec.Port))
	}
	return now.Sub(l.startedAt) >= a.UpFor
}

// stopTimeout is how long spec's app gets between its stop signal and
// SIGKILL.
func stopTimeout(spec types.InstanceSpec) time.Duration {
	if spec.StopTimeout > 0 {
		return time.Duration(spec.StopTimeout) * time.Second
	}
	return stopGrace
}

func (a *Agent) start(ctx context.Context, spec types.InstanceSpec, l *local, restart bool) {
	if len(spec.Argv) == 0 {
		a.Log.Error("instance has no command", "app", spec.App, "process", spec.Process)
		l.state = "failed"
		return
	}
	var vols []vm.Volume
	for _, m := range spec.Volumes {
		vols = append(vols, vm.Volume{Disk: a.volumePath(m.ID, ""), Path: m.Path})
	}
	// Internal names need this node's DNS service. Where it doesn't answer
	// (yet), the VM gets the host's resolvers: no internal names, but the
	// internet works.
	resolvers, search := a.DNS, []string(nil)
	if st := a.Desired(); a.Resolver != nil && st != nil && st.Node.MeshIP != "" && a.Resolver.Ready(st.Node.MeshIP) {
		resolvers, search = []string{st.Node.MeshIP}, []string{strings.ToLower(spec.App) + ".internal", "internal"}
	}
	err := a.Runtime.Start(ctx, vm.Spec{
		ID: spec.ID, App: spec.App, Process: spec.Process, Artifact: a.artifactPath(spec.Artifact),
		IP: spec.IP, CPUs: spec.CPUs, MemoryMB: spec.MemoryMB, Volumes: vols,
		Guest: guest.Config{
			Argv: spec.Argv, Env: spec.Env, User: spec.User, WorkDir: spec.WorkDir,
			Hostname: spec.Hostname, IP: spec.IP, DNS: resolvers, Search: search,
			StopTimeout: int(stopTimeout(spec) / time.Second), StopSignal: spec.StopSignal,
		},
	})
	if err != nil {
		a.Log.Error("starting instance", "app", spec.App, "process", spec.Process, "err", err)
		l.state = "failed"
		return
	}
	if restart {
		l.restarts++
	}
	l.state, l.startedAt, l.removed = "starting", time.Now(), false
	l.stopWait = stopTimeout(spec) + stopMargin
	l.cpuNanos, l.cpuAt = 0, time.Time{}
}

// stopAsync stops a VM gracefully in the background, then (when remove)
// deletes its files.
func (a *Agent) stopAsync(id string, remove bool) {
	if _, loaded := a.stopping.LoadOrStore(id, struct{}{}); loaded {
		return
	}
	wait := stopGrace + stopMargin
	if l := a.local[id]; l != nil && l.stopWait > 0 {
		wait = l.stopWait
	}
	go func() {
		defer a.stopping.Delete(id)
		ctx := context.Background()
		if err := a.Runtime.Stop(ctx, id, wait); err != nil {
			a.Log.Error("stopping instance", "id", id, "err", err)
			return
		}
		if remove {
			if err := a.Runtime.Remove(ctx, id); err != nil {
				a.Log.Error("removing instance files", "id", id, "err", err)
			} else {
				a.mu.Lock()
				if l := a.local[id]; l != nil {
					l.removed = true
				}
				a.mu.Unlock()
			}
		}
		a.Kick()
	}()
}

// Artifacts

func (a *Agent) artifactPath(name string) string {
	return filepath.Join(a.DataDir, "artifacts", filepath.Base(name))
}

func (a *Agent) haveArtifact(spec types.InstanceSpec) bool {
	_, err := os.Stat(a.artifactPath(spec.Artifact))
	return err == nil
}

// pull downloads a root filesystem in the background and verifies it before
// it can be used.
func (a *Agent) pull(spec types.InstanceSpec) {
	name := filepath.Base(spec.Artifact)
	if _, loaded := a.pulling.LoadOrStore(name, struct{}{}); loaded {
		return
	}
	go func() {
		defer a.pulling.Delete(name)
		if err := a.download(name, spec.ArtifactSHA256); err != nil {
			a.logOnce("pull "+name, err)
			time.Sleep(10 * time.Second) // don't hammer the control node
			return
		}
		a.clearErr("pull " + name)
		a.Log.Info("downloaded root filesystem", "artifact", name)
		a.Kick()
	}()
}

func (a *Agent) download(name, wantSHA string) error {
	dst := a.artifactPath(name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".pull-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	err = a.Control.Artifact(ctx, name, io.MultiWriter(tmp, h))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("downloading %s: %w", name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); wantSHA != "" && got != wantSHA {
		return fmt.Errorf("downloaded %s is corrupt (sha256 %s, want %s)", name, got, wantSHA)
	}
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (a *Agent) gcArtifacts(st *types.NodeState) {
	keep := map[string]bool{}
	for _, s := range st.Instances {
		keep[filepath.Base(s.Artifact)] = true
	}
	dir := filepath.Join(a.DataDir, "artifacts")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".ext4" && !keep[e.Name()] {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Mesh and proxy

func (a *Agent) syncMesh(ctx context.Context, st *types.NodeState) error {
	b, _ := json.Marshal([]any{st.Node, st.Peers})
	sum := sha256.Sum256(b)
	key := hex.EncodeToString(sum[:])
	// Re-apply every minute even without changes, to repair drift.
	if key == a.lastMesh && time.Since(a.lastSync) < time.Minute {
		return nil
	}
	if err := a.Mesh.Sync(ctx, st.Node, st.Peers); err != nil {
		return err
	}
	a.lastMesh, a.lastSync = key, time.Now()
	return nil
}

func (a *Agent) syncProxy(ctx context.Context, ps *types.ProxyState) error {
	b, _ := json.Marshal(ps)
	sum := sha256.Sum256(b)
	key := hex.EncodeToString(sum[:])
	if key == a.lastProx {
		return nil
	}
	if err := a.Proxy.Apply(ctx, ps); err != nil {
		return err
	}
	a.lastProx = key
	return nil
}

// Reporting

func (a *Agent) reportLoop(ctx context.Context) {
	t := time.NewTicker(a.ReportEvery)
	defer t.Stop()
	for {
		if err := a.Control.Report(ctx, a.Status()); err != nil {
			if ctx.Err() == nil && !errors.Is(err, ErrRemoved) {
				a.logOnce("report", err)
			}
		} else {
			a.clearErr("report")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.report:
		}
	}
}

// Status is the report sent to the control node.
func (a *Agent) Status() *types.NodeStatus {
	st := &types.NodeStatus{
		Protocol: types.ProtocolVersion, Version: a.Version, CanRun: a.canRun, Instances: []types.InstanceStatus{},
		Features: []string{types.FeatureVolumes}, Volumes: a.volumeStatus(),
	}
	if d := a.Desired(); d != nil {
		st.ETag = d.ETag
	}
	if a.Metrics != nil {
		st.Metrics = a.Metrics()
	} else {
		st.Metrics = a.host.sample(a.DataDir)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, l := range a.local {
		st.Instances = append(st.Instances, types.InstanceStatus{
			ID: id, State: l.state, HealthyOnce: l.healthyOnce, Restarts: l.restarts, StartedAt: l.startedAt,
			CPUPercent: l.cpuPercent, MemoryMB: l.memoryMB,
		})
	}
	return st
}

func (a *Agent) sampleUsage(ctx context.Context, units map[string]string, now time.Time) {
	var ids []string
	for id := range a.local {
		if _, running := units[id]; running {
			ids = append(ids, id)
		}
	}
	usage, err := a.Runtime.Usage(ctx, ids)
	if err != nil {
		return
	}
	for id, l := range a.local {
		u, ok := usage[id]
		if !ok {
			l.cpuPercent, l.memoryMB = 0, 0
			continue
		}
		if !l.cpuAt.IsZero() && u.CPUNanos >= l.cpuNanos {
			l.cpuPercent = float64(u.CPUNanos-l.cpuNanos) / float64(now.Sub(l.cpuAt).Nanoseconds()) * 100
		}
		l.cpuNanos, l.cpuAt, l.memoryMB = u.CPUNanos, now, int(u.MemoryBytes>>20)
	}
}

// logOnce logs an error unless it is the same as last time for what, so a
// persistent problem doesn't flood the journal.
func (a *Agent) logOnce(what string, err error) {
	a.errMu.Lock()
	defer a.errMu.Unlock()
	if msg := err.Error(); msg != a.lastErr[what] {
		a.lastErr[what] = msg
		a.Log.Error(what, "err", err)
	}
}

func (a *Agent) clearErr(what string) {
	a.errMu.Lock()
	defer a.errMu.Unlock()
	if a.lastErr[what] != "" {
		a.Log.Info(what + " recovered")
		delete(a.lastErr, what)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
