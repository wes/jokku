// Package deploy turns source into releases and rolls them out:
//
//	inspect source -> build rootfs -> release -> rollout
//
// A rollout is Dokku's zero-downtime deploy: boot the new release's
// instances, wait for them to pass checks, switch the proxy to them, and stop
// the old instances after wait-to-retire. If checks fail, the new instances
// are stopped and the old release keeps serving.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wes/jokku/internal/build"
	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/props"
	"github.com/wes/jokku/internal/proxy"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

type Pipeline struct {
	Store   *store.Store
	Builder *build.Builder
	Cluster *cluster.Controller
	DataDir string
	Log     *slog.Logger
}

// keepReleases is how many recent releases per app keep their rootfs on
// disk, for restarts and rollbacks.
const keepReleases = 3

func (p *Pipeline) Deploy(ctx context.Context, d *types.Deploy, sourcePath string, log func(string)) error {
	p.Store.AddEvent(ctx, "deploy", d.App, "", "deploy started (%s)", d.Source)
	version, err := p.deploy(ctx, d, sourcePath, log)
	if err != nil {
		p.Store.AddEvent(ctx, "deploy", d.App, "", "deploy failed: %v", err)
		return err
	}
	p.Store.AddEvent(ctx, "deploy", d.App, "", "deployed v%d", version)
	return nil
}

func (p *Pipeline) deploy(ctx context.Context, d *types.Deploy, sourcePath string, log func(string)) (int, error) {
	settings, err := p.settings(ctx, d.App)
	if err != nil {
		return 0, err
	}
	src, err := Inspect(sourcePath, settings)
	if err != nil {
		return 0, err
	}
	if err := p.Cluster.CanSchedule(ctx); err != nil {
		return 0, err
	}
	p.Store.SetDeployStatus(ctx, d.ID, "building", "")
	log("-----> Building " + d.App + " from " + src.Dockerfile)
	res, err := p.Builder.Build(ctx, build.Options{
		App: d.App, DeployID: d.ID, Source: sourcePath,
		BuildDir: settings.BuildDir, DockerfilePath: settings.DockerfilePath,
	}, log)
	if err != nil {
		return 0, err
	}
	procs, err := Processes(src.Procfile, res.Entrypoint, res.Cmd)
	if err != nil {
		return 0, err
	}
	if len(src.Procfile) > 0 {
		log("-----> Process types from " + settings.ProcfilePath + ": " + strings.Join(sortedProcessTypes(src.Procfile), ", "))
	}
	vars, err := p.configVars(ctx, d.App)
	if err != nil {
		return 0, err
	}
	description := "Deploy"
	if d.SourceRef != "" {
		description += " " + shortRef(d.SourceRef)
	}
	rel := &store.Release{
		App: d.App, Artifact: res.Artifact, ArtifactSHA: res.SHA256, ArtifactLen: res.Size,
		Processes: procs, ConfigVars: vars, Description: description,
		Image: store.ImageConfig{Env: res.Env, WorkingDir: res.WorkingDir, User: res.User, Port: res.Port},
	}
	if err := p.Store.CreateRelease(ctx, rel); err != nil {
		return 0, err
	}
	p.Store.SetDeployRelease(ctx, d.ID, rel.ID)
	from := res.PortFrom
	if rel.Port() != res.Port {
		from = "from the PORT config var"
	}
	log(fmt.Sprintf("-----> $PORT is %d, %s", rel.Port(), from))
	if err := p.defaultDomains(ctx, d.App); err != nil {
		return 0, err
	}

	p.Store.SetDeployStatus(ctx, d.ID, "deploying", "")
	if err := p.rollout(ctx, rel, log); err != nil {
		return 0, err
	}
	p.cleanup(ctx, d)
	return rel.Version, p.printURLs(ctx, d.App, log)
}

// Processes maps each process type to its argv. Without a Procfile, the app
// has one "web" process: the image's ENTRYPOINT + CMD. Procfile commands run
// through /bin/sh (so $PORT expands), after the ENTRYPOINT if there is one,
// the way "docker run image <command>" would.
func Processes(procfile map[string]string, entrypoint, cmd []string) (map[string][]string, error) {
	if len(procfile) == 0 {
		argv := append(append([]string{}, entrypoint...), cmd...)
		if len(argv) == 0 {
			return nil, errors.New("the image has no ENTRYPOINT or CMD and there is no Procfile, so there is nothing to run")
		}
		return map[string][]string{"web": argv}, nil
	}
	out := map[string][]string{}
	for name, command := range procfile {
		out[name] = append(append([]string{}, entrypoint...), "/bin/sh", "-c", command)
	}
	return out, nil
}

