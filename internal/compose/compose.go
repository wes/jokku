// Package compose turns a docker compose file into what Jokku runs: one app
// whose process types are the file's services, each with its own image.
//
// The file is read with compose-go, the loader docker compose itself uses,
// so interpolation (${VAR}, with the app's config vars and the repo's .env),
// profiles (COMPOSE_PROFILES), extends within the file and the many short
// and long syntaxes behave as in Docker. What Jokku cannot run (host
// networking, privileged containers, secrets, host paths) is refused with a
// message rather than half emulated.
//
// Every path the file names must stay inside the pushed source: the deploy
// runs as root on the control node and must never read anything else.
package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v4"
)

// DefaultFiles are looked for, in order, when no compose file is set.
var DefaultFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// Plan is a compose file, translated.
type Plan struct {
	File     string // the compose file, relative to the source root
	Services map[string]*Service
	// Web is the service that gets HTTP traffic: the one named web, or the
	// only one publishing ports. "" when none does.
	Web string
	// Notes are things in the file Jokku ignores, for the deploy log.
	Notes []string
}

// Service is one service, as a Jokku process type.
type Service struct {
	Name string

	// Image is pulled when Context is empty; otherwise Dockerfile (or
	// Inline) is built in Context with Args, up to Stage.
	Image      string
	Context    string // absolute
	Dockerfile string // absolute
	Inline     string
	Args       map[string]string
	Stage      string
	Copies     []Copy // files of the source baked into an Image, from bind mounts

	Entrypoint []string // nil: the image's
	Command    []string // nil: the image's
	Env        []string // KEY=value, sorted
	User       string
	WorkingDir string
	Port       int // 0: the image's EXPOSE, if any
	Published  bool
	Replicas   int
	CPUs       int // 0: Jokku's default
	MemoryMB   int // 0: Jokku's default
	Restart    string
	StopSecs   int
	StopSignal string
	DependsOn  []string
	NoCheck    bool // healthcheck disabled
	Mounts     []Mount
}

// Copy is a file or directory of the source (Src, relative to the root)
// baked into the image at Dst.
type Copy struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

// Mount puts a Jokku volume at Path.
type Mount struct {
	Volume string // Jokku volume name
	Path   string
	// External volumes must already exist; others are created on deploy.
	External bool
}

// Built reports whether the service's image is built from source.
func (s *Service) Built() bool { return s.Context != "" }

// BuildKey describes how the service's image is made, with paths relative
// to root so it compares across deploys. Two services with the same key
// share one build.
func (s *Service) BuildKey(root string) string {
	rel := func(p string) string {
		if p == "" {
			return ""
		}
		r, err := filepath.Rel(root, p)
		if err != nil {
			return p
		}
		return filepath.ToSlash(r)
	}
	b, _ := json.Marshal(struct {
		Image, Context, Dockerfile, Inline, Stage string
		Args                                      map[string]string
		Copies                                    []Copy
	}{s.Image, rel(s.Context), rel(s.Dockerfile), s.Inline, s.Stage, s.Args, s.Copies})
	return string(b)
}

var (
	serviceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	volumeNameRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// Load reads the compose file in dir, a directory inside root (the unpacked
// source), with vars filling in ${VAR}. file is relative to dir; "" finds
// one of DefaultFiles. app names the project.
func Load(ctx context.Context, root, dir, file, app string, vars map[string]string) (*Plan, error) {
	path, err := findFile(root, dir, file, app)
	if err != nil {
		return nil, err
	}
	if err := prescan(path); err != nil {
		return nil, err
	}
	if dotenv := filepath.Join(dir, ".env"); exists(dotenv) {
		if err := inside(root, dotenv); err != nil {
			return nil, err
		}
	}
	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	opts, err := cli.NewProjectOptions([]string{path},
		cli.WithWorkingDirectory(dir),
		cli.WithName(app),
		cli.WithEnv(env), // config vars win over the repo's .env
		cli.WithEnvFiles(),
		cli.WithDotEnv,
		cli.WithDefaultProfiles(),
		cli.WithoutEnvironmentResolution, // env_file is read below, once its path is checked
	)
	if err != nil {
		return nil, err
	}
	project, err := opts.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", relTo(root, path), err)
	}
	var problems []string
	for _, svc := range project.Services {
		for _, ef := range svc.EnvFiles {
			if err := inside(root, ef.Path); err != nil && exists(ef.Path) {
				problems = append(problems, fmt.Sprintf("%s: env_file %v", svc.Name, err))
			}
		}
	}
	if len(problems) > 0 {
		return nil, fail(relTo(root, path), problems)
	}
	if project, err = project.WithServicesEnvironmentResolved(true); err != nil {
		return nil, err
	}

	plan := &Plan{File: relTo(root, path), Services: map[string]*Service{}}
	volumeNames := map[string]string{} // Jokku name -> compose name
	names := make([]string, 0, len(project.Services))
	for name := range project.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		svc := project.Services[name]
		s, notes, errs := translate(root, project, svc, volumeNames)
		for _, e := range errs {
			problems = append(problems, name+": "+e)
		}
		for _, n := range notes {
			plan.Notes = append(plan.Notes, name+": "+n)
		}
		if s != nil {
			plan.Services[name] = s
		}
	}
	if len(plan.Services) == 0 && len(problems) == 0 {
		problems = append(problems, "no services to run (are they all behind profiles? set COMPOSE_PROFILES with config:set)")
	}
	problems = append(problems, plan.pickWeb()...)
	problems = append(problems, plan.checkVolumes()...)
	if len(problems) > 0 {
		return nil, fail(plan.File, problems)
	}
	return plan, nil
}

