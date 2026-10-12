package cluster_test

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/backup"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

func TestDatabases(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.waitReady(2)
	ctx, c := h.ctx, h.client
	// With a destination, a new database is backed up from the start.
	h.backupsTo(filepath.Join(t.TempDir(), "bucket"))
	if _, _, err := h.st.CreateBackupKey(ctx, backup.NewKey().String()); err != nil {
		t.Fatal(err)
	}
	h.st.SetBackupKeySaved(ctx)
	h.st.CreateBackupDestination(ctx, types.BackupDestination{Name: "s3", Endpoint: "https://s3.example.test", Bucket: "b", Encrypt: true})

	var out bytes.Buffer
	logged := func(e types.Event) { out.WriteString(e.Message + "\n") }
	if err := c.CreateDatabase(ctx, "postgres", types.CreateDatabaseRequest{Name: "shopdb"}, logged); err != nil {
		t.Fatalf("create: %v\n%s", err, out.String())
	}
	for _, want := range []string{"Creating postgres database shopdb (postgres:17)", "Backing it up to s3", "running at postgres-shopdb.internal:5432"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("create said:\n%s\nwithout %q", out.String(), want)
		}
	}
	db, err := c.Database(ctx, "postgres", "shopdb")
	if err != nil {
		t.Fatal(err)
	}
	if db.App != "postgres-shopdb" || db.Status != "running" || db.Volume == nil || db.Backups == nil || db.Vars["POSTGRES_PASSWORD"] == "" ||
		!strings.HasPrefix(db.URL, "postgres://postgres:"+db.Vars["POSTGRES_PASSWORD"]+"@postgres-shopdb.internal:5432/shopdb") {
		t.Fatalf("database: %+v", db)
	}
	if apps, _ := c.Apps(ctx); len(apps) != 0 {
		t.Errorf("apps:list shows the database: %+v", apps)
	}
	if v := h.volume("postgres-shopdb", "data"); len(v.Mounts) != 1 || v.Mounts[0].Path != "/var/lib/postgresql/data" {
		t.Errorf("volume: %+v", v)
	}
	if err := c.CreateDatabase(ctx, "postgres", types.CreateDatabaseRequest{Name: "shopdb"}, logged); err == nil {
		t.Error("created shopdb twice")
	}
	// It runs from its official image: pushes don't deploy it.
	if err := c.Deploy(ctx, "postgres-shopdb", "git", "abc1234", strings.NewReader(""), func(types.Event) {}); err == nil ||
		!strings.Contains(err.Error(), "is a postgres database") {
		t.Errorf("push to a database: %v", err)
	}

	// Linking sets the URL on the app and restarts it.
	h.deploy("shop", 1)
	before := h.web1("shop").ID
	if err := c.LinkDatabase(ctx, "postgres", "shopdb", types.LinkDatabaseRequest{App: "shop"}, logged); err != nil {
		t.Fatal(err)
	}
	vars, _ := h.st.ConfigVars(ctx, "shop")
	if vars["DATABASE_URL"] != db.URL {
		t.Errorf("DATABASE_URL = %q, want %q", vars["DATABASE_URL"], db.URL)
	}
	eventually(t, 10*time.Second, "shop restarted", func() error {
		if in := h.web1("shop"); in == nil || in.ID == before || in.State != store.StateHealthy {
			return errFmt("not yet")
		}
		return nil
	})
	// Domains route HTTP, which a database doesn't speak.
	if _, err := c.PatchDomains(ctx, "postgres-shopdb", types.DomainsPatch{Add: []string{"db.example.test"}}); err == nil ||
		!strings.Contains(err.Error(), "domains route HTTP") {
		t.Errorf("a domain on a database: %v", err)
	}
	// One added before the API refused them gets no route, even with
	// proxy:enable: by the time shop's new domain is routed, the database's
	// was in the store too.
	h.st.UpdateDomains(ctx, "postgres-shopdb", types.DomainsPatch{Add: []string{"db.example.test"}})
	h.st.SetProperty(ctx, "postgres-shopdb", "proxy", "enabled", "true")
	if _, err := c.PatchDomains(ctx, "shop", types.DomainsPatch{Add: []string{"www.shop.example.test"}}); err != nil {
		t.Fatal(err)
	}
	px := h.nodes["control"].proxy
	eventually(t, 10*time.Second, "shop's new domain routed", func() error {
		if r := px.route("shop"); r == nil || !slices.Contains(r.Hosts, "www.shop.example.test") {
			return errFmt("not yet")
		}
		return nil
	})
	if r := px.route("postgres-shopdb"); r != nil {
		t.Errorf("the database got a route: %+v", r)
	}
	if _, err := c.PatchDomains(ctx, "postgres-shopdb", types.DomainsPatch{Remove: []string{"db.example.test"}}); err != nil {
		t.Errorf("removing a database's domain: %v", err)
	}
	if err := c.CreateDatabase(ctx, "redis", types.CreateDatabaseRequest{Name: "cache"}, logged); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateDatabase(ctx, "mysql", types.CreateDatabaseRequest{Name: "orders", SizeMB: 2048}, logged); err != nil {
		t.Fatal(err)
	}
	if v := h.volume("mysql-orders", "data"); v.SizeMB != 2048 {
		t.Errorf("mysql's volume is %d MB, want 2048", v.SizeMB)
	}
	// A second DATABASE_URL needs another name.
	if err := c.LinkDatabase(ctx, "mysql", "orders", types.LinkDatabaseRequest{App: "shop", NoRestart: true}, logged); err == nil ||
		!strings.Contains(err.Error(), "already gets DATABASE_URL from postgres-shopdb") {
		t.Errorf("a second DATABASE_URL: %v", err)
	}
	if err := c.LinkDatabase(ctx, "mysql", "orders", types.LinkDatabaseRequest{App: "shop", Alias: "orders", NoRestart: true}, logged); err != nil {
		t.Fatal(err)
	}
	if err := c.LinkDatabase(ctx, "redis", "cache", types.LinkDatabaseRequest{App: "shop", NoRestart: true}, logged); err != nil {
		t.Fatal(err)
	}
	vars, _ = h.st.ConfigVars(ctx, "shop")
	if !strings.HasPrefix(vars["ORDERS_URL"], "mysql://mysql:") || !strings.HasPrefix(vars["REDIS_URL"], "redis://:") {
		t.Errorf("shop's vars: %v", vars)
	}
	all, _ := c.Databases(ctx, "")
	if len(all) != 3 {
		t.Errorf("db:list: %+v", all)
	}
	// jokku top shows which app each database belongs to.
	st, err := c.ClusterStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	links := map[string][]types.DatabaseLink{}
	for _, a := range st.Apps {
		links[a.Name] = a.Links
	}
	if l := links["mysql-orders"]; len(l) != 1 || l[0] != (types.DatabaseLink{App: "shop", Var: "ORDERS_URL"}) {
		t.Errorf("mysql-orders's links in the status: %+v", l)
	}
	if l := links["shop"]; len(l) != 0 {
		t.Errorf("an app has links of its own: %+v", l)
	}

	// Linked databases aren't destroyed.
	if err := c.DestroyDatabase(ctx, "postgres", "shopdb"); err == nil || !strings.Contains(err.Error(), "linked to shop") {
		t.Errorf("destroying a linked database: %v", err)
	}
	if err := c.UnlinkDatabase(ctx, "postgres", "shopdb", "shop", true, logged); err != nil {
		t.Fatal(err)
	}
	if vars, _ := h.st.ConfigVars(ctx, "shop"); vars["DATABASE_URL"] != "" {
		t.Error("unlinking left DATABASE_URL")
	}
	if err := c.DestroyDatabase(ctx, "postgres", "shopdb"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Database(ctx, "postgres", "shopdb"); err == nil {
		t.Error("the database is still there")
	}
	// Destroying an app forgets its links.
	if err := c.DestroyApp(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	if db, _ := c.Database(ctx, "redis", "cache"); len(db.Links) != 0 {
		t.Errorf("links to a destroyed app: %+v", db.Links)
	}
}
