package database

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wes/jokku/internal/compose"
)

// Each engine's compose file is one that Jokku's compose loader takes as
// it is: one service, its image, a data volume, no HTTP.
func TestComposeFiles(t *testing.T) {
	for _, name := range Names() {
		e, _ := Lookup(name)
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(e.Compose(0)), 0o644)
			image := e.ImageRef("", "")
			vars := e.Vars("shopdb", image)
			plan, err := compose.Load(context.Background(), dir, dir, "", e.AppName("shopdb"), vars)
			if err != nil {
				t.Fatal(err)
			}
			svc := plan.Services[e.Process()]
			if len(plan.Services) != 1 || svc == nil {
				t.Fatalf("services: %v", plan.Services)
			}
			if svc.Image != image || plan.Web != "" || svc.MemoryMB != e.MemoryMB || len(svc.Mounts) != 1 || svc.Mounts[0].Path != e.Mount || svc.Mounts[0].Volume != "data" {
				t.Errorf("service: %+v, web %q", svc, plan.Web)
			}
			// Passwords fill in the file and go nowhere else.
			env := strings.Join(svc.Env, " ") + strings.Join(svc.Command, " ")
			for _, s := range e.secrets {
				if !strings.Contains(env, vars[s]) {
					t.Errorf("%s isn't passed on", s)
				}
			}
		})
	}
}

func TestURLsAndCommands(t *testing.T) {
	pg, _ := Lookup("postgres")
	vars := map[string]string{"POSTGRES_PASSWORD": "p@ss"}
	if got := pg.URL("shopdb", vars); got != "postgres://postgres:p%40ss@postgres-shopdb.internal:5432/shopdb" {
		t.Errorf("postgres URL = %s", got)
	}
	redis, _ := Lookup("redis")
	if got := redis.URL("cache", map[string]string{"REDIS_PASSWORD": "pw"}); got != "redis://:pw@redis-cache.internal:6379" {
		t.Errorf("redis URL = %s", got)
	}
	// Exposed, over TLS where the proxies insist on it.
	if got := pg.PublicURL("shopdb", "db.example.com", 5432, vars); got != "postgres://postgres:p%40ss@db.example.com:5432/shopdb?sslmode=require" {
		t.Errorf("postgres public URL = %s", got)
	}
	if got := redis.PublicURL("cache", "cache.example.com", 6380, map[string]string{"REDIS_PASSWORD": "pw"}); got != "rediss://:pw@cache.example.com:6380" {
		t.Errorf("redis public URL = %s", got)
	}
	mysql, _ := Lookup("mysql")
	if got := mysql.PublicURL("orders", "db.example.com", 13306, map[string]string{"MYSQL_PASSWORD": "pw"}); got != "mysql://mysql:pw@db.example.com:13306/orders" {
		t.Errorf("mysql public URL = %s", got)
	}
	if _, _, ok := redis.ImportCommand("cache", nil); ok {
		t.Error("redis imports a dump")
	}
	argv, env := mysql.ExportCommand("shopdb", map[string]string{"MYSQL_ROOT_PASSWORD": "secret"})
	if strings.Contains(strings.Join(argv, " "), "secret") || !slices.Contains(env, "MYSQL_PWD=secret") {
		t.Errorf("the password goes in the arguments: %v %v", argv, env)
	}
	if v := pg.Vars("a", "x")["POSTGRES_PASSWORD"]; len(v) < 32 || v == pg.Vars("a", "x")["POSTGRES_PASSWORD"] {
		t.Error("passwords are short or the same each time")
	}
	for _, bad := range []string{"", "Shop", "-shop", "shop_db", strings.Repeat("a", 43)} {
		if CheckName(bad) == nil {
			t.Errorf("CheckName(%q) passed", bad)
		}
	}
}
