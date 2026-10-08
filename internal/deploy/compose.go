package deploy

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/wes/jokku/internal/build"
	"github.com/wes/jokku/internal/compose"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// A compose app (builder:set <app> selected compose) runs its compose file:
// each service is a process type with its own image. Config vars fill in
// ${VAR} in the file, the way a .env file does for docker compose; services
// get exactly the environment the file gives them.

// imageFacts is what a service's release needs to know about its image.
type imageFacts struct {
	artifact, sha   string
	size            int64
	base            store.ImageConfig // Port 0 when the image EXPOSEs none
	entrypoint, cmd []string
}

func factsFromBuild(r *build.Result) imageFacts {
	base := store.ImageConfig{Env: r.Env, WorkingDir: r.WorkingDir, User: r.User, StopSignal: r.StopSignal}
	if r.PortFrom != build.DefaultPortFrom {
		base.Port = r.Port
	}
	return imageFacts{artifact: r.Artifact, sha: r.SHA256, size: r.Size, base: base, entrypoint: r.Entrypoint, cmd: r.Cmd}
}

func factsFromService(s store.Service) imageFacts {
	return imageFacts{artifact: s.Artifact, sha: s.ArtifactSHA, size: s.ArtifactLen, base: s.Base, entrypoint: s.Entrypoint, cmd: s.Cmd}
}

func (p *Pipeline) loadCompose(ctx context.Context, app, root string, s Settings, vars map[string]string) (*compose.Plan, error) {
	dir := filepath.Join(root, filepath.FromSlash(path.Clean("/"+s.BuildDir)))
	return compose.Load(ctx, root, dir, s.ComposeFile, app, vars)
}

// buildCompose reads the compose file of a deploy's source, builds and pulls
// its services' images (once per distinct image) and returns the release.
func (p *Pipeline) buildCompose(ctx context.Context, d *types.Deploy, sourcePath string, s Settings, log func(string)) (*store.Release, error) {
	root, err := p.Builder.Unpack(ctx, d.ID, sourcePath)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	vars, err := p.configVars(ctx, d.App)
	if err != nil {
		return nil, err
	}
	plan, err := p.loadCompose(ctx, d.App, root, s, vars)
	if err != nil {
		return nil, err
	}
	names := serviceNames(plan)
	log(fmt.Sprintf("-----> Deploying %s: %s", plan.File, strings.Join(names, ", ")))
	for _, n := range plan.Notes {
		log(" !     " + n)
	}
	if err := p.checkExternalVolumes(ctx, d.App, plan); err != nil {
		return nil, err
	}
	if err := p.Cluster.CanSchedule(ctx); err != nil {
		return nil, err
	}
	logins, err := p.logins(ctx)
	if err != nil {
		return nil, err
	}
	p.Store.SetDeployStatus(ctx, d.ID, "building", "")
	facts := map[string]imageFacts{}
	built := map[string]string{} // build key -> the first service that made it
	for _, name := range names {
		svc := plan.Services[name]
		key := svc.BuildKey(root)
		if first, ok := built[key]; ok {
			facts[name] = facts[first]
			log(fmt.Sprintf("-----> %s uses the image built for %s", name, first))
			continue
		}
		t := build.Target{
			Name: d.App + "-" + name, Context: svc.Context, Dockerfile: svc.Dockerfile, Inline: svc.Inline,
			Args: svc.Args, Stage: svc.Stage, Image: svc.Image,
		}
		if svc.Built() {
			from := "an inline Dockerfile"
			if svc.Dockerfile != "" {
				from = relPath(root, svc.Dockerfile)
			}
			log(fmt.Sprintf("-----> Building %s from %s", name, from))
		} else {
			t.Context = root // where the copied files are
			for _, c := range svc.Copies {
				t.Copies = append(t.Copies, build.Copy{Src: c.Src, Dst: c.Dst})
			}
			log(fmt.Sprintf("-----> Pulling %s for %s", svc.Image, name))
		}
		res, err := p.Builder.BuildTarget(ctx, d.ID, t, logins, log)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		facts[name], built[key] = factsFromBuild(res), name
	}
	rel, err := composeRelease(d.App, plan, root, facts, vars)
	if err != nil {
		return nil, err
	}
	return rel, p.syncComposeVolumes(ctx, d.App, plan, log)
}

