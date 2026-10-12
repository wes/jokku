package database

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// The testdata is the stats commands' real output, from postgres:17 (after
// pgbench, and freshly started without pg_stat_statements), mysql:8.4 and
// redis:7 with some queries run.
func sample(t *testing.T, engine, file string) *Sample {
	t.Helper()
	b, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := Lookup(engine)
	s, err := e.ParseStats(b)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return s
}

func histTotal(s *Sample) uint64 {
	var n uint64
	for _, c := range s.Hist {
		n += c
	}
	return n
}

func TestParseStats(t *testing.T) {
	pg := sample(t, "postgres", "postgres.out")
	if pg.MaxConns != 97 || pg.Per != "query" || pg.Queries < 40000 || pg.QueryMS <= 0 || pg.Own != 0 ||
		pg.Hits == 0 || pg.HitsAre != "cache" || !pg.ActiveKnown || pg.Setup != nil || pg.Note != "" {
		t.Errorf("postgres: %+v", pg)
	}
	if len(pg.Slowest) != 5 || !strings.Contains(pg.Slowest[0].Text, "pgbench") || pg.Slowest[0].TotalMS < pg.Slowest[1].TotalMS {
		t.Errorf("postgres's slowest: %+v", pg.Slowest)
	}
	for _, q := range pg.Slowest {
		if strings.Contains(q.Text, "pg_stat_statements.track") {
			t.Errorf("the stats' own statement is among the slowest: %q", q.Text)
		}
	}
	if !slices.Equal(pg.Users, []User{{"postgres", AccessAll}, {"writer", AccessReadWrite}}) {
		t.Errorf("postgres's users: %+v", pg.Users)
	}

	// Without pg_stat_statements, a transaction is the unit, the stats'
	// own count, and turning it on waits for a restart.
	fresh := sample(t, "postgres", "postgres-no-statements.out")
	if fresh.Per != "transaction" || fresh.Queries == 0 || fresh.Own != postgresOwn || fresh.Slowest != nil ||
		fresh.Note != "restart" || fresh.Setup == nil ||
		fresh.Setup.Stdin != "ALTER SYSTEM SET shared_preload_libraries = 'pg_stat_statements';\n" {
		t.Errorf("postgres without statements: %+v %+v", fresh, fresh.Setup)
	}

	my := sample(t, "mysql", "mysql.out")
	if my.MaxConns != 151 || my.Per != "query" || my.Own != mysqlOwn || my.Queries < 400 || my.Queries != histTotal(my) ||
		my.MaxUsed < 1 || my.Hits == 0 || my.Setup != nil {
		t.Errorf("mysql: %+v", my)
	}
	for b := range my.Hist {
		if b.Lo >= b.Hi || b.Hi > 60e6 {
			t.Errorf("mysql bucket %+v isn't in microseconds", b)
		}
	}
	if !slices.Contains(my.Users, User{"mysql", AccessAll}) || !slices.Contains(my.Users, User{"writer", AccessReadWrite}) ||
		!slices.Contains(my.Users, User{"root@localhost", AccessAll}) {
		t.Errorf("mysql's users: %+v", my.Users)
	}

	// Redis: 2,000 INCRs and 501 GETs, besides a few others; the stats'
	// own commands left out.
	rd := sample(t, "redis", "redis.out")
	if rd.MaxConns != 10000 || rd.Per != "command" || rd.HitsAre != "keys" || rd.ActiveKnown || rd.MemoryUsed == 0 ||
		rd.Queries < 2501 || rd.Queries > 2600 || rd.Queries != histTotal(rd) || rd.Misses == 0 {
		t.Errorf("redis: %+v", rd)
	}
	for b := range rd.Hist {
		if b.Hi > 1 && b.Lo != b.Hi/2 {
			t.Errorf("redis bucket %+v doesn't start at half its bound", b)
		}
	}
}

