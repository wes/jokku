package compose

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repo writes files into a fresh source root.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func load(t *testing.T, root string, vars map[string]string) (*Plan, error) {
	t.Helper()
	return Load(context.Background(), root, root, "", "shop", vars)
}

const app = `
services:
  web:
    build:
      context: .
      args:
        RUBY: "3.4"
    ports: ["8080:3000"]
    environment:
      SECRET_KEY: ${SECRET_KEY}
      DATABASE_URL: postgres://shop:${DB_PASS:-devpass}@db:5432/shop
    depends_on:
      db:
        condition: service_healthy
  worker:
    build:
      context: .
      args:
        RUBY: "3.4"
    command: bin/worker --queue default
    deploy:
      replicas: 2
      resources:
        limits: {cpus: "0.5", memory: 512M}
    restart: unless-stopped
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: ${DB_PASS:-devpass}
    volumes:
      - pg_data:/var/lib/postgresql/data
    stop_grace_period: 30s
    healthcheck:
      test: ["CMD", "pg_isready"]
volumes:
  pg_data:
`

func TestLoadTranslatesAnApp(t *testing.T) {
	root := repo(t, map[string]string{
		"compose.yaml": app,
		"Dockerfile":   "FROM ruby\n",
		".env":         "SECRET_KEY=from-dotenv\nDB_PASS=dotenv-pass\n",
	})
	plan, err := load(t, root, map[string]string{"SECRET_KEY": "from-config"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.File != "compose.yaml" || plan.Web != "web" || len(plan.Services) != 3 {
		t.Fatalf("plan: file %s, web %q, %d services", plan.File, plan.Web, len(plan.Services))
	}

	web := plan.Services["web"]
	if web.Context != root || web.Dockerfile != filepath.Join(root, "Dockerfile") || web.Args["RUBY"] != "3.4" {
		t.Errorf("web build: %+v", web)
	}
	if web.Port != 3000 || !web.Published {
		t.Errorf("web port %d published %v", web.Port, web.Published)
	}
	// Config vars beat the repo's .env; .env fills in the rest.
	if !slices.Contains(web.Env, "SECRET_KEY=from-config") || !slices.Contains(web.Env, "DATABASE_URL=postgres://shop:dotenv-pass@db:5432/shop") {
		t.Errorf("web env: %v", web.Env)
	}
	if !slices.Equal(web.DependsOn, []string{"db"}) {
		t.Errorf("web depends on %v", web.DependsOn)
	}

	worker := plan.Services["worker"]
	if !slices.Equal(worker.Command, []string{"bin/worker", "--queue", "default"}) || worker.Entrypoint != nil {
		t.Errorf("worker command %q, entrypoint %q", worker.Command, worker.Entrypoint)
	}
	if worker.Replicas != 2 || worker.CPUs != 1 || worker.MemoryMB != 512 || worker.Restart != "always" {
		t.Errorf("worker: replicas %d, cpus %d, memory %d, restart %q", worker.Replicas, worker.CPUs, worker.MemoryMB, worker.Restart)
	}
	if web.BuildKey(root) != worker.BuildKey(root) {
		t.Error("web and worker build the same image but have different build keys")
	}

	db := plan.Services["db"]
	if db.Image != "postgres:17" || db.Built() || db.StopSecs != 30 || db.Port != 0 {
		t.Errorf("db: %+v", db)
	}
	if len(db.Mounts) != 1 || db.Mounts[0] != (Mount{Volume: "pg-data", Path: "/var/lib/postgresql/data"}) {
		t.Errorf("db mounts: %+v", db.Mounts)
	}
	if !slices.Contains(db.Env, "POSTGRES_PASSWORD=dotenv-pass") {
		t.Errorf("db env: %v", db.Env)
	}
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "db: its healthcheck command can't run") {
		t.Errorf("notes: %v", plan.Notes)
	}
	// Build keys don't depend on where the source was unpacked.
	again := repo(t, map[string]string{"compose.yaml": app, "Dockerfile": "FROM ruby\n"})
	plan2, err := load(t, again, map[string]string{"SECRET_KEY": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Services["web"].BuildKey(again) != web.BuildKey(root) {
		t.Error("the build key changed with the unpack directory")
	}
}

func TestLoadRefusesWhatCannotRun(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secrets.env"), []byte("TOKEN=stolen\n"), 0o600)

	for name, tc := range map[string]struct {
		files map[string]string
		link  [2]string // symlink in the repo -> target
		want  string
	}{
		"privileged":   {files: map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n    privileged: true\n"}, want: "privileged is not supported"},
		"host network": {files: map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n    network_mode: host\n"}, want: "network_mode is not supported"},
		"docker socket": {files: map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n    volumes: [\"/var/run/docker.sock:/var/run/docker.sock\"]\n"},
			want: "only files of the repo can be mounted"},
		"env_file outside": {files: map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n    env_file: " + filepath.Join(outside, "secrets.env") + "\n"},
			want: "is outside the repo"},
		"env_file through a symlink": {files: map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n    env_file: prod.env\n"},
			link: [2]string{"prod.env", filepath.Join(outside, "secrets.env")}, want: "is outside the repo"},
		"compose file through a symlink": {link: [2]string{"compose.yaml", filepath.Join(outside, "secrets.env")}, want: "is outside the repo"},
		"include":                        {files: map[string]string{"compose.yaml": "include: [other.yaml]\nservices:\n  a:\n    image: x\n"}, want: "include is not supported"},
		"extends file": {files: map[string]string{"compose.yaml": "services:\n  a:\n    extends: {file: base.yaml, service: b}\n"},
			want: "extends from another file"},
		"shared volume": {files: map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n    volumes: [\"data:/data\"]\n  b:\n    image: x\n    volumes: [\"data:/data\"]\nvolumes:\n  data:\n"},
			want: "volume data is mounted by a and b"},
		"replicas with a volume": {files: map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n    scale: 2\n    volumes: [\"data:/data\"]\nvolumes:\n  data:\n"},
			want: "can run one replica, not 2"},
		"two published": {files: map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n    ports: [\"80:80\"]\n  b:\n    image: x\n    ports: [\"81:81\"]\n"},
			want: "services a, b all publish ports"},
		"one-off service": {files: map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n    depends_on:\n      m: {condition: service_completed_successfully}\n  m:\n    image: x\n"},
			want: "one-off services"},
		"udp": {files: map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n    ports: [\"53:53/udp\"]\n"}, want: "only TCP"},
		"remote build context": {files: map[string]string{"compose.yaml": "services:\n  a:\n    build: https://github.com/acme/app.git\n"},
			want: "must be a directory of the repo"},
		"dotted name":       {files: map[string]string{"compose.yaml": "services:\n  a.b:\n    image: x\n"}, want: "the name must be"},
		"no compose file":   {files: map[string]string{"Dockerfile": "FROM x\n"}, want: "no compose file"},
		"bind into a build": {files: map[string]string{"compose.yaml": "services:\n  a:\n    build: .\n    volumes: [\"./conf:/conf\"]\n", "Dockerfile": "FROM x\n", "conf/x": "1"}, want: "copy it into the image"},
	} {
		t.Run(name, func(t *testing.T) {
			root := repo(t, tc.files)
			if tc.link[0] != "" {
				if err := os.Symlink(tc.link[1], filepath.Join(root, tc.link[0])); err != nil {
					t.Fatal(err)
				}
			}
			_, err := load(t, root, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "stolen") {
				t.Fatal("a file outside the repo was read")
			}
		})
	}
}

func TestLoadProfilesWebAndCopies(t *testing.T) {
	root := repo(t, map[string]string{
		"docker-compose.yml": `
services:
  site:
    image: nginx:alpine
    ports: ["80:80"]
    volumes: ["./nginx.conf:/etc/nginx/conf.d/default.conf:ro"]
  cache:
    image: redis:7
    expose: ["6379"]
  debug:
    image: busybox
    profiles: [debug]
`,
		"nginx.conf": "server {}\n",
	})
	plan, err := load(t, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Web != "site" {
		t.Errorf("web = %q, want the only service publishing ports", plan.Web)
	}
	if _, ok := plan.Services["debug"]; ok {
		t.Error("a service behind an inactive profile was included")
	}
	if c := plan.Services["site"].Copies; len(c) != 1 || c[0] != (Copy{Src: "nginx.conf", Dst: "/etc/nginx/conf.d/default.conf"}) {
		t.Errorf("site copies: %+v", c)
	}
	if plan.Services["cache"].Port != 6379 || plan.Services["cache"].Published {
		t.Errorf("cache: %+v", plan.Services["cache"])
	}

	plan, err = load(t, root, map[string]string{"COMPOSE_PROFILES": "debug"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plan.Services["debug"]; !ok {
		t.Error("COMPOSE_PROFILES did not enable the debug service")
	}

	// Nothing published, nothing named web: nothing routed, said so.
	root = repo(t, map[string]string{"compose.yaml": "services:\n  worker:\n    image: busybox\n"})
	plan, err = load(t, root, nil)
	if err != nil || plan.Web != "" || !strings.Contains(strings.Join(plan.Notes, " "), "no HTTP traffic is routed") {
		t.Fatalf("plan %+v, %v", plan, err)
	}
}

func TestLoadNamedFile(t *testing.T) {
	root := repo(t, map[string]string{"deploy/prod.yml": "services:\n  web:\n    image: nginx\n"})
	plan, err := Load(context.Background(), root, root, "deploy/prod.yml", "shop", nil)
	if err != nil || plan.File != "deploy/prod.yml" {
		t.Fatalf("plan %+v, %v", plan, err)
	}
	if _, err := Load(context.Background(), root, root, "missing.yml", "shop", nil); err == nil || !strings.Contains(err.Error(), "no missing.yml") {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := Load(context.Background(), root, root, "../../etc/passwd", "shop", nil); err == nil {
		t.Fatal("a path out of the repo was accepted")
	}
}
