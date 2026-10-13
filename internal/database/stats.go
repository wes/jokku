package database

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Sample is one look inside a running database: its connections now, and
// counters that only grow while it runs, which two samples turn into rates
// and latency ("jokku top"). The control node takes one every few seconds
// by running StatsCommand in the database's VM.
type Sample struct {
	// Started is when the server started (Unix seconds): its counters
	// start over with it.
	Started  float64
	MaxConns int
	// MaxUsed is the most connections at once since it started; 0 when the
	// engine doesn't keep it.
	MaxUsed int
	Clients []Client
	// ActiveKnown says whether Client.Active means anything: Redis runs
	// one command at a time, so its clients are never seen busy.
	ActiveKnown bool

	// Queries counts the queries run so far, leaving out the stats
	// command's own; Per says what one is ("query" or, for Postgres
	// without pg_stat_statements, "transaction"). An engine that keeps how
	// long they took as a histogram fills Hist, one that keeps only the
	// total fills QueryMS.
	Queries uint64
	Per     string
	QueryMS float64
	Hist    map[Bucket]uint64
	// Own is how many of Queries the stats command itself ran, when the
	// engine can't leave them out (Postgres's transactions): the collector
	// subtracts it from each interval.
	Own uint64

	// Hits and Misses are reads served from memory and from disk (Redis:
	// keys found and not found); HitsAre says which.
	Hits, Misses uint64
	HitsAre      string

	MemoryUsed, MemoryMax uint64 // Redis, in bytes; MemoryMax 0 when unlimited

	Slowest []Query // Postgres with pg_stat_statements: by total time
	Users   []User

	// Setup is what to run once to turn on what the stats need, and Note
	// what is still missing, in a sentence.
	Setup *Command
	Note  string
}

// Client is a database's connections from one address as one user.
type Client struct {
	Addr   string // "" for a local socket
	User   string
	Count  int
	Active int // running a query now
}

// Bucket is a histogram bucket: queries that took from Lo up to Hi
// microseconds.
type Bucket struct{ Lo, Hi float64 }

// Mid is a duration that stands for b.
func (b Bucket) Mid() float64 {
	if b.Lo <= 0 {
		return b.Hi / 2
	}
	return math.Sqrt(b.Lo * b.Hi)
}

// Query is a statement and how long it has taken, since the server's
// statistics last started.
type Query struct {
	Text    string
	Calls   uint64
	TotalMS float64
	MeanMS  float64
}

// User is a login of a database's, and what it may do.
type User struct {
	Name   string
	Access string // AccessAll, AccessReadWrite, AccessReadOnly or AccessOther
}

const (
	AccessAll       = "all"        // everything, schema changes included
	AccessReadWrite = "read-write" // read and change data
	AccessReadOnly  = "read-only"
	AccessOther     = "other" // privileges Jokku didn't set
)

// Command is something to run in a database's VM: Argv with Env, fed
// Stdin.
type Command struct {
	Argv  []string
	Env   []string
	Stdin string
}

// StatsCommand looks inside the database, for ParseStats.
func (e Engine) StatsCommand(name string, vars map[string]string) Command {
	switch e.Name {
	case "postgres":
		return Command{Argv: psql(name), Stdin: postgresStats}
	case "mysql":
		return Command{Argv: []string{"mysql", "-uroot", "-N", "-B", "--raw"}, Env: mysqlEnv(vars),
			Stdin: fmt.Sprintf(mysqlStats, sqlString(name))}
	default:
		return Command{Argv: []string{"sh", "-c", redisStats}, Env: []string{"REDISCLI_AUTH=" + vars["REDIS_PASSWORD"]}}
	}
}

// ParseStats reads StatsCommand's output.
func (e Engine) ParseStats(out []byte) (*Sample, error) {
	switch e.Name {
	case "postgres":
		return parsePostgres(out)
	case "mysql":
		return parseMySQL(out)
	default:
		return parseRedis(out)
	}
}

func psql(name string) []string {
	return []string{"psql", "-X", "-q", "-A", "-t", "-U", "postgres", "-d", name, "-v", "ON_ERROR_STOP=1", "-f", "-"}
}

func mysqlEnv(vars map[string]string) []string {
	return []string{"MYSQL_PWD=" + vars["MYSQL_ROOT_PASSWORD"]}
}

