package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/api"
	"github.com/wes/jokku/internal/backup"
	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/gitrepo"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// stubDeployer stands in for the microVM runtime, which needs Linux and KVM.
type stubDeployer struct{}

func (stubDeployer) Deploy(context.Context, *types.Deploy, string, func(string)) error { return nil }
func (stubDeployer) PS(context.Context, string, string, string, func(string)) error    { return nil }
func (stubDeployer) Logs(context.Context, string, types.LogOptions, func(string)) error {
	return nil
}
func (stubDeployer) RoutesChanged() {}

// startAPI serves a fresh API on a unix socket and points the CLI at it.
func startAPI(t *testing.T) {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes, so avoid t.TempDir's long
	// names.
	dir, err := os.MkdirTemp("", "jk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.New(api.Config{
		Store:    st,
		Git:      &gitrepo.Manager{Dir: filepath.Join(dir, "git"), Exe: "/usr/local/bin/jokku"},
		Deployer: stubDeployer{},
		Log:      log,
		DataDir:  dir,
		Cluster: cluster.New(&cluster.Controller{
			Store: st, Log: log, Self: "test", ClusterCIDR: netip.MustParsePrefix("10.210.0.0/16"),
		}),
	})
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })
	t.Setenv("JOKKU_SOCKET", sock)
}

func jokku(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Main(context.Background(), args, IO{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut})
	return out.String(), errOut.String(), code
}

