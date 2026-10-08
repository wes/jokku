package cluster_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wes/jokku/internal/build"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// fakeBuilder stands in for BuildKit: it unpacks sources for real, and
// "builds" an image by writing a small root filesystem file whose content
// (and so artifact) depends on what was asked for.
type fakeBuilder struct {
	*build.Builder
	mu     sync.Mutex
	images map[string]build.Result // image config by image name, or "build" for Dockerfile builds
	made   []string                // what was built or pulled, in order
}

func newFakeBuilder(dir string) *fakeBuilder {
	return &fakeBuilder{Builder: &build.Builder{DataDir: dir}, images: map[string]build.Result{
		"build":       {Cmd: []string{"/app/server"}, Port: 5000, PortFrom: build.DefaultPortFrom},
		"postgres:17": {Cmd: []string{"postgres"}, Env: []string{"PGDATA=/var/lib/postgresql/data"}, Port: 5432, PortFrom: "from EXPOSE 5432", StopSignal: "SIGINT"},
		"redis:7":     {Cmd: []string{"redis-server"}, Port: 6379, PortFrom: "from EXPOSE 6379"},
	}}
}

func (f *fakeBuilder) BuildTarget(_ context.Context, _ int64, t build.Target, _ []build.RegistryLogin, _ func(string)) (*build.Result, error) {
	key := t.Image
	if key == "" {
		key = "build"
	}
	f.mu.Lock()
	res, ok := f.images[key]
	f.made = append(f.made, key)
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no such image %s", key)
	}
	content := fmt.Sprintf("rootfs of %s %v %v %s", key, t.Args, t.Copies, t.Stage)
	sum := sha256.Sum256([]byte(content))
	res.Artifact = filepath.Join(f.DataDir, "artifacts", hex.EncodeToString(sum[:10])+".ext4")
	os.MkdirAll(filepath.Dir(res.Artifact), 0o755)
	if err := os.WriteFile(res.Artifact, []byte(content), 0o644); err != nil {
		return nil, err
	}
	res.SHA256, res.Size = hex.EncodeToString(sum[:]), int64(len(content))
	return &res, nil
}

func (f *fakeBuilder) builds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.made...)
}

// composeDeploy deploys files (a compose file and what it needs) to app
// through the real pipeline, as a git push would.
func (h *harness) composeDeploy(app string, files map[string]string) error {
	h.t.Helper()
	ctx := h.ctx
	if _, err := h.st.App(ctx, app); err != nil {
		if _, err := h.st.CreateApp(ctx, app); err != nil {
			h.t.Fatal(err)
		}
	}
	h.st.SetProperty(ctx, app, "builder", "selected", "compose")
	h.st.SetProperty(ctx, app, "checks", "wait-to-retire", "0")
	h.st.UpdateDomains(ctx, app, types.DomainsPatch{Add: []string{app + ".example.test"}})
	d, err := h.st.CreateDeploy(ctx, app, "git", "abc1234", "test")
	if err != nil {
		h.t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg})
		tw.Write([]byte(files[n]))
	}
	tw.Close()
	src := filepath.Join(h.dir, "builds", fmt.Sprint(d.ID), "source.tar")
	os.MkdirAll(filepath.Dir(src), 0o700)
	if err := os.WriteFile(src, buf.Bytes(), 0o600); err != nil {
		h.t.Fatal(err)
	}
	var out []string
	err = h.pipe.Deploy(ctx, d, src, func(l string) { out = append(out, l) })
	status, msg := types.StatusSucceeded, ""
	if err != nil {
		status, msg = types.StatusFailed, err.Error()
		h.t.Logf("deploy output:\n%s", strings.Join(out, "\n"))
	}
	h.st.SetDeployStatus(ctx, d.ID, status, msg)
	return err
}

// ids maps each wanted instance of app to its ID, by name (web.1).
func (h *harness) ids(app string) map[string]string {
	insts, _ := h.st.Instances(h.ctx, app)
	out := map[string]string{}
	for _, in := range insts {
		if in.Desired == store.DesiredRunning {
			out[in.Name()] = in.ID
		}
	}
	return out
}

// running finds the VM of app's wanted process (web.1) on any node.
func (h *harness) running(app, process string) (string, *runningVM) {
	id := h.ids(app)[process]
	for name, n := range h.nodes {
		n.rt.mu.Lock()
		for _, s := range n.rt.running {
			if s.ID == id {
				vm := &runningVM{Artifact: s.Artifact, Argv: s.Guest.Argv, Env: s.Guest.Env, StopSignal: s.Guest.StopSignal, StopTimeout: s.Guest.StopTimeout}
				n.rt.mu.Unlock()
				return name, vm
			}
		}
		n.rt.mu.Unlock()
	}
	return "", nil
}

type runningVM struct {
	Artifact    string
	Argv, Env   []string
	StopSignal  string
	StopTimeout int
}

