package build

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDockerConfig(t *testing.T) {
	if env, err := dockerConfig(t.TempDir(), nil); err != nil || env != nil {
		t.Fatalf("no logins should leave the environment alone: %v, %v", env, err)
	}
	dir := filepath.Join(t.TempDir(), "docker")
	env, err := dockerConfig(dir, []RegistryLogin{
		{Server: "ghcr.io", Username: "me", Password: "tok"},
		{Server: "docker.io", Username: "hub", Password: "pw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "DOCKER_CONFIG="+dir) {
		t.Fatalf("env does not point at the config: %v", env)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "config.json")); info.Mode().Perm() != 0o600 {
		t.Errorf("config.json mode %v", info.Mode().Perm())
	}
	var cfg struct {
		Auths map[string]struct{ Auth string }
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	for server, want := range map[string]string{"ghcr.io": "me:tok", "https://index.docker.io/v1/": "hub:pw"} {
		got, _ := base64.StdEncoding.DecodeString(cfg.Auths[server].Auth)
		if string(got) != want {
			t.Errorf("%s: auth %q, want %q", server, got, want)
		}
	}
}
