package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wes/jokku/internal/types"
)

// The Databases view: what the control node sees inside each database,
// a look every ten seconds. Each row is a database's connections against
// its limit, its queries a second and how long they take; under the
// table, the selected one's clients, users and slowest queries.

type dbStatsMsg struct {
	stats []types.DatabaseStats
	err   error
}

func (m *model) fetchDBStats() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		stats, err := m.api.DatabaseStats(ctx)
		return dbStatsMsg{stats, err}
	}
}

// dbWindows are the spans the table shows, w cycling through them.
var dbWindows = []string{"last minute", "last hour", "last day"}

func (m *model) dbWindow(s types.DatabaseStats) types.DatabaseWindow {
	return [...]types.DatabaseWindow{s.Minute, s.Hour, s.Day}[m.dbWin]
}

func (m *model) selectedDB() (types.DatabaseStats, bool) {
	c := m.cursor[databasesView]
	if c >= len(m.dbs) {
		return types.DatabaseStats{}, false
	}
	return m.dbs[c], true
}

func (m *model) databasesView() string {
	var b strings.Builder
	switch {
	case m.dbs == nil && m.dbErr != nil:
		return sBad.Render("  Cannot read the databases' statistics: " + m.dbErr.Error())
	case m.dbs == nil:
		return sDim.Render("  Loading...")
	case len(m.dbs) == 0:
		return sDim.Render("  No databases yet: jokku db:postgres:create <name> (or mysql, redis)")
	}
	b.WriteString(sHead.Render(fmt.Sprintf("  %-20s %-9s %-26s %6s %8s  %-26s %6s   %s",
		"DATABASE", "ENGINE", "CONNECTIONS", "ACTIVE", "QUERY/S", "LATENCY p50 p95 p99", "HIT", sAccent.Render(dbWindows[m.dbWin]))) + "\n")
	for i, s := range m.dbs {
		w := m.dbWindow(s)
		conns := pad(sDim.Render("-"), 26)
		active, qps, lat, hit := "-", "-", sDim.Render("-"), "-"
		if !s.At.IsZero() {
			conns = bar(pct(s.Connections, s.MaxConnections), 10) + " " + pad(fmt.Sprintf("%d / %d", s.Connections, s.MaxConnections), 15)
			if s.Active >= 0 {
				active = fmt.Sprint(s.Active)
			}
			qps = rate(w.PerSecond)
			lat = latency(w)
			hit = hitRate(w)
		}
		line := fmt.Sprintf("  %s %-18s %-9s %s %6s %8s  %s %6s", dbDot(s), trunc(s.Name, 18), s.Engine, conns, active, qps, pad(lat, 26), hit)
		b.WriteString(m.selectable(databasesView, i, line) + "\n")
	}
	if s, ok := m.selectedDB(); ok {
		b.WriteString(m.dbDetail(s))
	}
	return b.String()
}

// hitRate is how often w's reads were served from memory (Redis: keys
// found).
func hitRate(w types.DatabaseWindow) string {
	switch {
	case w.HitRate < 0:
		return "-"
	case w.HitRate >= 0.9995:
		return "100%"
	}
	return fmt.Sprintf("%.1f%%", w.HitRate*100)
}

// dbDot is a database's state: green when looked inside, amber when the
// look failed, dim when it's stopped or not yet looked at.
func dbDot(s types.DatabaseStats) string {
	switch {
	case s.Error != "":
		return sWarn.Render("●")
	case s.At.IsZero():
		return sDim.Render("○")
	}
	return sGood.Render("●")
}

// latency is how long a window's queries took: percentiles where the
// engine keeps a histogram, the mean where it doesn't (Postgres).
func latency(w types.DatabaseWindow) string {
	switch {
	case w.Queries == 0:
		return sDim.Render("no queries")
	case w.P50 > 0:
		return fmt.Sprintf("%s %s %s", dbMS(w.P50), dbMS(w.P95), sDim.Render(dbMS(w.P99)))
	case w.AvgMS > 0:
		return dbMS(w.AvgMS) + sDim.Render(" avg")
	}
	return "-"
}

func rate(f float64) string {
	switch {
	case f == 0:
		return "0"
	case f < 10:
		return fmt.Sprintf("%.1f", f)
	}
	return fmt.Sprintf("%.0f", f)
}

