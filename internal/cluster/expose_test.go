package cluster_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// stream is n's proxy's stream for app, or nil.
func (p *fakeProxy) stream(app string) *types.ProxyStream {
	ps := p.get()
	if ps == nil {
		return nil
	}
	for _, s := range ps.Streams {
		if s.App == app {
			return &s
		}
	}
	return nil
}

func TestExposedDatabases(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	edge, _ := h.addEdge("edge1")
	h.waitReady(3)
	ctx, c := h.ctx, h.client
	logged := func(types.Event) {}
	for _, db := range []string{"postgres:shopdb", "mysql:orders", "redis:cache"} {
		engine, name, _ := strings.Cut(db, ":")
		if err := c.CreateDatabase(ctx, engine, types.CreateDatabaseRequest{Name: name}, logged); err != nil {
			t.Fatal(err)
		}
	}
	db, err := c.ExposeDatabase(ctx, "postgres", "shopdb", types.ExposeDatabaseRequest{Domain: "ShopDB.example.test.",
		Allow: []string{"203.0.113.7", "198.51.100.9/24"}})
	if err != nil {
		t.Fatal(err)
	}
	pw := db.Vars["POSTGRES_PASSWORD"]
	if full, _ := c.Database(ctx, "postgres", "shopdb"); full != nil {
		pw = full.Vars["POSTGRES_PASSWORD"]
	}
	if x := db.Exposed; x == nil || x.Host != "shopdb.example.test" || x.Port != 5432 ||
		!slices.Equal(x.Allow, []string{"198.51.100.0/24", "203.0.113.7/32"}) ||
		x.URL != "postgres://postgres:"+pw+"@shopdb.example.test:5432/shopdb?sslmode=require" {
		t.Errorf("exposed: %+v", db.Exposed)
	}
	var in store.Instance
	insts, _ := h.st.Instances(ctx, "postgres-shopdb")
	for _, i := range insts {
		if i.ProcessType == "postgres" {
			in = i
		}
	}
	// Every ingress node's proxy, and the edge's, carries it straight to
	// its instance; the node it runs on lets the edge reach it.
	want := types.ProxyStream{App: "postgres-shopdb", Protocol: types.StreamPostgres, Host: "shopdb.example.test", Port: 5432,
		Upstreams: []string{in.IP + ":5432"}, ACME: true, Allow: []string{"198.51.100.0/24", "203.0.113.7/32"}}
	eventually(t, 5*time.Second, "the proxies carry shopdb", func() error {
		for _, name := range []string{"control", "w1", "edge1"} {
			if s := h.nodes[name].proxy.stream("postgres-shopdb"); s == nil || !streamEqual(*s, want) {
				return fmt.Errorf("%s's stream: %+v", name, s)
			}
		}
		_, access := h.nodes[in.Node].mesh.get()
		if access == nil || !slices.Contains(access.Allow, in.IP+":5432") {
			return fmt.Errorf("%s doesn't let the edge reach shopdb: %+v", in.Node, access)
		}
		return nil
	})

	// MySQL can't be told apart by name: its port is its own.
	db, err = c.ExposeDatabase(ctx, "mysql", "orders", types.ExposeDatabaseRequest{Domain: "203.0.113.50"})
	if err != nil {
		t.Fatal(err)
	}
	if x := db.Exposed; x == nil || x.Port != 3306 || len(x.Allow) != 0 || !strings.HasPrefix(x.URL, "mysql://mysql:") {
		t.Errorf("mysql exposed: %+v", x)
	}
	for _, bad := range []struct {
		engine, name string
		req          types.ExposeDatabaseRequest
		want         string
	}{
		{"redis", "cache", types.ExposeDatabaseRequest{Domain: "cache.example.test", Port: 3306}, "needs a port to itself"},
		{"redis", "cache", types.ExposeDatabaseRequest{Domain: "cache.example.test", Port: 5432}, "which are postgres, not redis"},
		{"redis", "cache", types.ExposeDatabaseRequest{Domain: "cache.example.test", Port: 443}, "Jokku's nodes use themselves"},
		{"redis", "cache", types.ExposeDatabaseRequest{Domain: "203.0.113.60"}, "is an address"},
		{"redis", "cache", types.ExposeDatabaseRequest{Domain: "*.example.test"}, "wildcard"},
		{"redis", "cache", types.ExposeDatabaseRequest{Domain: "cache.example.test", Allow: []string{"office"}}, "not an address"},
		{"postgres", "nope", types.ExposeDatabaseRequest{Domain: "nope.example.test"}, "no postgres database nope"},
	} {
		if _, err := c.ExposeDatabase(ctx, bad.engine, bad.name, bad.req); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("exposing %s %s %+v: %v, want %q", bad.engine, bad.name, bad.req, err, bad.want)
		}
	}
	// Redis shares postgres's name on its own port.
	if _, err := c.ExposeDatabase(ctx, "redis", "cache", types.ExposeDatabaseRequest{Domain: "shopdb.example.test"}); err != nil {
		t.Fatal(err)
	}

	// Stopped, it is still routed (to nothing): clients are turned away
	// rather than sent to the wrong place.
	if err := c.PS(ctx, "postgres-shopdb", "stop", logged); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "shopdb's stream empties", func() error {
		if s := edge.proxy.stream("postgres-shopdb"); s == nil || len(s.Upstreams) != 0 {
			return fmt.Errorf("edge's stream: %+v", s)
		}
		return nil
	})
	if err := c.UnexposeDatabase(ctx, "postgres", "shopdb"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "shopdb's stream goes", func() error {
		if s := edge.proxy.stream("postgres-shopdb"); s != nil {
			return fmt.Errorf("edge's stream: %+v", s)
		}
		if s := edge.proxy.stream("redis-cache"); s == nil || s.Host != "shopdb.example.test" || s.Port != 6379 {
			return fmt.Errorf("edge's redis stream: %+v", s)
		}
		return nil
	})
	if db, _ := c.Database(ctx, "postgres", "shopdb"); db.Exposed != nil {
		t.Errorf("still exposed: %+v", db.Exposed)
	}
	if err := c.UnexposeDatabase(ctx, "postgres", "shopdb"); err == nil || !strings.Contains(err.Error(), "isn't exposed") {
		t.Errorf("unexposing twice: %v", err)
	}
}

func streamEqual(a, b types.ProxyStream) bool {
	return a.App == b.App && a.Protocol == b.Protocol && a.Host == b.Host && a.Port == b.Port && a.ACME == b.ACME &&
		slices.Equal(a.Upstreams, b.Upstreams) && slices.Equal(a.Allow, b.Allow)
}
