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
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	Builder ImageBuilder
	Cluster *cluster.Controller
	DataDir string
	Log     *slog.Logger
}

// ImageBuilder makes root filesystems: a *build.Builder, or a fake in tests.
type ImageBuilder interface {
	Build(ctx context.Context, o build.Options, log func(string)) (*build.Result, error)
	Unpack(ctx context.Context, deployID int64, source string) (string, error)
	BuildTarget(ctx context.Context, deployID int64, t build.Target, logins []build.RegistryLogin, log func(string)) (*build.Result, error)
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
	if d.Source == SourceImage {
		// The source is just "FROM <image>": the app's build settings are
		// for its own repo, not this.
		settings = Settings{DockerfilePath: "Dockerfile", ProcfilePath: "Procfile"}
	}
	if settings.Builder == "compose" {
		rel, err := p.buildCompose(ctx, d, sourcePath, settings, log)
		if err != nil {
			return 0, err
		}
		rel.Description = description(d)
		return p.release(ctx, d, rel, log)
	}
	src, err := Inspect(sourcePath, settings)
	if err != nil {
		return 0, err
	}
	if err := p.Cluster.CanSchedule(ctx); err != nil {
		return 0, err
	}
	logins, err := p.logins(ctx)
	if err != nil {
		return 0, err
	}
	p.Store.SetDeployStatus(ctx, d.ID, "building", "")
	if d.Source == SourceImage {
		log("-----> Pulling " + d.SourceRef)
	} else {
		log("-----> Building " + d.App + " from " + src.Dockerfile)
	}
	res, err := p.Builder.Build(ctx, build.Options{
		App: d.App, DeployID: d.ID, Source: sourcePath,
		BuildDir: settings.BuildDir, DockerfilePath: settings.DockerfilePath, Logins: logins,
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
	rel := &store.Release{
		App: d.App, Artifact: res.Artifact, ArtifactSHA: res.SHA256, ArtifactLen: res.Size,
		Processes: procs, ConfigVars: vars, Description: description(d),
		Image: store.ImageConfig{Env: res.Env, WorkingDir: res.WorkingDir, User: res.User, Port: res.Port, StopSignal: res.StopSignal},
	}
	from := res.PortFrom
	if rel.Port() != res.Port {
		from = "from the PORT config var"
	}
	log(fmt.Sprintf("-----> $PORT is %d, %s", rel.Port(), from))
	return p.release(ctx, d, rel, log)
}

func description(d *types.Deploy) string {
	switch {
	case d.Source == SourceImage:
		return "Deploy " + d.SourceRef
	case d.SourceRef != "":
		return "Deploy " + shortRef(d.SourceRef)
	}
	return "Deploy"
}

// release stores a deploy's release and rolls it out.
func (p *Pipeline) release(ctx context.Context, d *types.Deploy, rel *store.Release, log func(string)) (int, error) {
	if err := p.Store.CreateRelease(ctx, rel); err != nil {
		return 0, err
	}
	p.Store.SetDeployRelease(ctx, d.ID, rel.ID)
	if err := p.defaultDomains(ctx, d.App); err != nil {
		return 0, err
	}
	p.Store.SetDeployStatus(ctx, d.ID, "deploying", "")
	if err := p.rollout(ctx, rel, applyChanges, log); err != nil {
		return 0, err
	}
	p.cleanup(ctx, d)
	return rel.Version, p.printURLs(ctx, rel, log)
}

// SourceImage is the deploy source of git:from-image: a registry image,
// built from a one-line Dockerfile, "FROM <image>".
const SourceImage = "image"

// ImageSource is the source tarball for an image deploy.
func ImageSource(image string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dockerfile := []byte("FROM " + image + "\n")
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(dockerfile)), Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(dockerfile); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// logins are the registry credentials builds pull with.
func (p *Pipeline) logins(ctx context.Context) ([]build.RegistryLogin, error) {
	stored, err := p.Store.RegistryLogins(ctx)
	if err != nil {
		return nil, err
	}
	var out []build.RegistryLogin
	for _, l := range stored {
		out = append(out, build.RegistryLogin{Server: l.Server, Username: l.Username, Password: l.Password})
	}
	return out, nil
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

// PS runs a "ps:" action: restart, start, stop or rebuild, or apply (roll
// out settings changes, restarting only process types they change).
func (p *Pipeline) PS(ctx context.Context, app, action, actor string, log func(string)) error {
	if _, err := p.Store.App(ctx, app); err != nil {
		return err
	}
	switch action {
	case "stop":
		return p.stop(ctx, app, log)
	case "rebuild":
		return p.rebuild(ctx, app, actor, log)
	case "start", "restart", "apply":
		if err := p.Cluster.CanSchedule(ctx); err != nil {
			return err
		}
		mode := applyChanges
		if action == "restart" {
			mode = restartAll
		}
		return p.restart(ctx, app, mode, log)
	}
	return fmt.Errorf("unknown action %q", action)
}

// restart rolls out the current release again, picking up config, scale and
// resource changes. Changed config vars make a new release; for a compose
// app that means reading its compose file again. restartAll replaces every
// instance (ps:restart); applyChanges only those the changes affect.
func (p *Pipeline) restart(ctx context.Context, app string, mode rolloutMode, log func(string)) error {
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
		if cur.Compose() {
			recomposed, err := p.recompose(ctx, app, cur, log)
			if err != nil {
				return err
			}
			next = *recomposed
		}
		if err := p.Store.CreateRelease(ctx, &next); err != nil {
			return err
		}
		rel = &next
	}
	if mode == restartAll {
		log(fmt.Sprintf("-----> Restarting %s (v%d)", app, rel.Version))
	} else {
		log(fmt.Sprintf("-----> Applying changes to %s (v%d)", app, rel.Version))
	}
	if err := p.rollout(ctx, rel, mode, log); err != nil {
		p.Store.AddEvent(ctx, "deploy", app, "", "restart failed: %v", err)
		return err
	}
	p.Store.AddEvent(ctx, "deploy", app, "", "restarted (v%d)", rel.Version)
	return p.printURLs(ctx, rel, log)
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
	source, prev, err := p.lastSource(ctx, app)
	if err != nil {
		return err
	}
	kind := "rebuild"
	if prev.Source == SourceImage {
		kind = SourceImage // pull the image again
	}
	d, err := p.Store.CreateDeploy(ctx, app, kind, prev.SourceRef, actor)
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

// lastSource finds the source tarball of the app's latest successful
// deploy, which cleanup keeps.
func (p *Pipeline) lastSource(ctx context.Context, app string) (string, *types.Deploy, error) {
	deploys, err := p.Store.Deploys(ctx, app, 100)
	if err != nil {
		return "", nil, err
	}
	for _, prev := range deploys {
		if prev.Status != types.StatusSucceeded {
			continue
		}
		source := filepath.Join(p.DataDir, "builds", strconv.FormatInt(prev.ID, 10), "source.tar")
		if _, err := os.Stat(source); err == nil {
			return source, &prev, nil
		}
	}
	return "", nil, fmt.Errorf("no source to rebuild %s from: git push to deploy it", app)
}

// rolloutMode says what a rollout replaces.
type rolloutMode int

const (
	// restartAll replaces every instance (ps:restart).
	restartAll rolloutMode = iota
	// applyChanges keeps the instances of process types the new release
	// would run exactly the same way (deploys, config:set, ps:scale): a
	// compose app's database doesn't restart because its web service
	// changed.
	applyChanges
)

// rollout boots rel's formation, waits for checks, switches traffic and
// schedules the old instances' retirement. Process types start in
// dependency order (a compose file's depends_on): each group passes checks
// before the next starts.
func (p *Pipeline) rollout(ctx context.Context, rel *store.Release, mode rolloutMode, log func(string)) error {
	app := rel.App
	appInfo, err := p.Store.App(ctx, app)
	if err != nil {
		return err
	}
	cur, err := p.Store.CurrentRelease(ctx, app)
	if err != nil {
		return err
	}
	before, err := p.Store.Instances(ctx, app)
	if err != nil {
		return err
	}
	quantity, err := p.quantities(ctx, rel)
	if err != nil {
		return err
	}
	sizes, err := p.Store.Resources(ctx, app)
	if err != nil {
		return err
	}
	disks, err := p.disks(ctx, app, rel, quantity)
	if err != nil {
		return err
	}

	kept := map[string]bool{} // instance IDs that carry on into rel
	var keptProcs []string
	if mode == applyChanges && cur != nil && !appInfo.Stopped {
		for _, proc := range sortedProcessTypes(rel.Processes) {
			ins, ok := unchanged(cur, rel, proc, before, quantity[proc], sizeFor(sizes, rel, proc), disks[proc].mounts)
			if !ok || len(ins) == 0 {
				continue
			}
			keptProcs = append(keptProcs, proc)
			for _, in := range ins {
				kept[in.ID] = true
			}
		}
	}

	// Instances that are not serving (a stopped app's, crashed ones) are
	// replaced outright; serving ones retire once the new ones pass checks.
	// A process type with local volumes can't overlap: its one instance
	// holds the disks, so the old instance stops first.
	var stale, serving []string
	var first []store.Instance
	for _, in := range before {
		switch {
		case in.Desired != store.DesiredRunning || kept[in.ID]:
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
	if len(keptProcs) > 0 {
		log("-----> Unchanged, left running: " + strings.Join(keptProcs, ", "))
	}

	var created []store.Instance
	for _, stage := range stages(rel) {
		var batch []store.Instance
		var names []string
		for _, proc := range stage {
			if slices.Contains(keptProcs, proc) {
				continue
			}
			size := sizeFor(sizes, rel, proc)
			d := disks[proc]
			for i := 1; i <= quantity[proc]; i++ {
				node, subnet, err := p.Cluster.Place(ctx, cluster.Placement{
					App: app, ProcessType: proc, CPUs: size.CPUs, MemoryMB: size.MemoryMB,
					Pin: d.node, PinReason: d.pinReason, Volumes: len(d.mounts) > 0,
				})
				if err != nil {
					return fail(append(created, batch...), err)
				}
				for _, v := range d.unplaced {
					if err := p.Store.PlaceVolume(ctx, v.ID, node); err != nil {
						return fail(append(created, batch...), err)
					}
				}
				in := store.Instance{
					App: app, ReleaseID: rel.ID, ProcessType: proc, Index: i, Node: node, Port: rel.PortFor(proc),
					CPUs: size.CPUs, MemoryMB: size.MemoryMB, Desired: store.DesiredRunning, Volumes: d.mounts,
				}
				if err := p.Store.CreateInstance(ctx, &in, subnet); err != nil {
					return fail(append(created, batch...), err)
				}
				batch = append(batch, in)
				names = append(names, fmt.Sprintf("%s on %s (%d vCPU, %s)", in.Name(), node, in.CPUs, formatMB(in.MemoryMB)))
			}
		}
		if len(batch) == 0 {
			continue
		}
		log("-----> Starting " + strings.Join(names, ", "))
		p.Cluster.Changed()
		created = append(created, batch...)
		if err := p.waitHealthy(ctx, rel, batch, log); err != nil {
			return fail(created, err)
		}
	}
	if len(created) == 0 && len(keptProcs) == 0 {
		hint := "web=1"
		if rel.Compose() {
			hint = "<service>=1"
		}
		log("-----> No processes are scaled up (jokku ps:scale " + app + " " + hint + ")")
	}

	keptIDs := make([]string, 0, len(kept))
	for id := range kept {
		keptIDs = append(keptIDs, id)
	}
	if err := p.Store.PromoteRelease(ctx, app, rel.ID, keptIDs); err != nil {
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

// quantities is how many instances of each process type rel runs: what
// ps:scale set, otherwise a compose service's replicas, otherwise one web
// process (as in Dokku) and none of the others.
func (p *Pipeline) quantities(ctx context.Context, rel *store.Release) (map[string]int, error) {
	scaled, err := p.Store.Formation(ctx, rel.App)
	if err != nil {
		return nil, err
	}
	set := map[string]int{}
	for _, proc := range scaled {
		set[proc.Type] = proc.Quantity
	}
	q := map[string]int{}
	for proc := range rel.Processes {
		n, ok := set[proc]
		switch {
		case ok:
		case rel.Compose():
			n = rel.Services[proc].Replicas
		case proc == "web":
			n = 1
		}
		q[proc] = n
	}
	return q, nil
}

// sizeFor is a process type's size: resource:limit for the process type,
// then the compose file's limits, then resource:limit for the app, then the
// defaults.
func sizeFor(sizes store.AppResources, rel *store.Release, proc string) types.ResourceSize {
	size := types.ResourceSize{CPUs: store.DefaultCPUs, MemoryMB: store.DefaultMemoryMB}
	svc := rel.Services[proc]
	for _, o := range []types.ResourceSize{sizes.Default, {CPUs: svc.CPUs, MemoryMB: svc.MemoryMB}, sizes.Process[proc]} {
		if o.CPUs > 0 {
			size.CPUs = o.CPUs
		}
		if o.MemoryMB > 0 {
			size.MemoryMB = o.MemoryMB
		}
	}
	return size
}

// unchanged returns proc's instances when rel would run them exactly as
// they run now (same image, command, environment, port, size and volumes,
// same count, all healthy), so they can carry on.
func unchanged(cur, rel *store.Release, proc string, before []store.Instance, n int, size types.ResourceSize, mounts []types.InstanceVolume) ([]store.Instance, bool) {
	if _, ok := cur.Processes[proc]; !ok {
		return nil, false
	}
	var ins []store.Instance
	for _, in := range before {
		if in.ProcessType != proc || in.Desired != store.DesiredRunning {
			continue
		}
		if in.ReleaseID != cur.ID || in.State != store.StateHealthy {
			return nil, false
		}
		ins = append(ins, in)
	}
	if len(ins) != n {
		return nil, false
	}
	for _, in := range ins {
		next := in
		next.Port, next.CPUs, next.MemoryMB, next.Volumes = rel.PortFor(proc), size.CPUs, size.MemoryMB, mounts
		if specKey(cur, in) != specKey(rel, next) {
			return nil, false
		}
	}
	return ins, true
}

// specKey sums up how an instance runs under a release: everything its VM
// is booted with.
func specKey(rel *store.Release, in store.Instance) string {
	artifact, _, _, img := rel.ImageFor(in.ProcessType)
	svc := rel.Services[in.ProcessType]
	var vols []types.InstanceVolume
	if len(in.Volumes) > 0 {
		vols = in.Volumes
	}
	b, _ := json.Marshal([]any{
		artifact, rel.Processes[in.ProcessType], cluster.Env(rel, in), img,
		in.Port, in.CPUs, in.MemoryMB, vols, svc.Check, svc.StopSecs,
	})
	return string(b)
}

// stages orders a release's process types for starting: each stage only
// depends on earlier ones. A Dockerfile app is one stage.
func stages(rel *store.Release) [][]string {
	procs := sortedProcessTypes(rel.Processes)
	if !rel.Compose() {
		return [][]string{procs}
	}
	done := map[string]bool{}
	var out [][]string
	for len(done) < len(procs) {
		var stage []string
		for _, proc := range procs {
			if done[proc] {
				continue
			}
			ready := true
			for _, dep := range rel.Services[proc].DependsOn {
				if _, ok := rel.Processes[dep]; ok && !done[dep] {
					ready = false
				}
			}
			if ready {
				stage = append(stage, proc)
			}
		}
		if len(stage) == 0 { // a cycle: start the rest together
			for _, proc := range procs {
				if !done[proc] {
					stage = append(stage, proc)
				}
			}
		}
		for _, proc := range stage {
			done[proc] = true
		}
		out = append(out, stage)
	}
	return out
}

// waitHealthy follows new instances until all pass checks, one fails, or the
// checks timeout passes.
func (p *Pipeline) waitHealthy(ctx context.Context, rel *store.Release, created []store.Instance, log func(string)) error {
	app := rel.App
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
			if check := rel.Services[waiting.ProcessType].Check; check == types.CheckTCP || (check == "" && waiting.ProcessType == "web") {
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

func (p *Pipeline) printURLs(ctx context.Context, rel *store.Release, log func(string)) error {
	app := rel.App
	if rel.WebProcess() == "" {
		log("=====> Deployed " + app + ". No service gets HTTP traffic (publish ports on one, or name it web)")
		return nil
	}
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