// dbMS is a query's time: microseconds for Redis's, milliseconds for most.
func dbMS(ms float64) string {
	switch {
	case ms < 1:
		return fmt.Sprintf("%.0fµs", max(ms*1000, 1))
	case ms < 10:
		return fmt.Sprintf("%.1fms", ms)
	case ms < 1000:
		return fmt.Sprintf("%.0fms", ms)
	}
	return fmt.Sprintf("%.1fs", ms/1000)
}

// dbDetail is the selected database's clients, windows, slowest queries and
// users.
func (m *model) dbDetail(s types.DatabaseStats) string {
	var b strings.Builder
	head := fmt.Sprintf("%s %s", s.Engine, s.Name)
	if s.Node != "" {
		head += " on " + s.Node
	}
	if !s.At.IsZero() {
		head += " · looked " + ago(s.At)
		switch s.Per {
		case "transaction":
			head += " · counting transactions"
		case "command":
			head += " · counting commands"
		}
		if s.MaxUsed > 0 {
			head += fmt.Sprintf(" · at most %d connections since it started", s.MaxUsed)
		}
		if s.MemoryUsed > 0 {
			head += " · memory " + mem(int(s.MemoryUsed>>20))
			if s.MemoryMax > 0 {
				head += " of " + mem(int(s.MemoryMax>>20))
			}
		}
	}
	b.WriteString("\n" + sSection.Render(head) + "\n")
	if s.Error != "" {
		b.WriteString("  " + sWarn.Render("can't look inside: "+s.Error) + "\n")
	}
	if s.Note != "" {
		b.WriteString("  " + sDim.Render(s.Note) + "\n")
	}
	if s.At.IsZero() {
		return b.String()
	}
	b.WriteString("  " + sHead.Render(fmt.Sprintf("%-22s %8s  %-30s %6s", "", "PER SEC", "LATENCY", "HIT")) + "\n")
	for i, w := range []types.DatabaseWindow{s.Minute, s.Hour, s.Day} {
		// A span the looks don't cover yet says how much they do.
		label := dbWindows[i]
		if w.Seconds > 0 && w.Seconds < [...]float64{60, 3600, 86400}[i]*0.9 {
			label += sDim.Render(" (" + span(w.Seconds) + ")")
		}
		b.WriteString(fmt.Sprintf("  %s %8s  %s %6s\n", pad(label, 22), rate(w.PerSecond), pad(latencyLong(w), 30), hitRate(w)))
	}
	if len(s.Clients) > 0 {
		b.WriteString("  " + sHead.Render("CONNECTED") + "\n")
		for _, c := range s.Clients {
			from := c.Addr
			switch {
			case c.App != "":
				from = c.App + " " + sDim.Render(c.Instance)
			case c.Via != "":
				from = "outside " + sDim.Render("through "+c.Via)
			case c.Local:
				from = "its own VM " + sDim.Render("(db:connect)")
			}
			n := fmt.Sprint(c.Count)
			if c.Active > 0 {
				n += sAccent.Render(fmt.Sprintf(" (%d active)", c.Active))
			}
			b.WriteString(fmt.Sprintf("    %s %s %s\n", pad(from, 30), pad(sDim.Render(c.User), 14), n))
		}
	}
	if len(s.Slowest) > 0 {
		b.WriteString("  " + sHead.Render("SLOWEST QUERIES, BY TOTAL TIME") + "\n")
		for _, q := range s.Slowest {
			b.WriteString(fmt.Sprintf("    %8s %s  %s\n", dbMS(q.MeanMS), sDim.Render(fmt.Sprintf("×%-7d", q.Calls)), trunc(q.Query, max(m.width-24, 20))))
		}
	}
	if len(s.Users) > 0 {
		var us []string
		for _, u := range s.Users {
			us = append(us, u.Name+sDim.Render(" "+u.Access))
		}
		b.WriteString("  " + sHead.Render("USERS") + "  " + strings.Join(us, sDim.Render(" · ")) + "\n")
	}
	return b.String()
}

// span is a number of seconds, roughly: 45s, 30m, 5.5h.
func span(secs float64) string {
	switch {
	case secs < 60:
		return fmt.Sprintf("%.0fs", secs)
	case secs < 3600:
		return fmt.Sprintf("%.0fm", secs/60)
	}
	return fmt.Sprintf("%.1fh", secs/3600)
}

// latencyLong is latency, with each percentile named.
func latencyLong(w types.DatabaseWindow) string {
	if w.Queries > 0 && w.P50 > 0 {
		return fmt.Sprintf("p50 %s  p95 %s  p99 %s", dbMS(w.P50), dbMS(w.P95), dbMS(w.P99))
	}
	return latency(w)
}