func fail(file string, problems []string) error {
	return fmt.Errorf("Jokku cannot run %s as it is:\n- %s", file, strings.Join(problems, "\n- "))
}

func findFile(root, dir, file, app string) (string, error) {
	if file != "" {
		path := filepath.Join(dir, filepath.FromSlash(filepath.Clean("/"+file)))
		if !exists(path) {
			return "", fmt.Errorf("no %s in the source (to deploy another file: jokku builder:compose %s <path>)", relTo(root, path), app)
		}
		return path, inside(root, path)
	}
	for _, f := range DefaultFiles {
		if path := filepath.Join(dir, f); exists(path) {
			return path, inside(root, path)
		}
	}
	return "", fmt.Errorf("no compose file in %s (looked for %s; to deploy another file: jokku builder:compose %s <path>)", relTo(root, dir)+"/", strings.Join(DefaultFiles, ", "), app)
}

// prescan refuses what would make the loader read other files before Jokku
// can check where they are: include, and extends from another file.
func prescan(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc struct {
		Include  any `yaml:"include"`
		Services map[string]struct {
			Extends any `yaml:"extends"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if doc.Include != nil {
		return errors.New("include is not supported yet: put the services in one compose file")
	}
	for name, s := range doc.Services {
		if m, ok := s.Extends.(map[string]any); ok && m["file"] != nil {
			return fmt.Errorf("%s: extends from another file is not supported yet; extend a service of the same file", name)
		}
	}
	return nil
}

// translate maps one service, returning notes on what it ignores and the
// problems that stop it from running.
func translate(root string, project *types.Project, svc types.ServiceConfig, volumeNames map[string]string) (*Service, []string, []string) {
	var notes, problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if !serviceNameRe.MatchString(svc.Name) {
		bad("the name must be letters, digits, - and _ (it becomes a process type and a DNS name)")
	}
	for field, set := range map[string]bool{
		"privileged":   svc.Privileged,
		"cap_add":      len(svc.CapAdd) > 0,
		"devices":      len(svc.Devices) > 0,
		"network_mode": svc.NetworkMode != "" && svc.NetworkMode != "bridge",
		"pid":          svc.Pid != "",
		"ipc":          svc.Ipc != "",
		"userns_mode":  svc.UserNSMode != "",
		"cgroup":       svc.Cgroup != "" || svc.CgroupParent != "",
		"sysctls":      len(svc.Sysctls) > 0,
		"security_opt": len(svc.SecurityOpt) > 0,
		"secrets":      len(svc.Secrets) > 0,
		"configs":      len(svc.Configs) > 0,
		"gpus":         len(svc.Gpus) > 0,
		"runtime":      svc.Runtime != "",
		"volumes_from": len(svc.VolumesFrom) > 0,
		"extra_hosts":  len(svc.ExtraHosts) > 0,
		"dns":          len(svc.DNS) > 0 || len(svc.DNSSearch) > 0 || len(svc.DNSOpts) > 0,
		"label_file":   len(svc.LabelFiles) > 0,
		"provider":     svc.Provider != nil,
		"models":       len(svc.Models) > 0,
	} {
		if set {
			bad("%s is not supported in a microVM", field)
		}
	}

	s := &Service{Name: svc.Name, Replicas: 1, User: svc.User, WorkingDir: svc.WorkingDir, StopSignal: svc.StopSignal}
	switch {
	case svc.Build != nil:
		b := svc.Build
		if b.Context == "" || strings.Contains(b.Context, "://") || strings.HasPrefix(b.Context, "git@") {
			bad("build.context must be a directory of the repo, not %q", b.Context)
			break
		}
		s.Context = absIn(project.WorkingDir, b.Context)
		if err := inside(root, s.Context); err != nil {
			bad("build.context %v", err)
		}
		if b.DockerfileInline != "" {
			s.Inline = b.DockerfileInline
		} else {
			df := b.Dockerfile
			if df == "" {
				df = "Dockerfile"
			}
			s.Dockerfile = absIn(s.Context, df)
			if err := inside(root, s.Dockerfile); err != nil {
				bad("build.dockerfile %v", err)
			} else if !exists(s.Dockerfile) {
				bad("no %s in the source", relTo(root, s.Dockerfile))
			}
		}
		s.Stage = b.Target
		s.Args = map[string]string{}
		for k, v := range b.Args {
			if v != nil {
				s.Args[k] = *v
			}
		}
		for field, set := range map[string]bool{
			"build.secrets": len(b.Secrets) > 0, "build.ssh": len(b.SSH) > 0,
			"build.additional_contexts": len(b.AdditionalContexts) > 0,
			"build.network":             b.Network != "" && b.Network != "default",
			"build.entitlements":        len(b.Entitlements) > 0,
		} {
			if set {
				bad("%s is not supported", field)
			}
		}
	case svc.Image != "":
		s.Image = svc.Image
	default:
		bad("needs an image or a build")
	}

	if svc.Entrypoint != nil {
		s.Entrypoint = []string(svc.Entrypoint)
	}
	if svc.Command != nil {
		s.Command = []string(svc.Command)
	}
	for k, v := range svc.Environment {
		if v != nil {
			s.Env = append(s.Env, k+"="+*v)
		}
	}
	sort.Strings(s.Env)

	for _, p := range svc.Ports {
		if p.Protocol != "" && p.Protocol != "tcp" {
			bad("port %d/%s: only TCP is supported", p.Target, p.Protocol)
			continue
		}
		if s.Port == 0 {
			s.Port = int(p.Target)
		}
		if p.Published != "" {
			s.Published = true
		}
	}
	if s.Port == 0 {
		for _, e := range svc.Expose {
			if n, err := strconv.Atoi(strings.SplitN(strings.TrimSuffix(e, "/tcp"), "-", 2)[0]); err == nil && n > 0 && n < 65536 {
				s.Port = n
				break
			}
		}
	}

	if svc.Scale != nil {
		s.Replicas = *svc.Scale
	}
	if svc.Deploy != nil && svc.Deploy.Replicas != nil {
		s.Replicas = *svc.Deploy.Replicas
	}
	cpus, mem := float64(svc.CPUS), int64(svc.MemLimit)
	if svc.Deploy != nil && svc.Deploy.Resources.Limits != nil {
		if l := svc.Deploy.Resources.Limits; l.NanoCPUs > 0 {
			cpus = float64(l.NanoCPUs)
		}
		if l := svc.Deploy.Resources.Limits; l.MemoryBytes > 0 {
			mem = int64(l.MemoryBytes)
		}
	}
	if cpus > 0 {
		s.CPUs = max(1, int(math.Ceil(cpus)))
	}
	if mem > 0 {
		s.MemoryMB = max(128, int(mem>>20))
	}

	s.Restart = restartPolicy(svc)
	if svc.StopGracePeriod != nil {
		s.StopSecs = max(1, int(math.Ceil(time.Duration(*svc.StopGracePeriod).Seconds())))
	}
	if hc := svc.HealthCheck; hc != nil && (hc.Disable || (len(hc.Test) > 0 && strings.EqualFold(hc.Test[0], "NONE"))) {
		s.NoCheck = true
	} else if hc != nil && len(hc.Test) > 0 {
		notes = append(notes, "its healthcheck command can't run inside the VM yet; Jokku checks that it accepts connections on its port instead")
	}

	deps := make([]string, 0, len(svc.DependsOn))
	for dep, d := range svc.DependsOn {
		if d.Condition == types.ServiceConditionCompletedSuccessfully {
			bad("depends_on %s with service_completed_successfully: one-off services (migrations) are not supported yet; run them from the app's entrypoint", dep)
			continue
		}
		if _, ok := project.Services[dep]; ok {
			deps = append(deps, dep)
		}
	}
	sort.Strings(deps)
	s.DependsOn = deps

	for _, v := range svc.Volumes {
		switch v.Type {
		case types.VolumeTypeVolume:
			if v.Source == "" {
				notes = append(notes, v.Target+" is an anonymous volume; it is the instance's own disk, kept until the instance is replaced")
				continue
			}
			name := strings.ReplaceAll(strings.ToLower(v.Source), "_", "-")
			if !volumeNameRe.MatchString(name) {
				bad("volume %q: Jokku volume names are lowercase letters, digits and dashes", v.Source)
				continue
			}
			if other, ok := volumeNames[name]; ok && other != v.Source {
				bad("volumes %q and %q would both be named %s", other, v.Source, name)
				continue
			}
			volumeNames[name] = v.Source
			if v.ReadOnly {
				notes = append(notes, "volume "+v.Source+" is mounted read-write: read-only mounts are not supported yet")
			}
			s.Mounts = append(s.Mounts, Mount{Volume: name, Path: v.Target, External: bool(project.Volumes[v.Source].External)})
		case types.VolumeTypeBind:
			src := absIn(project.WorkingDir, v.Source)
			if err := inside(root, src); err != nil || !exists(src) {
				bad("bind mount %s: only files of the repo can be mounted (it is copied into the image)", v.Source)
				continue
			}
			if s.Built() {
				bad("bind mount %s: copy it into the image in the Dockerfile instead (COPY %s %s)", v.Source, relTo(project.WorkingDir, src), v.Target)
				continue
			}
			s.Copies = append(s.Copies, Copy{Src: relTo(root, src), Dst: v.Target})
		case types.VolumeTypeTmpfs:
			notes = append(notes, v.Target+" (tmpfs) is the instance's own disk")
		default:
			bad("%s volumes are not supported", v.Type)
		}
	}
	for _, t := range svc.Tmpfs {
		notes = append(notes, t+" (tmpfs) is the instance's own disk")
	}
	if len(problems) > 0 {
		return nil, notes, problems
	}
	return s, notes, nil
}

func restartPolicy(svc types.ServiceConfig) string {
	if svc.Deploy != nil && svc.Deploy.RestartPolicy != nil {
		rp := svc.Deploy.RestartPolicy
		switch rp.Condition {
		case "none":
			return "no"
		case "any":
			return "always"
		case "on-failure":
			if rp.MaxAttempts != nil {
				return fmt.Sprintf("on-failure:%d", *rp.MaxAttempts)
			}
			return "on-failure"
		}
	}
	switch r := svc.Restart; {
	case r == "no" || r == "always" || r == "on-failure":
		return r
	case r == "unless-stopped":
		return "always"
	case strings.HasPrefix(r, "on-failure:"):
		return r
	}
	return ""
}

// pickWeb chooses the service that gets HTTP traffic.
func (p *Plan) pickWeb() []string {
	if _, ok := p.Services["web"]; ok {
		p.Web = "web"
		return nil
	}
	var published []string
	for name, s := range p.Services {
		if s.Published {
			published = append(published, name)
		}
	}
	sort.Strings(published)
	switch len(published) {
	case 0:
		p.Notes = append(p.Notes, "no service publishes ports or is named web, so no HTTP traffic is routed")
	case 1:
		p.Web = published[0]
	default:
		return []string{fmt.Sprintf("services %s all publish ports; Jokku routes HTTP to one: name it web, or remove ports: from the others (they still reach each other by name)",
			strings.Join(published, ", "))}
	}
	return nil
}

// checkVolumes enforces what local volumes allow: one service, one
// instance.
func (p *Plan) checkVolumes() []string {
	var problems []string
	users := map[string]string{}
	names := make([]string, 0, len(p.Services))
	for name := range p.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := p.Services[name]
		for _, m := range s.Mounts {
			if other, ok := users[m.Volume]; ok && other != name {
				problems = append(problems, fmt.Sprintf("volume %s is mounted by %s and %s: a volume is a disk attached to one instance", m.Volume, other, name))
			}
			users[m.Volume] = name
		}
		if len(s.Mounts) > 0 && s.Replicas > 1 {
			problems = append(problems, fmt.Sprintf("%s: mounts volume %s, so it can run one replica, not %d", name, s.Mounts[0].Volume, s.Replicas))
		}
	}
	return problems
}

// Volumes lists the Jokku volumes the plan mounts.
func (p *Plan) Volumes() []Mount {
	var out []Mount
	for _, s := range p.Services {
		out = append(out, s.Mounts...)
	}
	slices.SortFunc(out, func(a, b Mount) int { return strings.Compare(a.Volume, b.Volume) })
	return out
}

func absIn(dir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// inside checks path is in root, following symlinks, so a link committed to
// the repo can't reach the rest of the server.
func inside(root, path string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			real = filepath.Clean(path)
			realRoot = filepath.Clean(root)
		} else {
			return err
		}
	}
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside the repo", path)
	}
	return nil
}

func relTo(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(r)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