func TestPostgresSetup(t *testing.T) {
	out := func(preload, pending string, available bool) []byte {
		p := "null"
		if pending != "" {
			p = `"` + pending + `"`
		}
		return []byte(fmt.Sprintf(`{"started": 1, "max": 100, "reserved": 3, "preload": %q, "pending": %s, "extension": false, "available": %v, "clients": [], "xacts": 10, "hits": 1, "reads": 1}`,
			preload, p, available))
	}
	e, _ := Lookup("postgres")
	for _, c := range []struct {
		preload, pending string
		available        bool
		setup, note      string
	}{
		// Libraries already loaded stay.
		{"auto_explain", "", true, "ALTER SYSTEM SET shared_preload_libraries = 'auto_explain', 'pg_stat_statements';\n", "restart"},
		{"", "pg_stat_statements", true, "", "restart"}, // waiting for the restart
		{"pg_stat_statements", "", true, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;\n", ""},
		// An image without it would no longer start with it.
		{"", "", false, "", "unavailable"},
	} {
		s, err := e.ParseStats(out(c.preload, c.pending, c.available))
		if err != nil {
			t.Fatal(err)
		}
		setup := ""
		if s.Setup != nil {
			setup = s.Setup.Stdin
		}
		if setup != c.setup || s.Note != c.note {
			t.Errorf("preload %q, pending %q: setup %q, note %q", c.preload, c.pending, setup, s.Note)
		}
	}
}

func TestParseStatsRefusesNonsense(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql", "redis"} {
		e, _ := Lookup(engine)
		for _, out := range []string{"", "ERROR: no", "{}"} {
			if _, err := e.ParseStats([]byte(out)); err == nil {
				t.Errorf("%s parsed %q", engine, out)
			}
		}
	}
}

func TestUsers(t *testing.T) {
	pg, _ := Lookup("postgres")
	my, _ := Lookup("mysql")
	for _, c := range []struct {
		e    Engine
		user string
		ok   bool
	}{
		{pg, "reporting", true}, {pg, "bi_tool2", true}, {my, "reporting", true},
		{pg, "postgres", false}, {pg, "pg_monitor", false}, {my, "root", false}, {my, "mysql", false},
		{pg, "Reporting", false}, {pg, "2x", false}, {pg, "a-b", false}, {pg, `x"; DROP`, false},
		{pg, strings.Repeat("a", 33), false},
	} {
		if err := c.e.CheckUser(c.user); (err == nil) != c.ok {
			t.Errorf("%s user %q: %v", c.e.Name, c.user, err)
		}
	}
	// Passwords go in on stdin, never in a command's arguments.
	vars := map[string]string{"MYSQL_ROOT_PASSWORD": "rootpw"}
	for _, e := range []Engine{pg, my} {
		c := e.AddUserCommand("shop", vars, "reporting", "s3cret", true)
		if strings.Contains(strings.Join(c.Argv, " "), "s3cret") || !strings.Contains(c.Stdin, "'s3cret'") {
			t.Errorf("%s: %+v", e.Name, c)
		}
		if strings.Contains(c.Stdin, "INSERT") || strings.Contains(c.Stdin, "pg_write_all_data") {
			t.Errorf("%s: a read-only user may write: %s", e.Name, c.Stdin)
		}
		c = e.AddUserCommand("shop", vars, "app2", "s3cret", false)
		if !strings.Contains(c.Stdin, "INSERT") && !strings.Contains(c.Stdin, "pg_write_all_data") {
			t.Errorf("%s: a read-write user can't write: %s", e.Name, c.Stdin)
		}
	}
	internal, public := pg.UserURLs("shop", "reporting", "pw", "db.example.com", 5432)
	if internal != "postgres://reporting:pw@postgres-shop.internal:5432/shop" ||
		public != "postgres://reporting:pw@db.example.com:5432/shop?sslmode=require" {
		t.Errorf("urls: %s %s", internal, public)
	}
	if _, public := my.UserURLs("shop", "reporting", "pw", "", 0); public != "" {
		t.Errorf("an unexposed database has a public URL: %s", public)
	}
	rd, _ := Lookup("redis")
	if rd.HasUsers() || !pg.HasUsers() || !my.HasUsers() {
		t.Error("HasUsers")
	}
}
