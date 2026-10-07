// Package agent makes this node match the desired state in the store: it
// boots instances that should run, health-checks them, restarts crashes per
// the app's restart policy, retires old instances after deploys and keeps the
// proxy's routes current.
//
// Every pass is idempotent: it compares the store with the VM units that are
// actually running and fixes the difference, so crashes, daemon restarts and
// reboots all converge the same way.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wes/jokku/internal/guest"
	"github.com/wes/jokku/internal/props"
	"github.com/wes/jokku/internal/proxy"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/vm"
)

// stopGrace is how long an app gets to exit after SIGTERM.
const stopGrace = 10 * time.Second

type Config struct {
	Store   *store.Store
	Host    *vm.Host
	Node    string
	DataDir string
	DNS     []string // resolvers handed to guests
	Log     *slog.Logger
}

type Agent struct {
	Config
	kick      chan struct{}
	mu        sync.Mutex // one pass at a time
	stopping  sync.Map   // instance ID -> struct{}, while a graceful stop runs
	lastProxy []byte
	lastErr   string
}

func New(c Config) *Agent {
	return &Agent{Config: c, kick: make(chan struct{}, 1)}
}

// Available reports why this node cannot run microVMs, or nil.
func (a *Agent) Available() error { return a.Host.Available() }

// Run reconciles every two seconds, and right away when kicked.
func (a *Agent) Run(ctx context.Context) {
	if err := a.Host.EnsureNetwork(ctx); err != nil {
		a.Log.Error("setting up the VM network", "err", err)
	}
	t := time.NewTicker(2 * time.Second)
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

// Kick asks for a pass soon.
func (a *Agent) Kick() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// Reconcile runs one pass.
func (a *Agent) Reconcile(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.reconcile(ctx); err != nil {
		a.logOnce("reconcile", err)
	}
}

func (a *Agent) reconcile(ctx context.Context) error {
	insts, err := a.Store.Instances(ctx, "")
	if err != nil {
		return err
	}
	apps, err := a.Store.Apps(ctx)
	if err != nil {
		return err
	}
	stoppedApp := map[string]bool{}
	for _, app := range apps {
		stoppedApp[app.Name] = app.Stopped
	}
	units, err := a.Host.Units(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	releases := map[int64]*store.Release{}
	known := map[string]bool{}

	for _, in := range insts {
		if in.Node != a.Node {
			continue
		}
		known[in.ID] = true
		_, running := units[in.ID]
		if _, busy := a.stopping.Load(in.ID); busy {
			continue
		}
		if !shouldRun(in, stoppedApp[in.App], now) {
			switch {
			case running:
				a.stopAsync(in.ID, in.Desired == store.DesiredStopped)
			case in.Desired == store.DesiredStopped:
				a.remove(ctx, in.ID)
			case in.State != store.StatePending:
				// The app was stopped: start fresh on ps:start.
				a.Store.SetInstanceState(ctx, in.ID, store.StatePending)
			}
			continue
		}

		switch in.State {
		case store.StatePending:
			a.start(ctx, in, releases, false)
		case store.StateStarting:
			switch {
			case !running && in.HealthyOnce:
				a.crashed(ctx, in)
			case !running:
				a.Log.Warn("instance exited before passing checks", "app", in.App, "process", in.Name())
				a.Store.SetInstanceState(ctx, in.ID, store.StateFailed)
			case healthy(in):
				a.Store.SetInstanceState(ctx, in.ID, store.StateHealthy)
			}
		case store.StateHealthy:
			if !running {
				a.crashed(ctx, in)
			}
		case store.StateCrashed:
			max, ok := a.restartPolicy(ctx, in.App)
			backoff := time.Duration(min(1<<min(in.Restarts, 6), 60)) * time.Second
			switch {
			case !ok || (max >= 0 && in.Restarts >= max):
				a.Log.Warn("not restarting instance (restart policy)", "app", in.App, "process", in.Name(), "restarts", in.Restarts)
				a.Store.SetInstanceState(ctx, in.ID, store.StateFailed)
			case now.Sub(in.StartedAt) >= backoff:
				a.start(ctx, in, releases, true)
			}
		case store.StateFailed:
			if running {
				a.stopAsync(in.ID, false)
			}
		}
	}

	// VMs the store no longer knows about (destroyed apps, lost rows).
	for id := range units {
		if !known[id] {
			if _, busy := a.stopping.Load(id); !busy {
				a.stopAsync(id, true)
			}
		}
	}
	return a.syncProxy(ctx, insts)
}

// shouldRun: wanted, or retiring but still inside its wait-to-retire window.
func shouldRun(in store.Instance, appStopped bool, now time.Time) bool {
	if appStopped {
		return false
	}
	if in.Desired == store.DesiredRunning {
		return true
	}
	return in.RetireAt != nil && now.Before(*in.RetireAt)
}

func healthy(in store.Instance) bool {
	if in.ProcessType == "web" {
		return vm.CheckTCP(fmt.Sprintf("%s:%d", in.IP, in.Port))
	}
	return time.Since(in.StartedAt) >= 5*time.Second
}

func (a *Agent) crashed(ctx context.Context, in store.Instance) {
	a.Log.Warn("instance exited", "app", in.App, "process", in.Name())
	a.Store.SetInstanceState(ctx, in.ID, store.StateCrashed)
}

// restartPolicy returns the maximum restarts (-1 for unlimited) and whether
// crashed instances are restarted at all.
func (a *Agent) restartPolicy(ctx context.Context, app string) (int, bool) {
	global, _ := a.Store.Properties(ctx, "", "ps")
	appProps, _ := a.Store.Properties(ctx, app, "ps")
	p, _ := props.Lookup("ps")
	policy := props.Compute(p, appProps, global)["restart-policy"]
	switch {
	case policy == "no":
		return 0, false
	case policy == "always" || policy == "unless-stopped" || policy == "on-failure":
		return -1, true
	}
	if n, ok := strings.CutPrefix(policy, "on-failure:"); ok {
		if max, err := strconv.Atoi(n); err == nil {
			return max, true
		}
	}
	return 10, true
}

func (a *Agent) start(ctx context.Context, in store.Instance, releases map[int64]*store.Release, restart bool) {
	rel := releases[in.ReleaseID]
	if rel == nil {
		var err error
		if rel, err = a.Store.Release(ctx, in.ReleaseID); err != nil {
			a.Log.Error("loading release", "app", in.App, "err", err)
			return
		}
		releases[in.ReleaseID] = rel
	}
	argv := rel.Processes[in.ProcessType]
	if len(argv) == 0 {
		a.Log.Error("release has no such process type", "app", in.App, "process", in.ProcessType)
		a.Store.SetInstanceState(ctx, in.ID, store.StateFailed)
		return
	}
	spec := vm.Spec{
		ID: in.ID, App: in.App, Process: in.Name(), Artifact: rel.Artifact,
		IP: in.IP, CPUs: in.CPUs, MemoryMB: in.MemoryMB,
		Guest: guest.Config{
			Argv:        argv,
			Env:         Env(rel, in),
			User:        rel.Image.User,
			WorkDir:     rel.Image.WorkingDir,
			Hostname:    Hostname(in),
			IP:          in.IP,
			DNS:         a.DNS,
			StopTimeout: int(stopGrace / time.Second),
		},
	}
	if err := a.Host.Start(ctx, spec); err != nil {
		a.Log.Error("starting instance", "app", in.App, "process", in.Name(), "err", err)
		a.Store.SetInstanceState(ctx, in.ID, store.StateFailed)
		return
	}
	a.Store.InstanceStarted(ctx, in.ID, restart)
}

// Env is the process environment: the image's, then config vars, then what
// Jokku sets.
func Env(rel *store.Release, in store.Instance) []string {
	env := append([]string(nil), rel.Image.Env...)
	keys := make([]string, 0, len(rel.ConfigVars))
	for k := range rel.ConfigVars {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		env = append(env, k+"="+rel.ConfigVars[k])
	}
	return append(env,
		"PORT="+strconv.Itoa(in.Port),
		"DYNO="+in.Name(),
		"JOKKU_APP_NAME="+in.App,
		"JOKKU_PROCESS_TYPE="+in.ProcessType,
	)
}

// Hostname is <app>-<type>-<n>, a valid DNS label.
func Hostname(in store.Instance) string {
	h := strings.ToLower(fmt.Sprintf("%s-%s-%d", in.App, in.ProcessType, in.Index))
	h = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, h)
	if len(h) > 63 {
		h = h[:63]
	}
	return strings.Trim(h, "-")
}

// stopAsync stops a VM gracefully in the background, then removes its files
// and (when remove) its row.
func (a *Agent) stopAsync(id string, remove bool) {
	if _, loaded := a.stopping.LoadOrStore(id, struct{}{}); loaded {
		return
	}
	go func() {
		defer a.stopping.Delete(id)
		ctx := context.Background()
		if err := a.Host.Stop(ctx, id, stopGrace); err != nil {
			a.Log.Error("stopping instance", "id", id, "err", err)
			return
		}
		if remove {
			a.remove(ctx, id)
		}
		a.Kick()
	}()
}

func (a *Agent) remove(ctx context.Context, id string) {
	if err := a.Host.Remove(ctx, id); err != nil {
		a.Log.Error("removing instance files", "id", id, "err", err)
	}
	a.Store.DeleteInstance(ctx, id)
}

// syncProxy loads routes for every deployed app into the proxy when they
// changed: its domains to the healthy web instances of its current release.
func (a *Agent) syncProxy(ctx context.Context, insts []store.Instance) error {
	apps, err := a.Store.Apps(ctx)
	if err != nil {
		return err
	}
	le, _ := props.Lookup("letsencrypt")
	px, _ := props.Lookup("proxy")
	globalLE, _ := a.Store.Properties(ctx, "", "letsencrypt")
	globalProxy, _ := a.Store.Properties(ctx, "", "proxy")
	now := time.Now()

	var routes []proxy.Route
	for _, app := range apps {
		if app.CurrentRelease == 0 {
			continue
		}
		appProxy, _ := a.Store.Properties(ctx, app.Name, "proxy")
		if props.Compute(px, appProxy, globalProxy)["enabled"] != "true" {
			continue
		}
		cur, err := a.Store.CurrentRelease(ctx, app.Name)
		if err != nil || cur == nil {
			continue
		}
		domains, err := a.Store.Domains(ctx, app.Name)
		if err != nil {
			return err
		}
		var upstreams []string
		for _, in := range insts {
			if in.App == app.Name && in.ReleaseID == cur.ID && in.ProcessType == "web" &&
				in.State == store.StateHealthy && shouldRun(in, app.Stopped, now) {
				upstreams = append(upstreams, fmt.Sprintf("%s:%d", in.IP, in.Port))
			}
		}
		appLE, _ := a.Store.Properties(ctx, app.Name, "letsencrypt")
		tlsOn := props.Compute(le, appLE, globalLE)["enabled"] == "true"
		plain, secure := proxy.Route{App: app.Name, Upstreams: upstreams}, proxy.Route{App: app.Name, Upstreams: upstreams, TLS: true}
		for _, d := range domains {
			if tlsOn && proxy.PublicHost(d) {
				secure.Hosts = append(secure.Hosts, d)
			} else {
				plain.Hosts = append(plain.Hosts, d)
			}
		}
		routes = append(routes, plain, secure)
	}
	cfg, err := proxy.Config(proxy.Settings{
		Routes: routes, Email: globalLE["email"], DataDir: a.DataDir, HTTPPort: 80, HTTPSPort: 443,
	})
	if err != nil {
		return err
	}
	if bytes.Equal(cfg, a.lastProxy) {
		return nil
	}
	if err := proxy.Load(ctx, proxy.AdminSocket, a.DataDir, cfg); err != nil {
		return err
	}
	a.lastProxy = cfg
	return nil
}

// logOnce logs an error unless it is the same as last time, so a persistent
// problem doesn't flood the journal every two seconds.
func (a *Agent) logOnce(what string, err error) {
	if msg := err.Error(); msg != a.lastErr {
		a.lastErr = msg
		a.Log.Error(what, "err", err)
	}
}

// Logs streams an app's log lines, formatted the way "dokku logs" prints them.
func Logs(ctx context.Context, app string, o types.LogOptions, line func(string)) error {
	// _TRANSPORT=stdout keeps only what the VM printed, not systemd's own
	// messages about the unit.
	args := []string{"--no-pager", "--output=json", "--lines=" + strconv.Itoa(o.Tail), "JOKKU_APP=" + app, "_TRANSPORT=stdout"}
	if o.Follow {
		args = append(args, "--follow")
	}
	switch {
	case strings.Contains(o.Process, "."):
		args = append(args, "JOKKU_PROCESS="+o.Process)
	case o.Process != "":
		args = append(args, "JOKKU_PROCESS_TYPE="+o.Process)
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	err = forEachEntry(out, func(e journalEntry) {
		line(e.format())
	})
	cmd.Wait()
	if ctx.Err() != nil {
		return nil // the client went away while following
	}
	return err
}

// InstanceLogs returns the last n lines an instance printed, for deploy
// failure messages.
func InstanceLogs(ctx context.Context, id string, n int) []string {
	out, _ := exec.CommandContext(ctx, "journalctl", "--no-pager", "--output=cat", "--lines="+strconv.Itoa(n), "JOKKU_INSTANCE="+id, "_TRANSPORT=stdout").Output()
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
