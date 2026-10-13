package cluster

import (
	"context"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wes/jokku/internal/database"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// DBPollEvery is how often the control node looks inside each running
// database, for "jokku top" (a variable so tests can shorten it).
var DBPollEvery = 10 * time.Second

const (
	dbPollTimeout = 20 * time.Second
	dbKeep        = 24 * time.Hour
	dbRecent      = 5 * time.Minute // per-look points kept, for the last minute
)

// dbStats is what the control node has seen inside the databases, by app.
type dbStats struct {
	mu  sync.Mutex
	dbs map[string]*dbTrack
}

type dbTrack struct {
	busy   bool
	polled time.Time // when the last look started
	inst   string    // the instance last looked in
	node   string
	last   *database.Sample
	lastAt time.Time
	err    string
	setup  string // the instance and start the sample's Setup last ran for

	recent  []dbPoint         // one per look, the last few minutes
	minutes map[int64]*dbAggr // by Unix minute, the last day
}

// dbAggr adds up the looks over a span: how long it was, how many queries
// ran and how long they took, and how reads went.
type dbAggr struct {
	secs         float64
	queries      uint64
	ms           float64        // total time, from engines without a histogram
	hist         map[int]uint64 // queries by usBucket of how long they took
	hits, misses uint64
}

type dbPoint struct {
	at time.Time
	dbAggr
}

func (a *dbAggr) add(o dbAggr) {
	a.secs += o.secs
	a.queries += o.queries
	a.ms += o.ms
	a.hits += o.hits
	a.misses += o.misses
	for b, n := range o.hist {
		if a.hist == nil {
			a.hist = map[int]uint64{}
		}
		a.hist[b] += n
	}
}

// Query times are kept in log-spaced buckets of microseconds: bucket 0
// holds up to a microsecond, bucket i up to usGrowth^i, so a percentile is
// within 5%.
const usGrowth = 1.1

func usBucket(us float64) int {
	if us <= 1 {
		return 0
	}
	return int(math.Ceil(math.Log(us) / math.Log(usGrowth)))
}

func usMiddle(i int) float64 {
	if i == 0 {
		return 1
	}
	return math.Pow(usGrowth, float64(i)-0.5)
}

// percentileMS is the p-th percentile of h, in milliseconds.
func percentileMS(h map[int]uint64, p float64) float64 {
	var total uint64
	keys := make([]int, 0, len(h))
	for b, n := range h {
		total += n
		keys = append(keys, b)
	}
	if total == 0 {
		return 0
	}
	sort.Ints(keys)
	rank := uint64(math.Ceil(p / 100 * float64(total)))
	var seen uint64
	for _, b := range keys {
		if seen += h[b]; seen >= rank {
			return usMiddle(b) / 1000
		}
	}
	return usMiddle(keys[len(keys)-1]) / 1000
}

// delta is what happened inside a database between two looks; ok is false
// when the counters started over (a restart) or the looks are from
// different instances.
func delta(prev, cur *database.Sample, secs float64) (dbAggr, bool) {
	if math.Abs(cur.Started-prev.Started) > 2 || cur.Queries < prev.Queries || cur.Hits < prev.Hits ||
		cur.Misses < prev.Misses || cur.QueryMS < prev.QueryMS || cur.Per != prev.Per {
		return dbAggr{}, false
	}
	d := dbAggr{secs: secs, queries: cur.Queries - prev.Queries, ms: cur.QueryMS - prev.QueryMS,
		hits: cur.Hits - prev.Hits, misses: cur.Misses - prev.Misses}
	for b, n := range cur.Hist {
		p := prev.Hist[b]
		if n < p {
			return dbAggr{}, false
		}
		if n > p {
			if d.hist == nil {
				d.hist = map[int]uint64{}
			}
			d.hist[usBucket(b.Mid())] += n - p
		}
	}
	// The look's own queries, which the engine counted: they're quick, so
	// they come out of the fastest buckets.
	own := min(cur.Own, d.queries)
	d.queries -= own
	if len(d.hist) > 0 {
		keys := make([]int, 0, len(d.hist))
		for b := range d.hist {
			keys = append(keys, b)
		}
		sort.Ints(keys)
		for _, b := range keys {
			take := min(own, d.hist[b])
			if d.hist[b] -= take; d.hist[b] == 0 {
				delete(d.hist, b)
			}
			if own -= take; own == 0 {
				break
			}
		}
	}
	return d, true
}

// record keeps a look at app's database, taken in instance in at now.
func (s *dbStats) record(app string, in store.Instance, sample *database.Sample, err error, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.track(app)
	t.busy = false
	if err != nil {
		t.err = err.Error()
		return
	}
	t.err = ""
	if t.last != nil && t.last.Per != sample.Per {
		// It counts something else now (Postgres's statements, once
		// pg_stat_statements loads): what it counted before doesn't add up
		// with it.
		t.recent, t.minutes = nil, map[int64]*dbAggr{}
	}
	if t.last != nil && t.inst == in.ID {
		if d, ok := delta(t.last, sample, now.Sub(t.lastAt).Seconds()); ok {
			t.recent = append(t.recent, dbPoint{now, d})
			for len(t.recent) > 0 && now.Sub(t.recent[0].at) > dbRecent {
				t.recent = t.recent[1:]
			}
			m := now.Unix() / 60
			if t.minutes[m] == nil {
				t.minutes[m] = &dbAggr{}
			}
			t.minutes[m].add(d)
			for k := range t.minutes {
				if k < (now.Add(-dbKeep).Unix())/60 {
					delete(t.minutes, k)
				}
			}
		}
	}
	t.inst, t.node, t.last, t.lastAt = in.ID, in.Node, sample, now
}

func (s *dbStats) track(app string) *dbTrack {
	if s.dbs == nil {
		s.dbs = map[string]*dbTrack{}
	}
	t := s.dbs[app]
	if t == nil {
		t = &dbTrack{minutes: map[int64]*dbAggr{}}
		s.dbs[app] = t
	}
	return t
}

// start says whether to look inside app's database now, and if so marks it
// as being looked at.
func (s *dbStats) start(app string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.track(app)
	if t.busy || now.Sub(t.polled) < DBPollEvery {
		return false
	}
	t.busy, t.polled = true, now
	return true
}

// needsSetup says whether sample's Setup is yet to run for the server it
// came from, and if so notes that it ran.
func (s *dbStats) needsSetup(app string, in store.Instance, sample *database.Sample) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%s@%.0f", in.ID, sample.Started)
	t := s.track(app)
	if t.setup == key {
		return false
	}
	t.setup = key
	return true
}