// postgresStats runs with pg_stat_statements off for its own session, so
// the statements it counts are the apps'. Its transactions do count in
// pg_stat_database: postgresOwn of them, or one more with
// pg_stat_statements.
const postgresStats = `SET pg_stat_statements.track = none;
SELECT current_setting('server_version_num')::int >= 140000 AS v14,
  current_setting('shared_preload_libraries') ~ 'pg_stat_statements'
    AND EXISTS (SELECT FROM pg_extension WHERE extname = 'pg_stat_statements') AS stmts \gset
SELECT json_build_object(
  'started', extract(epoch FROM pg_postmaster_start_time()),
  'max', current_setting('max_connections')::int,
  'reserved', current_setting('superuser_reserved_connections')::int,
  'preload', current_setting('shared_preload_libraries'),
  'pending', (SELECT setting FROM pg_file_settings WHERE name = 'shared_preload_libraries' ORDER BY seqno DESC LIMIT 1),
  'extension', EXISTS (SELECT FROM pg_extension WHERE extname = 'pg_stat_statements'),
  'available', EXISTS (SELECT FROM pg_available_extensions WHERE name = 'pg_stat_statements'),
  'clients', (SELECT coalesce(json_agg(c), '[]') FROM (
      SELECT coalesce(host(client_addr), '') AS addr, usename AS user, count(*) AS n,
        count(*) FILTER (WHERE state = 'active') AS active
      FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()
      GROUP BY 1, 2) c),
  'xacts', (SELECT sum(xact_commit + xact_rollback) FROM pg_stat_database),
  'hits', (SELECT sum(blks_hit) FROM pg_stat_database),
  'reads', (SELECT sum(blks_read) FROM pg_stat_database));
\if :v14
SELECT json_build_object(
  'active_ms', (SELECT sum(active_time) FROM pg_stat_database),
  'users', (SELECT coalesce(json_agg(u), '[]') FROM (
      SELECT rolname AS name, rolsuper AS super,
        pg_has_role(oid, 'pg_read_all_data', 'MEMBER') AS can_read,
        pg_has_role(oid, 'pg_write_all_data', 'MEMBER') AS can_write
      FROM pg_roles WHERE rolcanlogin) u));
\else
SELECT json_build_object('users', (SELECT coalesce(json_agg(u), '[]') FROM (
  SELECT rolname AS name, rolsuper AS super FROM pg_roles WHERE rolcanlogin) u));
\endif
\if :stmts
SELECT json_build_object(
  'calls', coalesce(sum(calls), 0),
  'exec_ms', coalesce(sum(total_exec_time), 0),
  'top', (SELECT coalesce(json_agg(t), '[]') FROM (
      SELECT left(regexp_replace(query, '\s+', ' ', 'g'), 500) AS query, calls,
        total_exec_time AS total_ms, mean_exec_time AS mean_ms
      FROM pg_stat_statements WHERE query <> 'SET pg_stat_statements.track = none'
      ORDER BY total_exec_time DESC LIMIT 5) t))
FROM pg_stat_statements WHERE query <> 'SET pg_stat_statements.track = none';
\endif
`

// postgresOwn is how many transactions postgresStats runs, without the
// pg_stat_statements query.
const postgresOwn = 5

type pgStats struct {
	Started   float64 `json:"started"`
	Max       int     `json:"max"`
	Reserved  int     `json:"reserved"`
	Preload   string  `json:"preload"`
	Pending   *string `json:"pending"`
	Extension bool    `json:"extension"`
	Available bool    `json:"available"`
	Clients   []struct {
		Addr   string `json:"addr"`
		User   string `json:"user"`
		N      int    `json:"n"`
		Active int    `json:"active"`
	} `json:"clients"`
	Xacts    *float64 `json:"xacts"`
	Hits     *float64 `json:"hits"`
	Reads    *float64 `json:"reads"`
	ActiveMS *float64 `json:"active_ms"`
	Users    []struct {
		Name     string `json:"name"`
		Super    bool   `json:"super"`
		CanRead  bool   `json:"can_read"`
		CanWrite bool   `json:"can_write"`
	} `json:"users"`
	Calls  *float64 `json:"calls"`
	ExecMS *float64 `json:"exec_ms"`
	Top    []struct {
		Query   string  `json:"query"`
		Calls   float64 `json:"calls"`
		TotalMS float64 `json:"total_ms"`
		MeanMS  float64 `json:"mean_ms"`
	} `json:"top"`
}

