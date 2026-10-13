package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/wes/jokku/internal/types"
)

func sampleDBStats() []types.DatabaseStats {
	now := time.Now()
	return []types.DatabaseStats{
		{App: "postgres-shopdb", Engine: "postgres", Name: "shopdb", Node: "jokku1", At: now.Add(-3 * time.Second),
			Connections: 12, MaxConnections: 97, Active: 2, Per: "transaction", HitsAre: "cache",
			Note:   "query times per statement start at its next restart: jokku db:postgres:restart shopdb",
			Minute: types.DatabaseWindow{Seconds: 60, Queries: 1260, PerSecond: 21, AvgMS: 2.1, HitRate: 0.995},
			Hour:   types.DatabaseWindow{Seconds: 1800, Queries: 30000, PerSecond: 16.7, AvgMS: 2.4, HitRate: 0.99},
			Day:    types.DatabaseWindow{Seconds: 1800, Queries: 30000, PerSecond: 16.7, AvgMS: 2.4, HitRate: 0.99},
			Clients: []types.DatabaseClient{
				{App: "shop", Instance: "web.1", User: "postgres", Count: 10, Active: 2},
				{Via: "jokku-edge1", User: "reporting", Count: 2},
			},
			Users: []types.DatabaseUser{{Name: "postgres", Access: "all"}, {Name: "reporting", Access: "read-only"}}},
		{App: "mysql-orders", Engine: "mysql", Name: "orders", Node: "jokku3", At: now, Connections: 3, MaxConnections: 151, Active: 0,
			Per: "query", MaxUsed: 9,
			Minute:  types.DatabaseWindow{Seconds: 60, Queries: 600, PerSecond: 10, P50: 1.5, P95: 10.4, P99: 104, AvgMS: 3.3, HitRate: -1},
			Hour:    types.DatabaseWindow{Seconds: 3600, HitRate: -1},
			Day:     types.DatabaseWindow{Seconds: 3600, HitRate: -1},
			Slowest: []types.DatabaseQuery{{Query: "SELECT * FROM orders WHERE customer_id = ?", Calls: 4100, MeanMS: 12.5}}},
		{App: "redis-cache", Engine: "redis", Name: "cache", Note: "stopped", Active: -1,
			Minute: types.DatabaseWindow{HitRate: -1}, Hour: types.DatabaseWindow{HitRate: -1}, Day: types.DatabaseWindow{HitRate: -1}},
	}
}

func TestDatabasesView(t *testing.T) {
	m := &model{ctx: context.Background(), cursor: map[view]int{}, st: sampleStatus(), at: time.Now()}
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 50})
	m.key("8")
	if m.view != databasesView {
		t.Fatalf("8 is view %v", m.view)
	}
	if out := ansi.Strip(m.View()); !strings.Contains(out, "Loading") {
		t.Errorf("before the first answer:\n%s", out)
	}
	m.Update(dbStatsMsg{stats: sampleDBStats()})
	out := ansi.Strip(m.View())
	for _, want := range []string{
		"8 Databases", "last minute",
		"shopdb", "12 / 97", "21", "2.1ms avg", "99.5%",
		"orders", "3 / 151", "1.5ms 10ms 104ms",
		"cache", // stopped: listed, with nothing to show
		// The selected database's detail.
		"postgres shopdb on jokku1 · looked 3s ago · counting transactions", "next restart", "last hour (30m)",
		"shop web.1", "outside through jokku-edge1", "(2 active)",
		"USERS", "reporting read-only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SLOWEST") {
		t.Errorf("shopdb has no slowest queries, but:\n%s", out)
	}

	m.key("w")
	if out := ansi.Strip(m.View()); !strings.Contains(out, "last hour") || !strings.Contains(out, "2.4ms avg") {
		t.Errorf("w doesn't show the last hour:\n%s", out)
	}
	m.key("j")
	out = ansi.Strip(m.View())
	for _, want := range []string{"mysql orders on jokku3", "at most 9 connections", "SLOWEST", "SELECT * FROM orders", "×4100", "p50 1.5ms  p95 10ms  p99 104ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in orders's detail:\n%s", want, out)
		}
	}
	m.key("enter")
	if m.view != instancesView || m.filterApp != "mysql-orders" {
		t.Errorf("enter: view %v, filter %q", m.view, m.filterApp)
	}
	// Tab wraps from Firewall, the last view, to the first.
	m.key("9")
	m.key("tab")
	if m.view != overview {
		t.Errorf("tab from the last view: %v", m.view)
	}
}
