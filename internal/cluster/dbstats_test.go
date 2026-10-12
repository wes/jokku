package cluster

import (
	"errors"
	"math"
	"net/netip"
	"testing"
	"time"

	"github.com/wes/jokku/internal/database"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// within says whether got is within 5% of want.
func within(got, want float64) bool { return math.Abs(got-want) <= want*0.05 }

func TestDatabaseWindows(t *testing.T) {
	var s dbStats
	in := store.Instance{ID: "i1", Node: "w1", ProcessType: "mysql", Index: 1}
	t0 := time.Unix(1_800_000_000, 0)
	// A MySQL that runs 100 queries a second: 90 in 1-2 ms, 9 in 10-11 ms
	// and 1 in 100-110 ms, plus the stats' own 3 under 10 µs each.
	quick, mid, slow, own := database.Bucket{Lo: 1000, Hi: 2000}, database.Bucket{Lo: 10000, Hi: 11000},
		database.Bucket{Lo: 100000, Hi: 110000}, database.Bucket{Lo: 0, Hi: 10}
	look := func(i int) *database.Sample {
		n := uint64(i)
		return &database.Sample{Started: 1, Per: "query", Own: 3, MaxConns: 151, ActiveKnown: true,
			Queries: n * 1003, Hits: n * 990, Misses: n * 10,
			Hist:    map[database.Bucket]uint64{quick: n * 900, mid: n * 90, slow: n * 10, own: n * 3},
			Clients: []database.Client{{Addr: "10.210.2.5", User: "mysql", Count: 4, Active: 1}, {Addr: "10.210.3.1", User: "mysql", Count: 1}}}
	}
	for i := 0; i <= 60; i++ { // ten minutes, ten seconds apart
		s.record("mysql-orders", in, look(i), nil, t0.Add(time.Duration(i)*10*time.Second))
	}
	now := t0.Add(600 * time.Second)
	who := func(addr string) types.DatabaseClient {
		return map[string]types.DatabaseClient{"10.210.2.5": {App: "shop", Instance: "web.1"}, "10.210.3.1": {Via: "w3"}}[addr]
	}
	v := s.view(types.App{Name: "mysql-orders", Kind: "mysql"}, mustEngine("mysql"), who, now)
	if v.Name != "orders" || v.Node != "w1" || v.Connections != 5 || v.Active != 1 || v.MaxConnections != 151 || v.Error != "" {
		t.Fatalf("view: %+v", v)
	}
	if c := v.Clients; len(c) != 2 || c[0].App != "shop" || c[0].Instance != "web.1" || c[1].Via != "w3" || c[0].User != "mysql" {
		t.Errorf("clients: %+v", c)
	}
	for name, w := range map[string]types.DatabaseWindow{"minute": v.Minute, "hour": v.Hour, "day": v.Day} {
		if !within(w.PerSecond, 100) || !within(w.P50, 1.5) || !within(w.P95, 10.5) || !within(w.P99, 10.5) ||
			!within(w.HitRate, 0.99) || !within(w.AvgMS, 0.9*1.5+0.09*10.5+0.01*105) {
			t.Errorf("%s: %+v", name, w)
		}
	}
	if !within(v.Minute.Seconds, 60) || !within(v.Hour.Seconds, 600) || v.Day.Queries != 60*1000 {
		t.Errorf("spans: minute %+v, hour %+v, day %+v", v.Minute, v.Hour, v.Day)
	}

	// A restart starts the counters over: no negative interval, and the
	// next look counts again.
	s.record("mysql-orders", in, &database.Sample{Started: 900, Per: "query", Own: 3, MaxConns: 151, Hist: map[database.Bucket]uint64{}}, nil, now.Add(10*time.Second))
	s.record("mysql-orders", in, &database.Sample{Started: 900, Per: "query", Own: 3, MaxConns: 151, Queries: 1003,
		Hist: map[database.Bucket]uint64{quick: 1000, own: 3}}, nil, now.Add(20*time.Second))
	v = s.view(types.App{Name: "mysql-orders", Kind: "mysql"}, mustEngine("mysql"), who, now.Add(20*time.Second))
	if v.Day.Queries != 61*1000 || !within(v.Minute.P50, 1.5) {
		t.Errorf("after a restart: day %+v, minute %+v", v.Day, v.Minute)
	}

	// A failed look says why, and keeps what was seen.
	s.record("mysql-orders", in, nil, errors.New("mysql: can't connect"), now.Add(30*time.Second))
	v = s.view(types.App{Name: "mysql-orders", Kind: "mysql"}, mustEngine("mysql"), who, now.Add(30*time.Second))
	if v.Error != "mysql: can't connect" || v.Day.Queries != 61*1000 {
		t.Errorf("after a failed look: %+v", v)
	}
	if v := s.view(types.App{Name: "mysql-orders", Kind: "mysql", Stopped: true}, mustEngine("mysql"), who, now); v.Note != "stopped" {
		t.Errorf("stopped: %+v", v)
	}
}

func TestPostgresAverages(t *testing.T) {
	var s dbStats
	in := store.Instance{ID: "i1", Node: "w1", ProcessType: "postgres", Index: 1}
	t0 := time.Unix(1_800_000_000, 0)
	// 50 transactions a second of 4 ms each, besides the stats' own 5.
	for i := 0; i <= 6; i++ {
		n := uint64(i)
		s.record("postgres-shop", in, &database.Sample{Started: 1, Per: "transaction", Own: 5, MaxConns: 97,
			Queries: n * 505, QueryMS: float64(n) * 2000, Note: "restart"}, nil, t0.Add(time.Duration(i)*10*time.Second))
	}
	v := s.view(types.App{Name: "postgres-shop", Kind: "postgres"}, mustEngine("postgres"), nil, t0.Add(60*time.Second))
	if !within(v.Minute.PerSecond, 50) || !within(v.Minute.AvgMS, 4) || v.Minute.P50 != 0 || v.Per != "transaction" ||
		v.Minute.HitRate != -1 || v.Note != "query times per statement start at its next restart: jokku db:postgres:restart shop" {
		t.Errorf("%+v", v)
	}
	// Restarted with pg_stat_statements, it counts statements: the
	// transactions before don't add up with them.
	for i := 0; i <= 2; i++ {
		n := uint64(i)
		s.record("postgres-shop", in, &database.Sample{Started: 100, Per: "query", MaxConns: 97, Queries: n * 300, QueryMS: float64(n) * 300},
			nil, t0.Add(time.Duration(70+i*10)*time.Second))
	}
	v = s.view(types.App{Name: "postgres-shop", Kind: "postgres"}, mustEngine("postgres"), nil, t0.Add(90*time.Second))
	if v.Per != "query" || v.Day.Queries != 600 || !within(v.Day.AvgMS, 1) || v.Note != "" {
		t.Errorf("after loading pg_stat_statements: %+v", v)
	}
}

func TestPercentiles(t *testing.T) {
	h := map[int]uint64{}
	for us := 1.0; us <= 1000; us++ { // 1 µs to 1 ms, evenly
		h[usBucket(us)]++
	}
	for p, want := range map[float64]float64{50: 0.5, 95: 0.95, 99: 0.99} {
		if got := percentileMS(h, p); !within(got, want) {
			t.Errorf("p%v: %v, want %v", p, got, want)
		}
	}
	if percentileMS(map[int]uint64{}, 50) != 0 {
		t.Error("an empty histogram has a percentile")
	}
}

func TestClientNames(t *testing.T) {
	c := &Controller{ClusterCIDR: netip.MustParsePrefix("10.210.0.0/16")}
	who := c.clientNames([]store.Instance{{App: "shop", ProcessType: "web", Index: 2, IP: "10.210.2.7"}},
		[]store.Node{{Name: "jokku-edge1", SubnetIndex: 6}})
	for addr, want := range map[string]types.DatabaseClient{
		"":           {Local: true},
		"10.210.2.7": {App: "shop", Instance: "web.2", Addr: "10.210.2.7"},
		"10.210.6.1": {Via: "jokku-edge1", Addr: "10.210.6.1"},
		"192.0.2.9":  {Addr: "192.0.2.9"},
	} {
		if got := who(addr); got != want {
			t.Errorf("%q: %+v, want %+v", addr, got, want)
		}
	}
}

func mustEngine(name string) database.Engine {
	e, _ := database.Lookup(name)
	return e
}