// composeRelease turns a plan and its services' images into a release.
func composeRelease(app string, plan *compose.Plan, root string, facts map[string]imageFacts, vars map[string]string) (*store.Release, error) {
	rel := &store.Release{
		App: app, ConfigVars: vars, Web: plan.Web,
		Processes: map[string][]string{}, Services: map[string]store.Service{},
	}
	for name, svc := range plan.Services {
		f := facts[name]
		// As docker does: a new entrypoint drops the image's CMD.
		entrypoint, cmd := f.entrypoint, f.cmd
		if svc.Entrypoint != nil {
			entrypoint, cmd = svc.Entrypoint, nil
		}
		if svc.Command != nil {
			cmd = svc.Command
		}
		argv := append(append([]string{}, entrypoint...), cmd...)
		if len(argv) == 0 {
			return nil, fmt.Errorf("%s: its image has no ENTRYPOINT or CMD and the compose file gives no command", name)
		}
		img := f.base
		if svc.User != "" {
			img.User = svc.User
		}
		if svc.WorkingDir != "" {
			img.WorkingDir = svc.WorkingDir
		}
		if svc.StopSignal != "" {
			img.StopSignal = svc.StopSignal
		}
		if svc.Port > 0 {
			img.Port = svc.Port
		}
		if img.Port == 0 && name == plan.Web {
			img.Port = build.DefaultPort // the proxy needs a port to send to
		}
		check := types.CheckUp
		if img.Port > 0 && !svc.NoCheck {
			check = types.CheckTCP
		}
		rel.Processes[name] = argv
		rel.Services[name] = store.Service{
			Artifact: f.artifact, ArtifactSHA: f.sha, ArtifactLen: f.size, Image: img, Env: svc.Env,
			Replicas: svc.Replicas, CPUs: svc.CPUs, MemoryMB: svc.MemoryMB, Restart: svc.Restart,
			Check: check, StopSecs: svc.StopSecs, DependsOn: svc.DependsOn,
			BuildKey: svc.BuildKey(root), Base: f.base, Entrypoint: f.entrypoint, Cmd: f.cmd,
		}
	}
	return rel, nil
}

// recompose reads the last deploy's compose file again with today's config
// vars, reusing each service's image, for a config change. A change that
// alters how an image is made needs a deploy instead.
func (p *Pipeline) recompose(ctx context.Context, app string, cur *store.Release, log func(string)) (*store.Release, error) {
	vars, err := p.configVars(ctx, app)
	if err != nil {
		return nil, err
	}
	src, prev, err := p.lastSource(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("the compose file of the last deploy is gone, so the config change can't be applied; deploy again with: jokku ps:rebuild %s", app)
	}
	root, err := p.Builder.Unpack(ctx, prev.ID, src)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	settings, err := p.settings(ctx, app)
	if err != nil {
		return nil, err
	}
	plan, err := p.loadCompose(ctx, app, root, settings, vars)
	if err != nil {
		return nil, err
	}
	facts := map[string]imageFacts{}
	for name, svc := range plan.Services {
		old, ok := cur.Services[name]
		if !ok || old.BuildKey != svc.BuildKey(root) {
			return nil, fmt.Errorf("the config change alters how %s is built; build it with: jokku ps:rebuild %s", name, app)
		}
		facts[name] = factsFromService(old)
	}
	rel, err := composeRelease(app, plan, root, facts, vars)
	if err != nil {
		return nil, err
	}
	rel.Description = "Config change"
	return rel, p.syncComposeVolumes(ctx, app, plan, log)
}

// checkExternalVolumes makes sure volumes the file marks external exist.
func (p *Pipeline) checkExternalVolumes(ctx context.Context, app string, plan *compose.Plan) error {
	for _, m := range plan.Volumes() {
		if !m.External {
			continue
		}
		if _, err := p.Store.Volume(ctx, app, m.Volume); err != nil {
			return fmt.Errorf("volume %s is external, so it must exist first: jokku storage:create %s %s", m.Volume, app, m.Volume)
		}
	}
	return nil
}

// syncComposeVolumes makes the app's volumes and mounts what the compose
// file says: missing volumes are created, and mounts the file no longer
// has are removed (the volumes and their data stay).
func (p *Pipeline) syncComposeVolumes(ctx context.Context, app string, plan *compose.Plan, log func(string)) error {
	vols, err := p.Store.Volumes(ctx, app)
	if err != nil {
		return err
	}
	byName := map[string]store.Volume{}
	for _, v := range vols {
		if v.State != store.VolumeDestroying {
			byName[v.Name] = v
		}
	}
	want := map[string]types.VolumeMount{}
	for name, svc := range plan.Services {
		for _, m := range svc.Mounts {
			want[m.Volume] = types.VolumeMount{ProcessType: name, Path: m.Path}
		}
	}
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		m := want[name]
		v, ok := byName[name]
		if !ok {
			nv := &store.Volume{App: app, Name: name, Type: types.VolumeLocal, SizeMB: store.DefaultVolumeMB}
			if err := p.Store.CreateVolume(ctx, nv); err != nil {
				return err
			}
			log(fmt.Sprintf("-----> Created volume %s (%s, %d GiB; grow it with storage:resize)", name, nv.Type, nv.SizeMB/1024))
			v = *nv
		}
		for _, old := range v.Mounts {
			if old != m {
				if err := p.Store.RemoveVolumeMount(ctx, v.ID, old); err != nil {
					return err
				}
			}
		}
		if !slices.Contains(v.Mounts, m) {
			if err := p.Store.AddVolumeMount(ctx, v.ID, m); err != nil {
				return err
			}
		}
	}
	for name, v := range byName {
		if _, ok := want[name]; ok || len(v.Mounts) == 0 {
			continue
		}
		for _, old := range v.Mounts {
			if err := p.Store.RemoveVolumeMount(ctx, v.ID, old); err != nil {
				return err
			}
		}
		log(fmt.Sprintf("-----> No service mounts volume %s any more; it keeps its data (delete it with storage:destroy)", name))
	}
	return nil
}

func serviceNames(plan *compose.Plan) []string { return sortedProcessTypes(plan.Services) }

func relPath(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}
