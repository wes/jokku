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
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wes/jokku/internal/agent"
	"github.com/wes/jokku/internal/build"
	"github.com/wes/jokku/internal/props"
	"github.com/wes/jokku/internal/proxy"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

type Pipeline struct {
	Store   *store.Store
	Builder *build.Builder
	Agent   *agent.Agent
	Node    string
	Subnet  netip.Prefix // this node's instance network
	DataDir string
	Log     *slog.Logger
}

// keepReleases is how many recent releases per app keep their rootfs on
// disk, for restarts and rollbacks.
const keepReleases = 3

func (p *Pipeline) Deploy(ctx context.Context, d *types.Deploy, sourcePath string, log func(string)) error {
	settings, err := p.settings(ctx, d.App)
	if err != nil {
		return err
	}
	src, err := Inspect(sourcePath, settings)
	if err != nil {
		return err
	}
	if err := p.Agent.Available(); err != nil {
		return err
	}
	p.Store.SetDeployStatus(ctx, d.ID, "building", "")
	log("-----> Building " + d.App + " from " + src.Dockerfile)
	res, err := p.Builder.Build(ctx, build.Options{
		App: d.App, DeployID: d.ID, Source: sourcePath,
		BuildDir: settings.BuildDir, DockerfilePath: settings.DockerfilePath,
	}, log)
	if err != nil {
		return err
	}
	procs, err := Processes(src.Procfile, res.Entrypoint, res.Cmd)
	if err != nil {
		return err
	}
	if len(src.Procfile) > 0 {
		log("-----> Process types from " + settings.ProcfilePath + ": " + strings.Join(sortedProcessTypes(src.Procfile), ", "))
	}
	vars, err := p.configVars(ctx, d.App)
	if err != nil {
		return err
	}
	description := "Deploy"
	if d.SourceRef != "" {
		description += " " + shortRef(d.SourceRef)
	}
	rel := &store.Release{
		App: d.App, Artifact: res.Artifact, Processes: procs, ConfigVars: vars, Description: description,
		Image: store.ImageConfig{Env: res.Env, WorkingDir: res.WorkingDir, User: res.User, Port: res.Port},
	}
	if err := p.Store.CreateRelease(ctx, rel); err != nil {
		return err
	}
	p.Store.SetDeployRelease(ctx, d.ID, rel.ID)
	if err := p.defaultDomains(ctx, d.App); err != nil {
		return err
	}

	p.Store.SetDeployStatus(ctx, d.ID, "deploying", "")
	if err := p.rollout(ctx, rel, log); err != nil {
		return err
	}
	p.cleanup(ctx, d)
	return p.printURLs(ctx, d.App, log)
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
		if err := p.Agent.Available(); err != nil {
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
		return err
	}
	return p.printURLs(ctx, app, log)
}

func (p *Pipeline) stop(ctx context.Context, app string, log func(string)) error {
	log("-----> Stopping " + app)
	if err := p.Store.SetAppStopped(ctx, app, true); err != nil {
		return err
	}
	p.Agent.Kick()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		insts, err := p.Store.Instances(ctx, app)
		if err != nil {
			return err
		}
		running, err := p.Agent.Host.Units(ctx)
		if err != nil {
			return err
		}
		left := 0
		for _, in := range insts {
			if _, ok := running[in.ID]; ok {
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
	// Instances that are not serving (a stopped app's, crashed ones) are
	// replaced outright; serving ones retire once the new ones pass checks.
	var stale, serving []string
	for _, in := range before {
		switch {
		case in.Desired != store.DesiredRunning:
		case in.State == store.StateHealthy:
			serving = append(serving, in.ID)
		default:
			stale = append(stale, in.ID)
		}
	}
	if err := p.Store.StopInstances(ctx, stale, nil); err != nil {
		return err
	}
	if err := p.Store.SetAppStopped(ctx, app, false); err != nil {
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
	sizes, err := p.Store.Resources(ctx, app)
	if err != nil {
		return err
	}

	var created []store.Instance
	var names []string
	for _, proc := range sortedProcessTypes(rel.Processes) {
		n, ok := quantity[proc]
		if !ok && proc == "web" {
			n = 1 // as in Dokku: one web process unless scaled
		}
		size := sizes.Effective(proc)
		for i := 1; i <= n; i++ {
			in := store.Instance{
				App: app, ReleaseID: rel.ID, ProcessType: proc, Index: i, Node: p.Node, Port: rel.Image.Port,
				CPUs: size.CPUs, MemoryMB: size.MemoryMB, Desired: store.DesiredRunning,
			}
			if err := p.Store.CreateInstance(ctx, &in, p.Subnet); err != nil {
				return err
			}
			created = append(created, in)
			names = append(names, fmt.Sprintf("%s (%d vCPU, %s)", in.Name(), in.CPUs, formatMB(in.MemoryMB)))
		}
	}
	if len(created) > 0 {
		log("-----> Starting " + strings.Join(names, ", "))
		p.Agent.Kick()
		if err := p.waitHealthy(ctx, app, created, log); err != nil {
			ids := make([]string, len(created))
			for i, in := range created {
				ids[i] = in.ID
			}
			p.Store.StopInstances(context.WithoutCancel(ctx), ids, nil)
			p.Agent.Kick()
			return err
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
	p.Agent.Reconcile(ctx) // switch the proxy to the new instances now
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
		p.Agent.Kick()
	}
}

func (p *Pipeline) failed(ctx context.Context, in store.Instance, why string, log func(string)) error {
	if lines := agent.InstanceLogs(ctx, in.ID, 25); len(lines) > 0 {
		log("-----> Last output from " + in.Name() + ":")
		for _, l := range lines {
			log("       " + l)
		}
	}
	return fmt.Errorf("%s %s; the previous release is still serving", in.Name(), why)
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
	for _, d := range domains {
		scheme := "http"
		if le["enabled"] == "true" && proxy.PublicHost(d) {
			scheme = "https"
		}
		log("       " + scheme + "://" + d)
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
	return agent.Logs(ctx, app, o, line)
}

// RoutesChanged applies domain and proxy setting changes right away.
func (p *Pipeline) RoutesChanged() { p.Agent.Kick() }

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