func parsePostgres(out []byte) (*Sample, error) {
	var st pgStats
	// One JSON object per query, each adding its fields.
	d := json.NewDecoder(bytes.NewReader(out))
	n := 0
	for {
		if err := d.Decode(&st); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("reading Postgres's statistics: %w", err)
		}
		n++
	}
	if n == 0 || st.Max == 0 {
		return nil, errors.New("Postgres's statistics are empty")
	}
	s := &Sample{Started: st.Started, MaxConns: st.Max - st.Reserved, ActiveKnown: true,
		Hits: u64(st.Hits), Misses: u64(st.Reads), HitsAre: "cache"}
	for _, c := range st.Clients {
		s.Clients = append(s.Clients, Client{Addr: c.Addr, User: c.User, Count: c.N, Active: c.Active})
	}
	for _, u := range st.Users {
		s.Users = append(s.Users, User{Name: u.Name, Access: accessOf(u.Super, u.CanRead, u.CanWrite)})
	}
	sort.Slice(s.Users, func(i, j int) bool { return s.Users[i].Name < s.Users[j].Name })
	if st.Calls != nil {
		// pg_stat_statements: its queries, the stats command's left out.
		s.Queries, s.QueryMS, s.Per = u64(st.Calls), f64(st.ExecMS), "query"
		for _, t := range st.Top {
			s.Slowest = append(s.Slowest, Query{Text: t.Query, Calls: uint64(t.Calls), TotalMS: t.TotalMS, MeanMS: t.MeanMS})
		}
	} else {
		s.Queries, s.QueryMS, s.Per, s.Own = u64(st.Xacts), f64(st.ActiveMS), "transaction", postgresOwn
	}
	loaded := hasLibrary(st.Preload, "pg_stat_statements")
	switch {
	case loaded && !st.Extension:
		s.Setup = &Command{Stdin: "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;\n"}
	case !loaded && st.Pending != nil && hasLibrary(*st.Pending, "pg_stat_statements"):
		s.Note = "restart"
	case !loaded && !st.Available:
		// Postgres won't start with a library it hasn't got.
		s.Note = "unavailable"
	case !loaded:
		libs := []string{}
		for _, l := range strings.Split(st.Preload, ",") {
			if l = strings.Trim(strings.TrimSpace(l), `"`); l != "" {
				libs = append(libs, sqlString(l))
			}
		}
		libs = append(libs, sqlString("pg_stat_statements"))
		s.Setup = &Command{Stdin: "ALTER SYSTEM SET shared_preload_libraries = " + strings.Join(libs, ", ") + ";\n"}
		s.Note = "restart"
	}
	return s, nil
}

// hasLibrary says whether the shared_preload_libraries list has lib.
func hasLibrary(list, lib string) bool {
	for _, l := range strings.Split(list, ",") {
		if strings.Trim(strings.TrimSpace(l), `"`) == lib {
			return true
		}
	}
	return false
}

// SetupCommand is s.Setup, run as the database's administrator.
func (e Engine) SetupCommand(name string, vars map[string]string, s *Sample) Command {
	c := *s.Setup
	switch e.Name {
	case "postgres":
		c.Argv = psql(name)
	case "mysql":
		c.Argv, c.Env = []string{"mysql", "-uroot", "-N", "-B", name}, mysqlEnv(vars)
	}
	return c
}

