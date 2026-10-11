package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/wes/jokku/internal/types"
)

// The Traffic view's headline numbers are the last few seconds'; under them
// are the last hour's and day's, which the control node gathers from every
// node's proxy. They change slowly, so they are fetched now and then.

// latencyEvery is how often the hour and day are fetched while the Traffic
// view is open.
const latencyEvery = 30 * time.Second

type latencyMsg struct {
	l   *types.Latency
	err error
}

func (m *model) fetchLatency() tea.Cmd {
	m.latAt = time.Now()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
		defer cancel()
		l, err := m.api.Latency(ctx)
		return latencyMsg{l, err}
	}
}

// latencyLine is the last hour's and day's p50 and p95, with how many
// requests they come from; shorter where it doesn't fit. Nothing before the
// first answer, or from a control node a release behind.
func (m *model) latencyLine() string {
	l := m.lat
	if l == nil {
		return ""
	}
	window := func(label string, w types.LatencyStats, counts bool) string {
		if w.Requests == 0 {
			return sDim.Render(label + "  no requests")
		}
		s := sDim.Render(label+"  ") + fmt.Sprintf("p50 %s  p95 %s", fmtMS(w.P50), fmtMS(w.P95))
		if counts {
			s += sDim.Render("  " + requestCount(w.Requests))
		}
		return s
	}
	missing := ""
	if len(l.Missing) > 0 {
		missing = sDim.Render("  ·  without " + strings.Join(l.Missing, ", "))
	}
	forms := []struct {
		hour, day string
		counts    bool
	}{{"last hour", "last 24h", true}, {"1h", "24h", true}, {"1h", "24h", false}}
	var line string
	for _, f := range forms {
		line = window(f.hour, l.Hour.LatencyStats, f.counts) + sDim.Render("   ·   ") + window(f.day, l.Day.LatencyStats, f.counts)
		if lipgloss.Width("  "+line+missing) <= m.width {
			return line + missing
		}
	}
	return line // the shortest, without the missing nodes
}

// requestCount is n requests, shortened past ten thousand.
func requestCount(n uint64) string {
	switch {
	case n == 1:
		return "1 request"
	case n < 10_000:
		s := fmt.Sprint(n)
		if n >= 1000 {
			s = s[:len(s)-3] + "," + s[len(s)-3:]
		}
		return s + " requests"
	case n < 1_000_000:
		return fmt.Sprintf("%dk requests", n/1000)
	}
	return fmt.Sprintf("%.1fM requests", float64(n)/1e6)
}