// forget drops what's kept of databases that are gone.
func (s *dbStats) forget(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for app := range s.dbs {
		if !keep[app] {
			delete(s.dbs, app)
		}
	}
}

// tickDBStats starts a look inside every running database that's due one.
func (c *Controller) tickDBStats(ctx context.Context, now time.Time) error {
	apps, err := c.Store.Apps(ctx)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, a := range apps {
		e, ok := database.Lookup(a.Kind)
		if !ok {
			continue
		}
		keep[a.Name] = true
		if a.Stopped {
			continue
		}
		in, ok, err := c.databaseInstance(ctx, a.Name, e)
		if err != nil {
			return err
		}
		if !ok || !c.dbstats.start(a.Name, now) {
			continue
		}
		go c.pollDatabase(ctx, a.Name, e, in)
	}
	c.dbstats.forget(keep)
	return nil
}

// databaseInstance is the running instance of app's database.
func (c *Controller) databaseInstance(ctx context.Context, app string, e database.Engine) (store.Instance, bool, error) {
	insts, err := c.Store.Instances(ctx, app)
	if err != nil {
		return store.Instance{}, false, err
	}
	for _, in := range insts {
		if in.ProcessType == e.Process() && in.Desired == store.DesiredRunning && in.State == store.StateHealthy && in.IP != "" {
			return in, true, nil
		}
	}
	return store.Instance{}, false, nil
}

// pollDatabase looks inside app's database, and turns on what the look
// needs the first time it's missing (Postgres's pg_stat_statements).
func (c *Controller) pollDatabase(ctx context.Context, app string, e database.Engine, in store.Instance) {
	ctx, cancel := context.WithTimeout(ctx, dbPollTimeout)
	defer cancel()
	name := strings.TrimPrefix(app, e.Name+"-")
	vars, err := c.Store.ConfigVars(ctx, app)
	var sample *database.Sample
	if err == nil {
		cmd := e.StatsCommand(name, vars)
		var out []byte
		if out, err = c.Exec(ctx, in, cmd.Argv, cmd.Env, cmd.Stdin); err == nil {
			sample, err = e.ParseStats(out)
		}
	}
	c.dbstats.record(app, in, sample, err, time.Now())
	if err != nil {
		c.Log.Debug("looking inside a database", "app", app, "err", err)
		return
	}
	if sample.Setup == nil || !c.dbstats.needsSetup(app, in, sample) {
		return
	}
	cmd := e.SetupCommand(name, vars, sample)
	if _, err := c.Exec(ctx, in, cmd.Argv, cmd.Env, cmd.Stdin); err != nil {
		c.Log.Warn("turning on a database's statistics", "app", app, "err", err)
		return
	}
	if sample.Note == "restart" {
		c.Store.AddEvent(ctx, "app", app, "", "turned on query statistics (pg_stat_statements) in %s database %s; they start at its next restart: jokku db:%s:restart %s",
			e.Name, name, e.Name, name)
	}
}