// mysqlStats turns performance_schema's instruments off for its own
// session first, so the statements it counts are the apps', all but
// mysqlOwn: the client's opening SELECT @@version_comment, the UPDATE
// itself and the closing Quit.
const mysqlStats = `UPDATE performance_schema.threads SET INSTRUMENTED = 'NO' WHERE PROCESSLIST_ID = CONNECTION_ID();
SELECT JSON_OBJECT(
  'started', UNIX_TIMESTAMP() - (SELECT VARIABLE_VALUE + 0 FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Uptime'),
  'max', @@max_connections,
  'max_used', (SELECT VARIABLE_VALUE + 0 FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Max_used_connections'),
  'requests', (SELECT VARIABLE_VALUE + 0 FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Innodb_buffer_pool_read_requests'),
  'reads', (SELECT VARIABLE_VALUE + 0 FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Innodb_buffer_pool_reads'),
  'clients', (SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT('addr', addr, 'user', usr, 'n', n, 'active', active)), JSON_ARRAY()) FROM (
      SELECT SUBSTRING_INDEX(HOST, ':', 1) AS addr, USER AS usr, COUNT(*) AS n, SUM(COMMAND <> 'Sleep') AS active
      FROM performance_schema.processlist
      WHERE ID <> CONNECTION_ID() AND USER NOT IN ('event_scheduler', 'system user') AND COMMAND <> 'Daemon'
      GROUP BY 1, 2) c),
  'hist', (SELECT COALESCE(JSON_ARRAYAGG(JSON_ARRAY(BUCKET_TIMER_LOW, BUCKET_TIMER_HIGH, COUNT_BUCKET)), JSON_ARRAY())
      FROM performance_schema.events_statements_histogram_global WHERE COUNT_BUCKET > 0),
  'users', (SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT('name', u.User, 'host', u.Host, 'super', u.Super_priv = 'Y',
        'all', COALESCE(d.Create_priv = 'Y' AND d.Alter_priv = 'Y' AND d.Drop_priv = 'Y', 0),
        'can_read', u.Select_priv = 'Y' OR COALESCE(d.Select_priv = 'Y', 0),
        'can_write', u.Insert_priv = 'Y' OR COALESCE(d.Insert_priv = 'Y' AND d.Update_priv = 'Y' AND d.Delete_priv = 'Y', 0))), JSON_ARRAY())
      FROM mysql.user u LEFT JOIN mysql.db d ON d.User = u.User AND d.Host = u.Host AND d.Db = %s
      WHERE u.User NOT LIKE 'mysql.%%'));
`

const mysqlOwn = 3

type myStats struct {
	Started  float64 `json:"started"`
	Max      int     `json:"max"`
	MaxUsed  float64 `json:"max_used"`
	Requests float64 `json:"requests"`
	Reads    float64 `json:"reads"`
	Clients  []struct {
		Addr   string  `json:"addr"`
		User   string  `json:"user"`
		N      int     `json:"n"`
		Active float64 `json:"active"`
	} `json:"clients"`
	Hist  [][3]float64 `json:"hist"`
	Users myUsers      `json:"users"`
}

type myUsers []struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Super    flag   `json:"super"`
	All      flag   `json:"all"`
	CanRead  flag   `json:"can_read"`
	CanWrite flag   `json:"can_write"`
}

