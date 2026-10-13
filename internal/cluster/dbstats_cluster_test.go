package cluster_test

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/types"
)

// TestDatabaseStatsAndUsers: the control node looks inside a running
// database by running its client in the VM, turns on what it needs once,
// and manages its users, against a fake psql that answers the way the
// real one did (internal/database/testdata).
func TestDatabaseStatsAndUsers(t *testing.T) {
	cluster.DBPollEvery = 200 * time.Millisecond
	defer func() { cluster.DBPollEvery = 10 * time.Second }()
	fresh, err := os.ReadFile("../database/testdata/postgres-no-statements.out")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var ran []string
	users := map[string]string{} // the logins added, and their access
	role := regexp.MustCompile(`ROLE "([a-z_0-9]+)"`)
	psql := func(argv []string, stdin string) (string, string, int) {
		mu.Lock()
		defer mu.Unlock()
		if argv[0] != "psql" {
			return "", "not a database this test fakes", 1
		}
		ran = append(ran, stdin)
		name := ""
		if m := role.FindStringSubmatch(stdin); m != nil {
			name = m[1]
		}
		switch {
		case strings.HasPrefix(stdin, "SET pg_stat_statements.track"):
			return string(fresh), "", 0
		case strings.HasPrefix(stdin, "ALTER SYSTEM"):
			return "", "", 0
		case strings.HasPrefix(stdin, "SELECT json_build_object('users'"):
			list := []map[string]any{{"name": "postgres", "super": true, "can_read": true, "can_write": true}}
			for u, access := range users {
				list = append(list, map[string]any{"name": u, "super": false, "can_read": true, "can_write": access == "read-write"})
			}
			b, _ := json.Marshal(map[string]any{"users": list})
			return string(b), "", 0
		case strings.HasPrefix(stdin, "BEGIN;\nCREATE ROLE"):
			if users[name] != "" {
				return "", fmt.Sprintf("psql:<stdin>:2: ERROR:  role %q already exists\n", name), 3
			}
			users[name] = "read-only"
			if strings.Contains(stdin, "pg_write_all_data") {
				users[name] = "read-write"
			}
			return "", "", 0
		case strings.HasPrefix(stdin, "ALTER ROLE"):
			return "", "", 0
		case strings.Contains(stdin, "DROP ROLE"):
			delete(users, name)
			return "0\n", "", 0
		}
		return "", "unexpected: " + stdin, 1
	}
	fakeDatabase.Store(&psql)
	defer fakeDatabase.Store(nil)
	count := func(prefix string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, s := range ran {
			if strings.HasPrefix(s, prefix) {
				n++
			}
		}
		return n
	}

	h := newHarness(t)
	h.join("w1")
	h.waitReady(2)
	ctx, c := h.ctx, h.client
	logged := func(types.Event) {}
	for _, db := range []string{"postgres:shopdb", "redis:cache"} {
		engine, name, _ := strings.Cut(db, ":")
		if err := c.CreateDatabase(ctx, engine, types.CreateDatabaseRequest{Name: name}, logged); err != nil {
			t.Fatal(err)
		}
	}

	// pg_stat_statements isn't loaded: Jokku turns it on once, and until a
	// restart counts transactions.
	var shop types.DatabaseStats
	eventually(t, 10*time.Second, "a look inside shopdb", func() error {
		all, err := c.DatabaseStats(ctx)
		if err != nil {
			return err
		}
		for _, s := range all {
			if s.Name == "shopdb" {
				shop = s
			}
		}
		if shop.At.IsZero() || count("ALTER SYSTEM") == 0 {
			return fmt.Errorf("shopdb: %+v", shop)
		}
		return nil
	})
	if shop.Engine != "postgres" || shop.MaxConnections != 97 || shop.Per != "transaction" || shop.Error != "" ||
		!strings.Contains(shop.Note, "next restart: jokku db:postgres:restart shopdb") ||
		!slices.Contains(shop.Users, types.DatabaseUser{Name: "postgres", Access: "all"}) {
		t.Errorf("shopdb: %+v", shop)
	}
	polls := count("SET pg_stat_statements.track")
	eventually(t, 5*time.Second, "more looks", func() error {
		if count("SET pg_stat_statements.track") < polls+3 {
			return fmt.Errorf("%d looks", count("SET pg_stat_statements.track"))
		}
		return nil
	})
	if n := count("ALTER SYSTEM"); n != 1 {
		t.Errorf("ALTER SYSTEM ran %d times", n)
	}
	events, _ := c.Events(ctx, "postgres-shopdb", 20)
	if !slices.ContainsFunc(events, func(e types.ClusterEvent) bool { return strings.Contains(e.Message, "turned on query statistics") }) {
		t.Errorf("no event for turning on pg_stat_statements: %+v", events)
	}
	if db, err := c.Database(ctx, "postgres", "shopdb"); err != nil || db.Stats == nil || db.Stats.MaxConnections != 97 {
		t.Errorf("db:postgres:info has no stats: %+v %v", db, err)
	}
	// Its redis can't be looked inside here, and says so.
	all, _ := c.DatabaseStats(ctx)
	if i := slices.IndexFunc(all, func(s types.DatabaseStats) bool { return s.Name == "cache" }); i < 0 || all[i].Error == "" {
		t.Errorf("cache: %+v", all)
	}

	// Users: added with a password Jokku makes and keeps for their URLs.
	u, err := c.AddDatabaseUser(ctx, "postgres", "shopdb", types.AddDatabaseUserRequest{Name: "reporting", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if u.Access != "read-only" || len(u.Password) < 32 || u.URL != "postgres://reporting:"+u.Password+"@postgres-shopdb.internal:5432/shopdb" || u.PublicURL != "" {
		t.Errorf("added: %+v", u)
	}
	list, err := c.DatabaseUsers(ctx, "postgres", "shopdb")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(list, []types.DatabaseUser{{Name: "postgres", Access: "all"}, *u}) {
		t.Errorf("users: %+v", list)
	}
	if _, err := c.AddDatabaseUser(ctx, "postgres", "shopdb", types.AddDatabaseUserRequest{Name: "reporting"}); err == nil ||
		!strings.Contains(err.Error(), `role "reporting" already exists`) || strings.Contains(err.Error(), "psql:") {
		t.Errorf("adding twice: %v", err)
	}
	again, err := c.DatabaseUserPassword(ctx, "postgres", "shopdb", "reporting")
	if err != nil || again.Password == u.Password || !strings.Contains(again.URL, again.Password) {
		t.Errorf("new password: %+v %v", again, err)
	}
	if err := c.RemoveDatabaseUser(ctx, "postgres", "shopdb", "reporting"); err != nil {
		t.Fatal(err)
	}
	if list, _ := c.DatabaseUsers(ctx, "postgres", "shopdb"); len(list) != 1 {
		t.Errorf("users after removing: %+v", list)
	}
	for _, bad := range []struct{ engine, db, user, want string }{
		{"postgres", "shopdb", "postgres", "Postgres's own user"},
		{"postgres", "shopdb", "Bad-Name", "not a user name"},
		{"redis", "cache", "reporting", "Jokku doesn't manage users"},
		{"postgres", "nope", "reporting", "no postgres database nope"},
	} {
		if err := c.RemoveDatabaseUser(ctx, bad.engine, bad.db, bad.user); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("removing %s from %s %s: %v, want %q", bad.user, bad.engine, bad.db, err, bad.want)
		}
	}
	if n := count("BEGIN;\nCREATE ROLE"); n != 2 {
		t.Errorf("CREATE ROLE ran %d times", n)
	}
}