// DatabaseStats is what the control node has seen inside every database.
func (c *Controller) DatabaseStats(ctx context.Context) ([]types.DatabaseStats, error) {
	apps, err := c.Store.Apps(ctx)
	if err != nil {
		return nil, err
	}
	insts, err := c.Store.Instances(ctx, "")
	if err != nil {
		return nil, err
	}
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	who := c.clientNames(insts, nodes)
	out := []types.DatabaseStats{}
	now := time.Now()
	for _, a := range apps {
		e, ok := database.Lookup(a.Kind)
		if !ok {
			continue
		}
		out = append(out, c.dbstats.view(a, e, who, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DatabaseStatsOf is DatabaseStats for one database's app.
func (c *Controller) DatabaseStatsOf(ctx context.Context, app string) (*types.DatabaseStats, error) {
	all, err := c.DatabaseStats(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.App == app {
			return &s, nil
		}
	}
	return nil, nil
}

// clientNames says who connects from an address: an app's instance, or a
// node (its proxy carrying clients outside, from its mesh address).
func (c *Controller) clientNames(insts []store.Instance, nodes []store.Node) func(addr string) types.DatabaseClient {
	byIP := map[string]store.Instance{}
	for _, in := range insts {
		if in.IP != "" {
			byIP[in.IP] = in
		}
	}
	nodeByIP := map[string]string{}
	for _, n := range nodes {
		_, ip := NodeSubnet(c.ClusterCIDR, n.SubnetIndex)
		nodeByIP[ip.String()] = n.Name
	}
	return func(addr string) types.DatabaseClient {
		if addr == "" {
			return types.DatabaseClient{Local: true}
		}
		if ip, err := netip.ParseAddr(addr); err == nil {
			addr = ip.Unmap().String()
		}
		if in, ok := byIP[addr]; ok {
			return types.DatabaseClient{App: in.App, Instance: in.Name(), Addr: addr}
		}
		if n, ok := nodeByIP[addr]; ok {
			return types.DatabaseClient{Via: n, Addr: addr}
		}
		return types.DatabaseClient{Addr: addr}
	}
}

// view is what's been seen inside a's database, e.
func (s *dbStats) view(a types.App, e database.Engine, who func(string) types.DatabaseClient, now time.Time) types.DatabaseStats {
	name := strings.TrimPrefix(a.Name, e.Name+"-")
	v := types.DatabaseStats{App: a.Name, Engine: e.Name, Name: name, Active: -1,
		Minute: emptyWindow, Hour: emptyWindow, Day: emptyWindow}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.dbs[a.Name]
	switch {
	case a.Stopped:
		v.Note = "stopped"
		return v
	case t == nil || (t.last == nil && t.err == ""):
		v.Note = "not looked at yet"
		return v
	}
	v.Node, v.Error = t.node, t.err
	if t.last == nil {
		return v
	}
	l := t.last
	v.At, v.MaxConnections, v.MaxUsed, v.Per, v.HitsAre = t.lastAt, l.MaxConns, l.MaxUsed, l.Per, l.HitsAre
	v.MemoryUsed, v.MemoryMax = l.MemoryUsed, l.MemoryMax
	if l.ActiveKnown {
		v.Active = 0
	}
	for _, cl := range l.Clients {
		dc := who(cl.Addr)
		dc.User, dc.Count, dc.Active = cl.User, cl.Count, cl.Active
		v.Clients = append(v.Clients, dc)
		v.Connections += cl.Count
		if l.ActiveKnown {
			v.Active += cl.Active
		}
	}
	for _, q := range l.Slowest {
		v.Slowest = append(v.Slowest, types.DatabaseQuery{Query: q.Text, Calls: q.Calls, TotalMS: q.TotalMS, MeanMS: q.MeanMS})
	}
	for _, u := range l.Users {
		v.Users = append(v.Users, types.DatabaseUser{Name: u.Name, Access: u.Access})
	}
	switch l.Note {
	case "restart":
		v.Note = fmt.Sprintf("query times per statement start at its next restart: jokku db:%s:restart %s", e.Name, name)
	case "unavailable":
		v.Note = "its image hasn't got pg_stat_statements, so query times are per transaction"
	}

	var minute, hour, day dbAggr
	// A point covers the interval up to its look.
	for _, p := range t.recent {
		if now.Sub(p.at) < time.Minute {
			minute.add(p.dbAggr)
		}
	}
	for m, a := range t.minutes {
		if m > (now.Add(-time.Hour).Unix())/60 {
			hour.add(*a)
		}
		day.add(*a)
	}
	v.Minute, v.Hour, v.Day = minute.window(), hour.window(), day.window()
	return v
}

var emptyWindow = types.DatabaseWindow{HitRate: -1}

func (a dbAggr) window() types.DatabaseWindow {
	w := types.DatabaseWindow{Seconds: a.secs, Queries: a.queries, HitRate: -1}
	if a.secs > 0 {
		w.PerSecond = float64(a.queries) / a.secs
	}
	if reads := a.hits + a.misses; reads > 0 {
		w.HitRate = float64(a.hits) / float64(reads)
	}
	if a.queries == 0 {
		return w
	}
	if len(a.hist) > 0 {
		w.P50, w.P95, w.P99 = percentileMS(a.hist, 50), percentileMS(a.hist, 95), percentileMS(a.hist, 99)
		var n uint64
		var us float64
		for b, c := range a.hist {
			n += c
			us += usMiddle(b) * float64(c)
		}
		if n > 0 {
			w.AvgMS = us / float64(n) / 1000
		}
	} else if a.ms > 0 {
		w.AvgMS = a.ms / float64(a.queries)
	}
	return w
}