// list is the users by name, with a host unless it's any (%).
func (us myUsers) list() []User {
	var out []User
	for _, u := range us {
		// Every privilege on the database's tables (its own user's) takes
		// in schema changes.
		access := accessOf(bool(u.Super || u.All), bool(u.CanRead), bool(u.CanWrite))
		name := u.Name
		if u.Host != "%" {
			name += "@" + u.Host
		}
		out = append(out, User{Name: name, Access: access})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// flag is a MySQL truth value in JSON: true or false, or 1 or 0 from a
// COALESCE.
type flag bool

func (f *flag) UnmarshalJSON(b []byte) error {
	s := string(b)
	*f = flag(s == "true" || (s != "false" && s != "null" && s != "0"))
	return nil
}

func parseMySQL(out []byte) (*Sample, error) {
	var st myStats
	if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
		return nil, fmt.Errorf("reading MySQL's statistics: %w", err)
	}
	if st.Max == 0 {
		return nil, errors.New("MySQL's statistics are empty")
	}
	// The read requests count every read; those that went to disk are
	// the misses.
	s := &Sample{Started: st.Started, MaxConns: st.Max, MaxUsed: int(st.MaxUsed), ActiveKnown: true, Per: "query", Own: mysqlOwn,
		Hits: uint64(max(st.Requests-st.Reads, 0)), Misses: uint64(st.Reads), HitsAre: "cache", Hist: map[Bucket]uint64{}}
	for _, c := range st.Clients {
		addr := c.Addr
		if addr == "localhost" {
			addr = ""
		}
		s.Clients = append(s.Clients, Client{Addr: addr, User: c.User, Count: c.N, Active: int(c.Active)})
	}
	// performance_schema times are in picoseconds.
	for _, b := range st.Hist {
		n := uint64(b[2])
		s.Hist[Bucket{Lo: b[0] / 1e6, Hi: b[1] / 1e6}] += n
		s.Queries += n
	}
	s.Users = st.Users.list()
	return s, nil
}

// redisStats: its own connections are the CLIENT LIST one, and its
// commands are left out of the histogram (redisOwn).
const redisStats = `set -e
redis-cli --no-auth-warning INFO server clients memory stats
echo '#jokku:clients'
redis-cli --no-auth-warning CLIENT LIST
echo '#jokku:latency'
redis-cli --no-auth-warning --json LATENCY HISTOGRAM
`

// redisOwn are the commands the stats (and every client's connecting)
// run, which aren't the apps' queries.
var redisOwn = map[string]bool{"info": true, "client|list": true, "latency|histogram": true, "auth": true, "hello": true}

func parseRedis(out []byte) (*Sample, error) {
	info, rest, ok := strings.Cut(string(out), "#jokku:clients\n")
	if !ok {
		return nil, errors.New("reading Redis's statistics: no client list")
	}
	clients, latency, ok := strings.Cut(rest, "#jokku:latency\n")
	if !ok {
		return nil, errors.New("reading Redis's statistics: no latency histogram")
	}
	kv := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(info))
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), ":"); ok && !strings.HasPrefix(k, "#") {
			kv[k] = v
		}
	}
	num := func(k string) float64 { f, _ := strconv.ParseFloat(kv[k], 64); return f }
	if kv["maxclients"] == "" {
		return nil, errors.New("reading Redis's statistics: no maxclients")
	}
	s := &Sample{Started: num("server_time_usec")/1e6 - num("uptime_in_seconds"), MaxConns: int(num("maxclients")),
		Hits: uint64(num("keyspace_hits")), Misses: uint64(num("keyspace_misses")), HitsAre: "keys", Per: "command",
		MemoryUsed: uint64(num("used_memory")), MemoryMax: uint64(num("maxmemory")), Hist: map[Bucket]uint64{}}
	byAddr := map[[2]string]int{}
	for _, line := range strings.Split(clients, "\n") {
		f := map[string]string{}
		for _, field := range strings.Fields(line) {
			if k, v, ok := strings.Cut(field, "="); ok {
				f[k] = v
			}
		}
		if f["addr"] == "" || f["cmd"] == "client|list" {
			continue
		}
		addr := f["addr"]
		if i := strings.LastIndex(addr, ":"); i >= 0 {
			addr = strings.Trim(addr[:i], "[]")
		}
		byAddr[[2]string{addr, f["user"]}]++
	}
	for k, n := range byAddr {
		s.Clients = append(s.Clients, Client{Addr: k[0], User: k[1], Count: n})
	}
	sort.Slice(s.Clients, func(i, j int) bool { return s.Clients[i].Addr+s.Clients[i].User < s.Clients[j].Addr+s.Clients[j].User })
	// Each command's histogram counts the calls that took up to each
	// bound, in microseconds.
	var hist map[string]struct {
		Calls     uint64            `json:"calls"`
		Histogram map[string]uint64 `json:"histogram_usec"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(latency)), &hist); err != nil {
		return nil, fmt.Errorf("reading Redis's latency histogram: %w", err)
	}
	for cmd, h := range hist {
		if redisOwn[cmd] {
			continue
		}
		bounds := make([]float64, 0, len(h.Histogram))
		for b := range h.Histogram {
			f, err := strconv.ParseFloat(b, 64)
			if err != nil {
				return nil, fmt.Errorf("reading Redis's latency histogram: bucket %q", b)
			}
			bounds = append(bounds, f)
		}
		sort.Float64s(bounds)
		// The buckets double: each holds what took over half its bound.
		var below uint64
		for _, hi := range bounds {
			cum := h.Histogram[strconv.FormatFloat(hi, 'f', -1, 64)]
			if cum > below {
				lo := hi / 2
				if hi <= 1 {
					lo = 0
				}
				s.Hist[Bucket{Lo: lo, Hi: hi}] += cum - below
				below = cum
			}
		}
		s.Queries += h.Calls
	}
	return s, nil
}

// sqlString quotes s as an SQL string literal.
func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func u64(f *float64) uint64 {
	if f == nil || *f < 0 {
		return 0
	}
	return uint64(*f)
}

func f64(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