func (h *harness) setConfig(app string, vars map[string]string) {
	h.t.Helper()
	if _, _, err := h.st.UpdateConfigVars(h.ctx, app, types.ConfigPatch{Set: vars}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) ps(app, action string) []string {
	h.t.Helper()
	var out []string
	if err := h.pipe.PS(h.ctx, app, action, "test", func(l string) { out = append(out, l) }); err != nil {
		h.t.Fatalf("ps:%s %s: %v\n%s", action, app, err, strings.Join(out, "\n"))
	}
	return out
}

const shopCompose = `
services:
  web:
    build: .
    ports: ["8080:3000"]
    environment:
      SECRET: ${SECRET}
      DATABASE_URL: postgres://db:5432/shop
    depends_on: [db]
  worker:
    build: .
    command: ["bin/worker"]
    environment:
      SECRET: ${SECRET}
    deploy:
      replicas: 2
  db:
    image: postgres:17
    volumes: ["pgdata:/var/lib/postgresql/data"]
    stop_grace_period: 30s
volumes:
  pgdata:
`

func TestComposeDeploy(t *testing.T) {
	h := newHarness(t)
	h.st.CreateApp(h.ctx, "shop")
	h.setConfig("shop", map[string]string{"SECRET": "s1", "UNUSED": "x"})
	if err := h.composeDeploy("shop", map[string]string{"compose.yaml": shopCompose, "Dockerfile": "FROM ruby\n"}); err != nil {
		t.Fatal(err)
	}
	if got := h.builder.builds(); !slices.Equal(got, []string{"build", "postgres:17"}) {
		t.Errorf("built %v: web and worker share one build, db is pulled", got)
	}
	ids := h.ids("shop")
	for _, name := range []string{"web.1", "worker.1", "worker.2", "db.1"} {
		if ids[name] == "" {
			t.Fatalf("no %s; instances %v", name, ids)
		}
	}
	if len(ids) != 4 {
		t.Fatalf("instances %v", ids)
	}

	_, web := h.running("shop", "web.1")
	_, worker := h.running("shop", "worker.1")
	_, db := h.running("shop", "db.1")
	if web == nil || worker == nil || db == nil {
		t.Fatal("not every service is running")
	}
	if web.Artifact != worker.Artifact || web.Artifact == db.Artifact {
		t.Errorf("artifacts: web %s, worker %s, db %s", web.Artifact, worker.Artifact, db.Artifact)
	}
	if !slices.Equal(worker.Argv, []string{"bin/worker"}) || !slices.Equal(db.Argv, []string{"postgres"}) {
		t.Errorf("argv: worker %v, db %v", worker.Argv, db.Argv)
	}
	// Services get the compose file's environment; config vars only fill
	// in ${VAR}.
	for _, want := range []string{"SECRET=s1", "DATABASE_URL=postgres://db:5432/shop", "PORT=3000"} {
		if !slices.Contains(web.Env, want) {
			t.Errorf("web env lacks %s: %v", want, web.Env)
		}
	}
	if slices.ContainsFunc(web.Env, func(e string) bool { return strings.HasPrefix(e, "UNUSED=") }) {
		t.Error("a config var the compose file doesn't use reached the service")
	}
	if slices.ContainsFunc(worker.Env, func(e string) bool { return strings.HasPrefix(e, "PORT=") }) {
		t.Error("the worker listens on nothing but got $PORT")
	}
	if !slices.Contains(db.Env, "PGDATA=/var/lib/postgresql/data") || db.StopSignal != "SIGINT" || db.StopTimeout != 30 {
		t.Errorf("db: env %v, stop %s after %ds", db.Env, db.StopSignal, db.StopTimeout)
	}

	// The database started before the web service that depends on it.
	order := h.nodes["control"].rt.order
	if slices.Index(order, "shop/db.1") > slices.Index(order, "shop/web.1") {
		t.Errorf("start order %v: db must come before web", order)
	}
	// Traffic goes to the web service's port; names resolve.
	insts, _ := h.st.Instances(h.ctx, "shop")
	addr := map[string]string{}
	for _, in := range insts {
		addr[in.Name()] = in.IP
	}
	eventually(t, 3*time.Second, "routes and names", func() error {
		n := h.nodes["control"]
		if got := n.proxy.upstreams("shop"); !slices.Equal(got, []string{addr["web.1"] + ":3000"}) {
			return fmt.Errorf("routes %v", got)
		}
		z := n.resolver.get()
		if !slices.Equal(z.Records["shop.internal"], []string{addr["web.1"]}) || !slices.Equal(z.Records["db.shop.internal"], []string{addr["db.1"]}) {
			return fmt.Errorf("names %v", z.Records)
		}
		return nil
	})
	v := h.volume("shop", "pgdata")
	if len(v.Mounts) != 1 || v.Mounts[0] != (types.VolumeMount{ProcessType: "db", Path: "/var/lib/postgresql/data"}) {
		t.Errorf("pgdata mounts %+v", v.Mounts)
	}

	// A config change restarts only the services that use it.
	starts := h.nodes["control"].rt.startCount()
	h.setConfig("shop", map[string]string{"SECRET": "s2"})
	out := h.ps("shop", "apply")
	after := h.ids("shop")
	if after["db.1"] != ids["db.1"] {
		t.Error("the database restarted for a config change it doesn't use")
	}
	if after["web.1"] == ids["web.1"] || after["worker.1"] == ids["worker.1"] {
		t.Error("services using the changed var were not restarted")
	}
	if _, web := h.running("shop", "web.1"); web == nil || !slices.Contains(web.Env, "SECRET=s2") {
		t.Errorf("web did not get the new value")
	}
	if !strings.Contains(strings.Join(out, "\n"), "Unchanged, left running: db") {
		t.Errorf("output: %s", strings.Join(out, "\n"))
	}
	if got := h.nodes["control"].rt.startCount() - starts; got != 3 {
		t.Errorf("%d VMs started for the config change, want 3 (web and two workers)", got)
	}

	// A var no service uses restarts nothing.
	ids = h.ids("shop")
	h.setConfig("shop", map[string]string{"UNUSED": "y"})
	h.ps("shop", "apply")
	if after := h.ids("shop"); !maps(after, ids) {
		t.Errorf("an unused config var restarted services: %v -> %v", ids, after)
	}

	// A new deploy that only changes the worker leaves web and db alone.
	h.builder.mu.Lock()
	h.builder.images["redis:7"] = build.Result{Cmd: []string{"redis-server"}, Port: 6379, PortFrom: "from EXPOSE 6379"}
	h.builder.mu.Unlock()
	changed := strings.Replace(shopCompose, `command: ["bin/worker"]`, `command: ["bin/worker", "--fast"]`, 1)
	if err := h.composeDeploy("shop", map[string]string{"compose.yaml": changed, "Dockerfile": "FROM ruby\n"}); err != nil {
		t.Fatal(err)
	}
	after = h.ids("shop")
	if after["web.1"] != ids["web.1"] || after["db.1"] != ids["db.1"] || after["worker.1"] == ids["worker.1"] {
		t.Errorf("deploying a worker change: %v -> %v", ids, after)
	}

	// ps:restart restarts everything.
	ids = after
	h.ps("shop", "restart")
	after = h.ids("shop")
	for name, id := range ids {
		if after[name] == id {
			t.Errorf("ps:restart left %s running", name)
		}
	}
	h.noDoubleAttach()
}

func maps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestComposeDeployRefusesWhatCannotRun(t *testing.T) {
	h := newHarness(t)
	err := h.composeDeploy("bad", map[string]string{"compose.yaml": "services:\n  a:\n    image: redis:7\n    privileged: true\n"})
	if err == nil || !strings.Contains(err.Error(), "privileged is not supported") {
		t.Fatalf("got %v", err)
	}
	if len(h.builder.builds()) != 0 {
		t.Error("something was built for a compose file that can't run")
	}
}

func TestApplyRestartsOnlyChangedProcessTypes(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx
	h.deploy("site", 1)
	// Give the release a worker too, and run one.
	cur, _ := h.st.CurrentRelease(ctx, "site")
	next := *cur
	next.Processes = map[string][]string{"web": {"/bin/app"}, "worker": {"/bin/worker"}}
	if err := h.st.CreateRelease(ctx, &next); err != nil {
		t.Fatal(err)
	}
	h.st.SetCurrentRelease(ctx, "site", next.ID)
	h.st.Scale(ctx, "site", map[string]int{"worker": 1})
	h.ps("site", "restart")
	ids := h.ids("site")

	// Scaling the worker leaves web alone (as Dokku's ps:scale does).
	h.st.Scale(ctx, "site", map[string]int{"worker": 2})
	h.ps("site", "apply")
	after := h.ids("site")
	if after["web.1"] != ids["web.1"] || after["worker.2"] == "" {
		t.Errorf("scaling the worker: %v -> %v", ids, after)
	}
	// A config var reaches every process of a Dockerfile app, so all restart.
	ids = after
	h.setConfig("site", map[string]string{"A": "1"})
	h.ps("site", "apply")
	after = h.ids("site")
	if after["web.1"] == ids["web.1"] || after["worker.1"] == ids["worker.1"] {
		t.Errorf("a config change left a process running: %v -> %v", ids, after)
	}
	// Nothing changed: nothing restarts.
	ids = after
	h.ps("site", "apply")
	if after := h.ids("site"); !maps(after, ids) {
		t.Errorf("apply with no changes restarted something: %v -> %v", ids, after)
	}
}
