package deploy

import (
	"archive/tar"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseProcfile(t *testing.T) {
	got, err := ParseProcfile("# comment\nweb: bin/server --port $PORT\n\nworker:  bin/worker\nrelease: bin/migrate: up\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"web": "bin/server --port $PORT", "worker": "bin/worker", "release": "bin/migrate: up"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if _, err := ParseProcfile("not a procfile line"); err == nil {
		t.Fatal("expected an error")
	}
}

func writeTar(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	f.Close()
	return path
}

func TestInspect(t *testing.T) {
	src := writeTar(t, map[string]string{
		"Dockerfile":              "FROM scratch\n",
		"Procfile":                "web: ./app\n",
		"svc/api/Dockerfile.prod": "FROM scratch\n",
		"svc/api/Procfile":        "web: ./api\nworker: ./jobs\n",
	})

	got, err := Inspect(src, Settings{DockerfilePath: "Dockerfile", ProcfilePath: "Procfile"})
	if err != nil || got.Dockerfile != "Dockerfile" || got.Procfile["web"] != "./app" {
		t.Fatalf("root: %+v, %v", got, err)
	}

	got, err = Inspect(src, Settings{BuildDir: "svc/api", DockerfilePath: "Dockerfile.prod", ProcfilePath: "Procfile"})
	if err != nil || got.Dockerfile != "svc/api/Dockerfile.prod" || len(got.Procfile) != 2 {
		t.Fatalf("build dir: %+v, %v", got, err)
	}

	_, err = Inspect(src, Settings{DockerfilePath: "Missing", ProcfilePath: "Procfile"})
	if err == nil || !strings.Contains(err.Error(), "no Missing found") {
		t.Fatalf("missing dockerfile: %v", err)
	}
}

func TestProcesses(t *testing.T) {
	got, err := Processes(nil, []string{"/entry.sh"}, []string{"serve", "--port", "80"})
	if err != nil || !reflect.DeepEqual(got, map[string][]string{"web": {"/entry.sh", "serve", "--port", "80"}}) {
		t.Fatalf("image command: %v, %v", got, err)
	}
	got, _ = Processes(map[string]string{"web": "bin/web -p $PORT", "worker": "bin/jobs"}, nil, []string{"ignored"})
	want := map[string][]string{"web": {"/bin/sh", "-c", "bin/web -p $PORT"}, "worker": {"/bin/sh", "-c", "bin/jobs"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("procfile: %v", got)
	}
	got, _ = Processes(map[string]string{"web": "rails s"}, []string{"docker-entrypoint.sh"}, nil)
	if !reflect.DeepEqual(got["web"], []string{"docker-entrypoint.sh", "/bin/sh", "-c", "rails s"}) {
		t.Fatalf("procfile with entrypoint: %v", got)
	}
	if _, err := Processes(nil, nil, nil); err == nil {
		t.Fatal("expected an error with nothing to run")
	}
}

func TestImageSource(t *testing.T) {
	b, err := ImageSource("ghcr.io/acme/api:v2")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source.tar")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(strings.NewReader(string(b)))
	h, err := tr.Next()
	if err != nil || h.Name != "Dockerfile" {
		t.Fatalf("first entry: %+v, %v", h, err)
	}
	body := make([]byte, h.Size)
	tr.Read(body)
	if string(body) != "FROM ghcr.io/acme/api:v2\n" {
		t.Fatalf("Dockerfile = %q", body)
	}
	// It inspects like any source, with the settings image deploys use.
	src, err := Inspect(path, Settings{DockerfilePath: "Dockerfile", ProcfilePath: "Procfile"})
	if err != nil || src.Dockerfile != "Dockerfile" || src.Procfile != nil {
		t.Fatalf("inspect: %+v, %v", src, err)
	}
}