// PS runs a "ps:" action: restart, start, stop or rebuild.
func (p *Pipeline) PS(ctx context.Context, app, action, actor string, log func(string)) error {
	if _, err := p.Store.App(ctx, app); err != nil {
		return err
	}
	switch action {
	case "stop":
		return p.stop(ctx, app, log)
	case "rebuild":
		return p.rebuild(ctx, app, actor, log)
	case "start", "restart":
		if err := p.Cluster.CanSchedule(ctx); err != nil {
			return err
		}
		return p.restart(ctx, app, log)
	}
	return fmt.Errorf("unknown action %q", action)
}

// restart rolls out the current release again, picking up config, scale and
// resource changes. Changed config vars make a new release.
func (p *Pipeline) restart(ctx context.Context, app string, log func(string)) error {
	cur, err := p.Store.CurrentRelease(ctx, app)
	if err != nil {
		return err
	}
	if cur == nil {
		return fmt.Errorf("%s has not been deployed yet: git push to deploy it", app)
	}
	vars, err := p.configVars(ctx, app)
	if err != nil {
		return err
	}
	rel := cur
	if !maps.Equal(vars, cur.ConfigVars) {
		next := *cur
		next.ConfigVars, next.Description = vars, "Config change"
		if err := p.Store.CreateRelease(ctx, &next); err != nil {
			return err
		}
		rel = &next
	}
	log(fmt.Sprintf("-----> Restarting %s (v%d)", app, rel.Version))
	if err := p.rollout(ctx, rel, log); err != nil {
		p.Store.AddEvent(ctx, "deploy", app, "", "restart failed: %v", err)
		return err
	}
	p.Store.AddEvent(ctx, "deploy", app, "", "restarted (v%d)", rel.Version)
	return p.printURLs(ctx, app, log)
}