func mustJokku(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, code := jokku(t, "", args...)
	if code != 0 {
		t.Fatalf("jokku %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

func TestCLIAgainstAPI(t *testing.T) {
	startAPI(t)

	mustJokku(t, "apps:create", "web")
	if out := mustJokku(t, "apps:list"); !strings.Contains(out, "\nweb\n") {
		t.Fatalf("apps:list = %q", out)
	}

	mustJokku(t, "config:set", "web", "A=1", "B=two words")
	if out := mustJokku(t, "config:get", "web", "B"); out != "two words\n" {
		t.Fatalf("config:get = %q", out)
	}
	if out := mustJokku(t, "config:get", "--quoted", "web", "B"); out != "'two words'\n" {
		t.Fatalf("config:get --quoted = %q", out)
	}
	mustJokku(t, "config:set", "--global", "G=g")
	if out := mustJokku(t, "config:keys", "--merged", "web"); out != "A\nB\nG\n" {
		t.Fatalf("config:keys --merged = %q", out)
	}
	mustJokku(t, "config:unset", "web", "A")
	if _, _, code := jokku(t, "", "config:get", "web", "A"); code != 1 {
		t.Fatal("config:get of an unset key should exit 1")
	}

	mustJokku(t, "domains:set-global", "example.com")
	mustJokku(t, "apps:create", "api")
	if out := mustJokku(t, "domains:report", "api", "--domains-app-vhosts"); out != "api.example.com\n" {
		t.Fatalf("default domain = %q", out)
	}
	mustJokku(t, "domains:add", "web", "Shop.Example.org")
	if _, errOut, code := jokku(t, "", "domains:add", "api", "shop.example.org"); code == 0 || !strings.Contains(errOut, "already exists") {
		t.Fatalf("duplicate domain: exit %d, %q", code, errOut)
	}

	mustJokku(t, "ps:scale", "web", "web=2", "worker=1")
	mustJokku(t, "resource:limit", "web", "--memory", "1g", "--process-type", "worker")
	if out := mustJokku(t, "resource:report", "web", "--worker-memory"); out != "1g\n" {
		t.Fatalf("worker memory = %q", out)
	}

	mustJokku(t, "git:set", "web", "deploy-branch", "prod")
	if out := mustJokku(t, "git:report", "web", "--git-computed-deploy-branch"); out != "prod\n" {
		t.Fatalf("deploy branch = %q", out)
	}
	if _, errOut, code := jokku(t, "", "git:set", "web", "nope", "x"); code == 0 || !strings.Contains(errOut, "valid properties") {
		t.Fatalf("bad property: exit %d, %q", code, errOut)
	}

	if _, errOut, code := jokku(t, "", "apps:destroy", "api"); code == 0 || !strings.Contains(errOut, "--force") {
		t.Fatalf("destroy without a terminal must require --force: exit %d, %q", code, errOut)
	}
	mustJokku(t, "apps:destroy", "--force", "api")
	if _, _, code := jokku(t, "", "apps:exists", "api"); code != 1 {
		t.Fatal("destroyed app still exists")
	}
}

func TestStorageCLI(t *testing.T) {
	startAPI(t)
	mustJokku(t, "apps:create", "db")

	// Dokku muscle memory gets pointed at named volumes.
	if _, errOut, code := jokku(t, "", "storage:mount", "db", "/var/lib/dokku/data/storage/db:/data"); code == 0 || !strings.Contains(errOut, "named disks") {
		t.Fatalf("host path: exit %d, %q", code, errOut)
	}
	out := mustJokku(t, "storage:mount", "db", "pg:/var/lib/postgresql/data", "--size", "20g")
	if !strings.Contains(out, "Created volume pg (local, 20g)") || !strings.Contains(out, "Mounted volume pg at /var/lib/postgresql/data in web") {
		t.Fatalf("storage:mount = %q", out)
	}
	if out := mustJokku(t, "storage:list", "db"); !strings.Contains(out, "pg") || !strings.Contains(out, "web:/var/lib/postgresql/data") || !strings.Contains(out, "new") {
		t.Fatalf("storage:list = %q", out)
	}
	if out := mustJokku(t, "storage:report", "db", "--storage-mounts"); out != "web pg:/var/lib/postgresql/data\n" {
		t.Fatalf("storage:report = %q", out)
	}

	// A local volume belongs to one instance.
	if _, errOut, code := jokku(t, "", "ps:scale", "db", "web=2"); code == 0 || !strings.Contains(errOut, "one instance") {
		t.Fatalf("scaling past one: exit %d, %q", code, errOut)
	}
	mustJokku(t, "ps:scale", "db", "worker=3")
	if _, errOut, code := jokku(t, "", "storage:mount", "db", "cache:/cache", "--process-type", "worker"); code == 0 || !strings.Contains(errOut, "runs 3 instances") {
		t.Fatalf("mounting into a scaled process: exit %d, %q", code, errOut)
	}
	if _, errOut, code := jokku(t, "", "storage:mount", "db", "pg:/other"); code == 0 || !strings.Contains(errOut, "already mounted") {
		t.Fatalf("mounting twice: exit %d, %q", code, errOut)
	}
	if _, errOut, code := jokku(t, "", "storage:mount", "db", "x:/proc/x"); code == 0 || !strings.Contains(errOut, "cannot be mounted") {
		t.Fatalf("reserved path: exit %d, %q", code, errOut)
	}

	mustJokku(t, "storage:resize", "db", "pg", "30g")
	if _, errOut, code := jokku(t, "", "storage:resize", "db", "pg", "1g"); code == 0 || !strings.Contains(errOut, "only grow") {
		t.Fatalf("shrinking: exit %d, %q", code, errOut)
	}

	if _, errOut, code := jokku(t, "", "storage:destroy", "--force", "db", "pg"); code == 0 || !strings.Contains(errOut, "unmount it first") {
		t.Fatalf("destroying a mounted volume: exit %d, %q", code, errOut)
	}
	mustJokku(t, "storage:unmount", "db", "pg")
	if _, errOut, code := jokku(t, "", "storage:destroy", "db", "pg"); code == 0 || !strings.Contains(errOut, "--force") {
		t.Fatalf("destroy without a terminal must require --force: exit %d, %q", code, errOut)
	}
	mustJokku(t, "storage:destroy", "--force", "db", "pg")
	if out := mustJokku(t, "storage:list", "db"); !strings.Contains(out, "none") {
		t.Fatalf("storage:list after destroy = %q", out)
	}
}

func TestImageDeployAndRegistryCLI(t *testing.T) {
	startAPI(t)
	// builder:image creates the app and deploys the image (the stub
	// deployer stands in for BuildKit), which makes it an image app.
	out := mustJokku(t, "builder:image", "cache", "public.ecr.aws/docker/library/redis:7")
	if !strings.Contains(out, "Creating cache") || !strings.Contains(out, "Deploying image public.ecr.aws/docker/library/redis:7 to cache") ||
		!strings.Contains(out, "cache now runs an image: pushes no longer deploy it") {
		t.Fatalf("builder:image = %q", out)
	}
	if out := mustJokku(t, "builder:image", "cache", "redis:7"); strings.Contains(out, "now runs an image") {
		t.Errorf("a second image said the app switched: %q", out)
	}
	out = mustJokku(t, "builder:report", "cache")
	if !strings.Contains(out, "Builder type:") || !strings.Contains(out, "image") || !strings.Contains(out, "redis:7") {
		t.Fatalf("builder:report = %q", out)
	}
	if _, errOut, code := jokku(t, "", "builder:image", "cache", "redis:7\nRUN rm -rf /"); code == 0 || !strings.Contains(errOut, "not an image reference") {
		t.Fatalf("bad image: exit %d, %q", code, errOut)
	}
	if out := mustJokku(t, "builder:report", "cache", "--builder-image"); out != "redis:7\n" {
		t.Errorf("a failed image deploy changed the image to %q", out)
	}

	// Pushes don't deploy an image app, until it builds from git again.
	api := client.New(client.Target{Socket: os.Getenv("JOKKU_SOCKET")}, "me")
	push := func() error {
		return api.Deploy(context.Background(), "cache", "git", "abc1234", strings.NewReader(""), func(types.Event) {})
	}
	if err := push(); err == nil || !strings.Contains(err.Error(), "cache runs the image redis:7") || !strings.Contains(err.Error(), "jokku builder:dockerfile cache") {
		t.Fatalf("push to an image app: %v", err)
	}
	mustJokku(t, "builder:dockerfile", "cache")
	if err := push(); err != nil {
		t.Fatalf("push after builder:dockerfile: %v", err)
	}
	if out := mustJokku(t, "builder:report", "cache", "--builder-image"); out != "\n" {
		t.Errorf("builder:dockerfile kept the image %q", out)
	}

	if out, errOut, code := jokku(t, "ghp_secret\n", "registry:login", "ghcr.io", "me"); code != 0 || !strings.Contains(out, "Logged in to ghcr.io as me") {
		t.Fatalf("registry:login from stdin: exit %d, %q %q", code, out, errOut)
	}
	mustJokku(t, "registry:login", "--global", "https://Index.Docker.IO/", "hub", "pw")
	out = mustJokku(t, "registry:report")
	if !strings.Contains(out, "ghcr.io") || !strings.Contains(out, "docker.io") || strings.Contains(out, "secret") {
		t.Fatalf("registry:report = %q", out)
	}
	if _, errOut, code := jokku(t, "", "registry:login", "not a server", "me", "pw"); code == 0 || !strings.Contains(errOut, "not a registry server") {
		t.Fatalf("bad server: exit %d, %q", code, errOut)
	}
	mustJokku(t, "registry:logout", "ghcr.io")
	if _, _, code := jokku(t, "", "registry:logout", "ghcr.io"); code == 0 {
		t.Fatal("logging out twice should fail")
	}
}

func TestSSHKeysCLI(t *testing.T) {
	startAPI(t)
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKo6wvodnpAVyBluWuHPcgrJTcSBDE9Ozy8NPmmowyTO me@laptop\n"
	out, errOut, code := jokku(t, key, "ssh-keys:add", "admin")
	if code != 0 || !strings.HasPrefix(out, "SHA256:") {
		t.Fatalf("ssh-keys:add: exit %d, %q %q", code, out, errOut)
	}
	if out := mustJokku(t, "ssh-keys:list"); !strings.Contains(out, `NAME="admin"`) {
		t.Fatalf("ssh-keys:list = %q", out)
	}
	if _, _, code := jokku(t, key, "ssh-keys:add", "dup"); code == 0 {
		t.Fatal("adding the same key twice should fail")
	}
}

func TestResolveApp(t *testing.T) {
	configSet := commands["config:set"]
	configShow := commands["config:show"]
	appsList := commands["apps:list"]
	tests := []struct {
		name               string
		cmd                *Command
		explicit, inferred string
		args               []string
		wantApp            string
		wantArgs           []string
	}{
		{"positional", configSet, "", "", []string{"web", "A=1"}, "web", []string{"A=1"}},
		{"explicit flag", configSet, "web", "", []string{"A=1"}, "web", []string{"A=1"}},
		{"inferred", configSet, "", "web", []string{"A=1"}, "web", []string{"A=1"}},
		{"inferred, app repeated", configSet, "", "web", []string{"web", "A=1"}, "web", []string{"A=1"}},
		{"inferred, other app by arity", configShow, "", "web", []string{"api"}, "api", []string{}},
		{"no app", appsList, "", "web", nil, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Context{Cmd: tt.cmd, flags: map[string]string{}}
			app, args, err := resolveApp(tt.cmd, c, tt.explicit, tt.inferred, tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if app != tt.wantApp || (len(args) != 0 || len(tt.wantArgs) != 0) && !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("got %q %q, want %q %q", app, args, tt.wantApp, tt.wantArgs)
			}
		})
	}
}

func TestSplitWords(t *testing.T) {
	tests := map[string][]string{
		`git-receive-pack 'myapp'`:           {"git-receive-pack", "myapp"},
		`config:set app "A=b c" 'D=e f' G=h`: {"config:set", "app", "A=b c", "D=e f", "G=h"},
		`config:set app A=it\'s`:             {"config:set", "app", "A=it's"},
		`config:set app "A=say \"hi\""`:      {"config:set", "app", `A=say "hi"`},
		`  apps:list  `:                      {"apps:list"},
		`x ''`:                               {"x", ""},
	}
	for in, want := range tests {
		got, err := splitWords(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("splitWords(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := splitWords(`a "unterminated`); err == nil {
		t.Error("expected an error for an unterminated quote")
	}
}

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]int{"512": 512, "512m": 512, "512MB": 512, "1g": 1024, "1.5G": 1536, "2GiB": 2048, "1048576k": 1024, "1t": 1 << 20} {
		if got, err := parseMemory(in); err != nil || got != want {
			t.Errorf("parseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parseMemory("lots"); err == nil {
		t.Error("expected an error")
	}
}

func TestEnterPassesCommandArgsThrough(t *testing.T) {
	for _, tt := range []struct {
		args     []string
		wantArgs []string
		root     bool
	}{
		{[]string{"app", "web", "ls", "-la", "--root"}, []string{"app", "web", "ls", "-la", "--root"}, false},
		{[]string{"--root", "app", "web", "id", "-u"}, []string{"app", "web", "id", "-u"}, true},
		{[]string{"app", "--root", "web.2", "sh", "-c", "exit 7"}, []string{"app", "web.2", "sh", "-c", "exit 7"}, true},
	} {
		c := &Context{Cmd: commands["enter"], flags: map[string]string{}}
		got, err := parseFlags(c, tt.args)
		if err != nil || !reflect.DeepEqual(got, tt.wantArgs) || c.Bool("root") != tt.root {
			t.Errorf("parseFlags(%q) = %q (root %v), %v; want %q (root %v)", tt.args, got, c.Bool("root"), err, tt.wantArgs, tt.root)
		}
	}
}

func TestBuilderCLI(t *testing.T) {
	startAPI(t)
	// builder:compose picks compose and its file in one go, creating the app.
	out := mustJokku(t, "builder:compose", "shop", "docker-compose.prod.yml")
	if !strings.Contains(out, "Creating shop") || !strings.Contains(out, "shop now builds from docker-compose.prod.yml") || !strings.Contains(out, "Deploy it with git push") {
		t.Fatalf("builder:compose = %q", out)
	}
	report := func(flag string) string {
		t.Helper()
		return strings.TrimSuffix(mustJokku(t, "builder:report", "shop", "--builder-"+flag), "\n")
	}
	if report("type") != "compose" || report("file") != "docker-compose.prod.yml" || report("procfile") != "Procfile" {
		t.Fatalf("after builder:compose: %q", mustJokku(t, "builder:report", "shop"))
	}
	mustJokku(t, "builder:compose", "shop")
	if got := report("file"); got != "the first of compose.yaml, compose.yml, docker-compose.yaml, docker-compose.yml" {
		t.Errorf("default compose file = %q", got)
	}
	mustJokku(t, "builder:dockerfile", "shop", "docker/prod.Dockerfile")
	if report("type") != "dockerfile" || report("file") != "docker/prod.Dockerfile" {
		t.Fatalf("after builder:dockerfile <path>: %q", mustJokku(t, "builder:report", "shop"))
	}
	mustJokku(t, "builder:dockerfile", "shop")
	if got := report("file"); got != "Dockerfile" {
		t.Errorf("builder:dockerfile without a path left %q", got)
	}

	// builder:set takes the settings that go with any builder...
	mustJokku(t, "builder:set", "shop", "build-dir", "api")
	mustJokku(t, "builder:set", "--global", "procfile", "Procfile.all")
	if report("build-dir") != "api" || report("procfile") != "Procfile.all" {
		t.Errorf("builder:set: %q", mustJokku(t, "builder:report", "shop"))
	}
	mustJokku(t, "builder:set", "shop", "procfile", "Procfile.prod")
	if report("procfile") != "Procfile.prod" {
		t.Errorf("an app's procfile doesn't override the global one")
	}
	if out := mustJokku(t, "builder:report", "--global", "--builder-global-procfile"); out != "Procfile.all\n" {
		t.Errorf("global report = %q", out)
	}
	// ...but not the builder itself, which has its own commands.
	for _, key := range []string{"type", "file", "image"} {
		if _, errOut, code := jokku(t, "", "builder:set", "shop", key, "compose"); code == 0 || !strings.Contains(errOut, "is set with jokku builder:") {
			t.Errorf("builder:set %s: exit %d, %q", key, code, errOut)
		}
	}
	if _, errOut, _ := jokku(t, "", "builder:set", "shop", "nope", "x"); !strings.Contains(errOut, "valid properties: build-dir, procfile") {
		t.Errorf("builder:set nope: %q", errOut)
	}

	// The old commands are gone.
	for _, args := range [][]string{
		{"builder-compose:set", "shop", "compose-file", "x.yml"},
		{"builder-dockerfile:set", "shop", "dockerfile-path", "x"},
		{"git:from-image", "shop", "redis:7"},
		{"ps:set", "shop", "procfile-path", "x"},
	} {
		if _, _, code := jokku(t, "", args...); code == 0 {
			t.Errorf("%s still works", args[0])
		}
	}
	help := mustJokku(t, "help", "builder")
	for _, cmd := range []string{"builder:dockerfile", "builder:compose", "builder:image", "builder:set", "builder:report"} {
		if !strings.Contains(help, cmd) {
			t.Errorf("help builder lacks %s:\n%s", cmd, help)
		}
	}
}

func TestBackupsCLI(t *testing.T) {
	startAPI(t)
	bucket := t.TempDir()
	open := backup.Open
	backup.Open = func(types.BackupDestination) (backup.Store, error) { return backup.Dir(bucket), nil }
	t.Cleanup(func() { backup.Open = open })
	mustJokku(t, "apps:create", "db")
	mustJokku(t, "storage:mount", "db", "pg:/var/lib/postgresql/data")

	// The first encrypted destination shows the new key, and wants it saved.
	add := []string{"backups:destination-add", "tigris", "--endpoint", "fly.storage.tigris.dev", "--bucket", "b", "--access-key-id", "id"}
	out, errOut, code := jokku(t, "s3cret\n", add...)
	if code == 0 || !strings.Contains(out, "jbk1_") || !strings.Contains(errOut, "nothing can restore them without it") || !strings.Contains(errOut, "pass --force once it is saved") {
		t.Fatalf("destination-add before saving the key: exit %d\n%s%s", code, out, errOut)
	}
	key := regexp.MustCompile(`jbk1_\S+`).FindString(out)
	out, errOut, code = jokku(t, "s3cret\n", append(add, "--force")...)
	if code != 0 || !strings.Contains(out, key) || !strings.Contains(out, "Added tigris: bucket b at https://fly.storage.tigris.dev, encrypted") ||
		!strings.Contains(out, "The cluster itself is backed up there every hour") {
		t.Fatalf("destination-add --force: exit %d\n%s%s", code, out, errOut)
	}
	if out := mustJokku(t, "backups:cluster", "tigris", "--every", "6h"); !strings.Contains(out, "Backing up the cluster to tigris, under jokku/cluster, every 6h") {
		t.Errorf("backups:cluster = %q", out)
	}
	if out := mustJokku(t, "backups:cluster-list"); !strings.Contains(out, "none yet") {
		t.Errorf("backups:cluster-list = %q", out)
	}
	if out := mustJokku(t, "backups:report"); !strings.Contains(out, "Cluster backups") || !strings.Contains(out, "every 6h") {
		t.Errorf("backups:report without an app = %q", out)
	}
	if _, errOut, code := jokku(t, "", "backups:cluster", "tigris", "--auto-restore", "on"); code == 0 {
		t.Errorf("backups:cluster took --auto-restore: %q", errOut)
	}
	if out := mustJokku(t, "backups:key"); !strings.Contains(out, key) {
		t.Errorf("backups:key doesn't show the key: %q", out)
	}
	if out := mustJokku(t, "backups:destinations"); !strings.Contains(out, "tigris") || strings.Contains(out, "s3cret") {
		t.Errorf("backups:destinations = %q", out)
	}
	if _, errOut, code := jokku(t, "", append(add[:2:2], "--endpoint", "ftp://x", "--bucket", "b", "--access-key-id", "id", "--secret-access-key", "s")...); code == 0 || !strings.Contains(errOut, "not an S3 endpoint") {
		t.Errorf("a bad endpoint: exit %d, %q", code, errOut)
	}

	if out := mustJokku(t, "backups:set", "db", "pg", "tigris"); !strings.Contains(out, "Backing up volume pg of db to tigris, under jokku/db/pg") {
		t.Fatalf("backups:set = %q", out)
	}
	if _, errOut, code := jokku(t, "", "backups:set", "db", "pg", "tigris", "../up"); code == 0 || !strings.Contains(errOut, "not a path") {
		t.Errorf("a bad path: exit %d, %q", code, errOut)
	}
	report := func(flag string) string {
		t.Helper()
		return strings.TrimSuffix(mustJokku(t, "backups:report", "db", "--backups-"+flag), "\n")
	}
	if report("schedule") != "every 15m" || report("keep") != "every backup from the last 24h, and the last one of each day for 30 days" {
		t.Errorf("default schedule: %q, keeping %q", report("schedule"), report("keep"))
	}
	mustJokku(t, "backups:set", "db", "pg", "tigris", "--every", "1h", "--keep-recent", "2d", "--keep-daily", "7")
	mustJokku(t, "backups:set", "db", "pg", "tigris") // keeps the schedule it has
	if report("schedule") != "every 1h" || report("keep") != "every backup from the last 2d, and the last one of each day for 7 days" {
		t.Errorf("set schedule: %q, keeping %q", report("schedule"), report("keep"))
	}
	if out := mustJokku(t, "backups:set", "db", "pg", "tigris", "--every", "off"); !strings.Contains(out, "when asked") {
		t.Errorf("--every off: %q", out)
	}
	if got := report("auto-restore"); got != "it is restored onto another node from its latest backup after 5m" {
		t.Errorf("default auto restore: %q", got)
	}
	mustJokku(t, "backups:set", "db", "pg", "tigris", "--auto-restore", "off")
	if got := report("auto-restore"); !strings.Contains(got, "waits for that node") {
		t.Errorf("--auto-restore off: %q", got)
	}
	if _, errOut, code := jokku(t, "", "backups:set", "db", "pg", "tigris", "--auto-restore", "maybe"); code == 0 || !strings.Contains(errOut, "on or off") {
		t.Errorf("--auto-restore maybe: exit %d, %q", code, errOut)
	}
	if _, errOut, code := jokku(t, "", "storage:discard-old-copy", "db", "pg", "--force"); code == 0 || !strings.Contains(errOut, "no node keeps an old copy") {
		t.Errorf("discarding a copy nobody keeps: exit %d, %q", code, errOut)
	}
	for _, bad := range [][]string{{"--every", "1m"}, {"--every", "soon"}, {"--keep-recent", "-1h"}, {"--keep-daily", "x"}} {
		if _, errOut, code := jokku(t, "", append([]string{"backups:set", "db", "pg", "tigris"}, bad...)...); code == 0 {
			t.Errorf("backups:set %v passed: %q", bad, errOut)
		}
	}
	if out := mustJokku(t, "backups:report", "db", "--backups-path"); out != "jokku/db/pg\n" {
		t.Errorf("backups:report = %q", out)
	}
	if out := mustJokku(t, "backups:list", "db", "pg"); !strings.Contains(out, "none yet") {
		t.Errorf("backups:list = %q", out)
	}
	// The volume has no disk yet: nothing to back up.
	if _, errOut, code := jokku(t, "", "backups:run", "db", "pg"); code == 0 || !strings.Contains(errOut, "holds nothing yet") {
		t.Errorf("backups:run of an unused volume: exit %d, %q", code, errOut)
	}
	if _, errOut, code := jokku(t, "", "backups:restore", "db", "pg"); code == 0 || !strings.Contains(errOut, "--force") {
		t.Errorf("backups:restore without a terminal must require --force: exit %d, %q", code, errOut)
	}

	if _, errOut, code := jokku(t, "", "backups:destination-remove", "tigris"); code == 0 || !strings.Contains(errOut, "db pg") {
		t.Errorf("removing a destination in use: exit %d, %q", code, errOut)
	}
	mustJokku(t, "backups:unset", "db", "pg")
	if _, errOut, code := jokku(t, "", "backups:destination-remove", "tigris"); code == 0 || !strings.Contains(errOut, "the cluster is backed up to tigris") {
		t.Errorf("removing the cluster's destination: exit %d, %q", code, errOut)
	}
	mustJokku(t, "backups:cluster-unset")
	mustJokku(t, "backups:destination-remove", "tigris")
	if out := mustJokku(t, "backups:destinations"); !strings.Contains(out, "none") {
		t.Errorf("backups:destinations after removing = %q", out)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		15 * time.Minute: "15m", time.Hour: "1h", 90 * time.Minute: "1h30m", 24 * time.Hour: "24h",
		7 * 24 * time.Hour: "7d", 30 * time.Second: "30s",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %q, want %q", d, got, want)
		}
	}
}