func (p *Pipeline) stop(ctx context.Context, app string, log func(string)) error {
	log("-----> Stopping " + app)
	if err := p.Store.SetAppStopped(ctx, app, true); err != nil {
		return err
	}
	p.Store.AddEvent(ctx, "deploy", app, "", "stopped")
	p.Cluster.Changed()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		insts, err := p.Store.Instances(ctx, app)
		if err != nil {
			return err
		}
		nodes, err := p.readyNodes(ctx)
		if err != nil {
			return err
		}
		left := 0
		for _, in := range insts {
			if in.State != store.StateStopped && nodes[in.Node] && in.Desired == store.DesiredRunning {
				left++
			}
		}
		if left == 0 {
			log("-----> Stopped " + app)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("some instances are still stopping; check jokku ps:report " + app)
}

func (p *Pipeline) readyNodes(ctx context.Context) (map[string]bool, error) {
	nodes, err := p.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	ready := map[string]bool{}
	for _, n := range nodes {
		ready[n.Name] = n.Ready(now)
	}
	return ready, nil
}

// rebuild deploys the most recently deployed source again.
func (p *Pipeline) rebuild(ctx context.Context, app, actor string, log func(string)) error {
	deploys, err := p.Store.Deploys(ctx, app, 100)
	if err != nil {
		return err
	}
	for _, prev := range deploys {
		if prev.Status != types.StatusSucceeded {
			continue
		}
		source := filepath.Join(p.DataDir, "builds", strconv.FormatInt(prev.ID, 10), "source.tar")
		if _, err := os.Stat(source); err != nil {
			continue
		}
		d, err := p.Store.CreateDeploy(ctx, app, "rebuild", prev.SourceRef, actor)
		if err != nil {
			return err
		}
		dst := filepath.Join(p.DataDir, "builds", strconv.FormatInt(d.ID, 10), "source.tar")
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.Link(source, dst); err != nil {
			return err
		}
		err = p.Deploy(ctx, d, dst, log)
		status, msg := types.StatusSucceeded, ""
		if err != nil {
			status, msg = types.StatusFailed, err.Error()
		}
		p.Store.SetDeployStatus(context.WithoutCancel(ctx), d.ID, status, msg)
		return err
	}
	return fmt.Errorf("no source to rebuild %s from: git push to deploy it", app)
}

// rollout boots rel's formation, waits for checks, switches traffic and
// schedules the old instances' retirement.
func (p *Pipeline) rollout(ctx context.Context, rel *store.Release, log func(string)) error {
	app := rel.App
	before, err := p.Store.Instances(ctx, app)
	if err != nil {
		return err
	}
	scaled, err := p.Store.Formation(ctx, app)
	if err != nil {
		return err
	}
	quantity := map[string]int{}
	for _, proc := range scaled {
		quantity[proc.Type] = proc.Quantity
	}
	for _, proc := range sortedProcessTypes(rel.Processes) {
		if _, ok := quantity[proc]; !ok && proc == "web" {
			quantity[proc] = 1 // as in Dokku: one web process unless scaled
		}
	}
	sizes, err := p.Store.Resources(ctx, app)
	if err != nil {
		return err
	}
	disks, err := p.disks(ctx, app, rel, quantity)
	if err != nil {
		return err
	}

	// Instances that are not serving (a stopped app's, crashed ones) are
	// replaced outright; serving ones retire once the new ones pass checks.
	// A process type with local volumes can't overlap: its one instance
	// holds the disks, so the old instance stops first.
	var stale, serving []string
	var first []store.Instance
	for _, in := range before {
		switch {
		case in.Desired != store.DesiredRunning:
		case len(disks[in.ProcessType].mounts) > 0:
			first = append(first, in)
			stale = append(stale, in.ID)
		case in.State == store.StateHealthy:
			serving = append(serving, in.ID)
		default:
			stale = append(stale, in.ID)
		}
	}
	for _, in := range first {
		log(fmt.Sprintf("-----> Stopping %s first: volume %s can only be attached to one instance at a time",
			in.Name(), disks[in.ProcessType].names()))
	}
	if err := p.Store.StopInstances(ctx, stale, nil); err != nil {
		return err
	}
	if err := p.Store.SetAppStopped(ctx, app, false); err != nil {
		return err
	}
	fail := func(created []store.Instance, err error) error {
		p.abandon(ctx, created)
		p.restore(ctx, first)
		return err
	}

	var created []store.Instance
	var names []string
	for _, proc := range sortedProcessTypes(rel.Processes) {
		size := sizes.Effective(proc)
		d := disks[proc]
		for i := 1; i <= quantity[proc]; i++ {
			node, subnet, err := p.Cluster.Place(ctx, cluster.Placement{
				App: app, ProcessType: proc, CPUs: size.CPUs, MemoryMB: size.MemoryMB,
				Pin: d.node, PinReason: d.pinReason, Volumes: len(d.mounts) > 0,
			})
			if err != nil {
				return fail(created, err)
			}
			for _, v := range d.unplaced {
				if err := p.Store.PlaceVolume(ctx, v.ID, node); err != nil {
					return fail(created, err)
				}
			}
			in := store.Instance{
				App: app, ReleaseID: rel.ID, ProcessType: proc, Index: i, Node: node, Port: rel.Port(),
				CPUs: size.CPUs, MemoryMB: size.MemoryMB, Desired: store.DesiredRunning, Volumes: d.mounts,
			}
			if err := p.Store.CreateInstance(ctx, &in, subnet); err != nil {
				return fail(created, err)
			}
			created = append(created, in)
			names = append(names, fmt.Sprintf("%s on %s (%d vCPU, %s)", in.Name(), node, in.CPUs, formatMB(in.MemoryMB)))
		}
	}
	if len(created) > 0 {
		log("-----> Starting " + strings.Join(names, ", "))
		p.Cluster.Changed()
		if err := p.waitHealthy(ctx, app, created, log); err != nil {
			return fail(created, err)
		}
	} else {
		log("-----> No processes are scaled up (jokku ps:scale " + app + " web=1)")
	}

	if err := p.Store.SetCurrentRelease(ctx, app, rel.ID); err != nil {
		return err
	}
	checks, err := p.computed(ctx, app, "checks")
	if err != nil {
		return err
	}
	wait, _ := strconv.Atoi(checks["wait-to-retire"])
	retireAt := time.Now().Add(time.Duration(wait) * time.Second)
	if err := p.Store.StopInstances(ctx, serving, &retireAt); err != nil {
		return err
	}
	p.Cluster.Changed() // switch every proxy to the new instances now
	if len(serving) > 0 {
		log(fmt.Sprintf("-----> Old instances will shut down in %d seconds", wait))
	}
	return nil
}

// waitHealthy follows new instances until all pass checks, one fails, or the
// checks timeout passes.
func (p *Pipeline) waitHealthy(ctx context.Context, app string, created []store.Instance, log func(string)) error {
	checks, err := p.computed(ctx, app, "checks")
	if err != nil {
		return err
	}
	secs, _ := strconv.Atoi(checks["timeout"])
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	seen := map[string]string{}
	for {
		insts, err := p.Store.Instances(ctx, app)
		if err != nil {
			return err
		}
		byID := map[string]store.Instance{}
		for _, in := range insts {
			byID[in.ID] = in
		}
		var waiting *store.Instance
		for _, c := range created {
			in, ok := byID[c.ID]
			if !ok {
				return fmt.Errorf("%s was removed during the deploy", c.Name())
			}
			if in.State != seen[in.ID] {
				seen[in.ID] = in.State
				if in.State == store.StateHealthy {
					log("       " + in.Name() + " is up")
				}
			}
			switch in.State {
			case store.StateFailed, store.StateCrashed:
				return p.failed(ctx, in, "exited before passing checks", log)
			case store.StateHealthy:
			default:
				if waiting == nil {
					waiting = &in
				}
			}
		}
		if waiting == nil {
			return nil
		}
		if time.Now().After(deadline) {
			what := "stay up"
			if waiting.ProcessType == "web" {
				what = fmt.Sprintf("accept connections on port %d ($PORT)", waiting.Port)
			}
			return p.failed(ctx, *waiting, fmt.Sprintf("did not %s within %ds", what, secs), log)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// processDisks are the local volumes mounted in one process type.
type processDisks struct {
	mounts    []types.InstanceVolume
	vols      []store.Volume
	unplaced  []store.Volume // no disk yet: made where the instance lands
	node      string         // where the others' disks are
	pinReason string
}

func (d processDisks) names() string {
	names := make([]string, len(d.vols))
	for i, v := range d.vols {
		names[i] = v.Name
	}
	return strings.Join(names, ", ")
}

// disks works out, for each process type, the volumes its instance mounts
// and the node it must therefore run on. It refuses a rollout that cannot
// work: more than one instance, disks on different nodes, a disk moving.
func (p *Pipeline) disks(ctx context.Context, app string, rel *store.Release, quantity map[string]int) (map[string]processDisks, error) {
	vols, err := p.Store.Volumes(ctx, app)
	if err != nil {
		return nil, err
	}
	out := map[string]processDisks{}
	for _, v := range vols {
		if v.State == store.VolumeDestroying {
			continue
		}
		for _, m := range v.Mounts {
			d := out[m.ProcessType]
			d.mounts = append(d.mounts, types.InstanceVolume{ID: v.ID, Path: m.Path})
			d.vols = append(d.vols, v)
			out[m.ProcessType] = d
		}
	}
	for proc, d := range out {
		if _, ok := rel.Processes[proc]; !ok {
			continue
		}
		if quantity[proc] > 1 {
			return nil, fmt.Errorf("%s mounts volume %s, which can only be attached to one instance; scale it down with: jokku ps:scale %s %s=1",
				proc, d.names(), app, proc)
		}
		for _, v := range d.vols {
			switch {
			case v.Moving():
				return nil, fmt.Errorf("volume %s is moving to %s; deploy again once it is there (jokku storage:list %s)", v.Name, v.MovingTo, app)
			case v.Node == "":
				d.unplaced = append(d.unplaced, v)
			case d.node == "":
				d.node, d.pinReason = v.Node, fmt.Sprintf("volume %s is on %s", v.Name, v.Node)
			case d.node != v.Node:
				return nil, fmt.Errorf("%s mounts volumes on two nodes (%s, and %s on %s); move one with: jokku storage:move %s %s %s",
					proc, d.pinReason, v.Name, v.Node, app, v.Name, d.node)
			}
		}
		out[proc] = d
	}
	return out, nil
}

// restore starts again, where they ran, the instances a failed rollout had
// stopped first to free their volumes.
func (p *Pipeline) restore(ctx context.Context, insts []store.Instance) {
	ctx = context.WithoutCancel(ctx)
	for _, in := range insts {
		if in.State != store.StateHealthy {
			continue
		}
		node, err := p.Store.Node(ctx, in.Node)
		if err != nil {
			continue
		}
		subnet, _ := cluster.NodeSubnet(p.Cluster.ClusterCIDR, node.SubnetIndex)
		back := in
		back.Replaces = ""
		if err := p.Store.CreateInstance(ctx, &back, subnet); err != nil {
			p.Log.Error("restarting the previous instance", "app", in.App, "process", in.Name(), "err", err)
		}
	}
	p.Cluster.Changed()
}

// abandon stops instances a failed rollout created.
func (p *Pipeline) abandon(ctx context.Context, created []store.Instance) {
	ids := make([]string, len(created))
	for i, in := range created {
		ids[i] = in.ID
	}
	p.Store.StopInstances(context.WithoutCancel(ctx), ids, nil)
	p.Cluster.Changed()
}

func (p *Pipeline) failed(ctx context.Context, in store.Instance, why string, log func(string)) error {
	if lines := p.Cluster.InstanceLogs(ctx, in, 25); len(lines) > 0 {
		log("-----> Last output from " + in.Name() + ":")
		for _, l := range lines {
			log("       " + l)
		}
	}
	if cur, err := p.Store.CurrentRelease(ctx, in.App); err == nil && cur != nil {
		return fmt.Errorf("%s %s; v%d is still serving", in.Name(), why, cur.Version)
	}
	return fmt.Errorf("%s %s; nothing was deployed", in.Name(), why)
}

func (p *Pipeline) printURLs(ctx context.Context, app string, log func(string)) error {
	domains, err := p.Store.Domains(ctx, app)
	if err != nil {
		return err
	}
	le, err := p.computed(ctx, app, "letsencrypt")
	if err != nil {
		return err
	}
	if len(domains) == 0 {
		log("=====> Deployed " + app + ". Add a domain to reach it: jokku domains:add " + app + " <domain>")
		return nil
	}
	log("=====> Application deployed:")
	hint := false
	for _, d := range domains {
		scheme := "http"
		if proxy.PublicHost(d) {
			if le["enabled"] == "true" {
				scheme = "https"
			} else {
				hint = true
			}
		}
		log("       " + scheme + "://" + d)
	}
	if hint {
		log("       For HTTPS: jokku letsencrypt:enable " + app)
	}
	return nil
}

// defaultDomains gives an app with no domains <app>.<global> for each global
// domain, as Dokku does on deploy.
func (p *Pipeline) defaultDomains(ctx context.Context, app string) error {
	have, err := p.Store.Domains(ctx, app)
	if err != nil || len(have) > 0 {
		return err
	}
	global, err := p.Store.Domains(ctx, "")
	if err != nil {
		return err
	}
	var add []string
	for _, g := range global {
		add = append(add, app+"."+g)
	}
	_, err = p.Store.UpdateDomains(ctx, app, types.DomainsPatch{Add: add})
	return err
}

// configVars are the global vars overlaid with the app's.
func (p *Pipeline) configVars(ctx context.Context, app string) (map[string]string, error) {
	vars, err := p.Store.ConfigVars(ctx, "")
	if err != nil {
		return nil, err
	}
	appVars, err := p.Store.ConfigVars(ctx, app)
	if err != nil {
		return nil, err
	}
	maps.Copy(vars, appVars)
	return vars, nil
}

func (p *Pipeline) computed(ctx context.Context, app, plugin string) (map[string]string, error) {
	pl, _ := props.Lookup(plugin)
	global, err := p.Store.Properties(ctx, "", plugin)
	if err != nil {
		return nil, err
	}
	appProps, err := p.Store.Properties(ctx, app, plugin)
	if err != nil {
		return nil, err
	}
	return props.Compute(pl, appProps, global), nil
}

// cleanup keeps only the latest deploy's source (for ps:rebuild) and the
// rootfs artifacts recent releases or running instances use.
func (p *Pipeline) cleanup(ctx context.Context, d *types.Deploy) {
	if deploys, err := p.Store.Deploys(ctx, d.App, 100); err == nil {
		for _, old := range deploys {
			if old.ID != d.ID {
				os.RemoveAll(filepath.Join(p.DataDir, "builds", strconv.FormatInt(old.ID, 10)))
			}
		}
	}
	keep, err := p.Store.KeptArtifacts(ctx, keepReleases)
	if err != nil {
		return
	}
	dir := filepath.Join(p.DataDir, "artifacts")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if strings.HasSuffix(e.Name(), ".ext4") && !keep[path] {
			os.Remove(path)
		}
	}
}

func (p *Pipeline) Logs(ctx context.Context, app string, o types.LogOptions, line func(string)) error {
	if _, err := p.Store.App(ctx, app); err != nil {
		return err
	}
	return p.Cluster.Logs(ctx, app, o, line)
}

// RoutesChanged applies domain and proxy setting changes right away.
func (p *Pipeline) RoutesChanged() { p.Cluster.Changed() }

func shortRef(ref string) string {
	if len(ref) > 7 {
		return ref[:7]
	}
	return ref
}

func formatMB(mb int) string {
	if mb >= 1024 && mb%1024 == 0 {
		return strconv.Itoa(mb/1024) + " GiB"
	}
	return strconv.Itoa(mb) + " MiB"
}
