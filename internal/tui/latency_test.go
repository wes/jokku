package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/wes/jokku/internal/types"
)

func sampleLatency() *types.Latency {
	return &types.Latency{
		Hour: types.LatencyWindow{LatencyStats: types.LatencyStats{Requests: 2340, P50: 42.3, P95: 212}},
		Day:  types.LatencyWindow{LatencyStats: types.LatencyStats{Requests: 51204, P50: 38.6, P95: 1530}},
	}
}

func TestTheTrafficViewShowsTheHourAndTheDay(t *testing.T) {
	m := trafficModel(160, 40)
	if line := m.latencyLine(); line != "" {
		t.Errorf("a line before the first answer: %q", line)
	}
	m.Update(latencyMsg{l: sampleLatency()})
	out := ansi.Strip(m.View())
	want := "last hour  p50 42ms  p95 212ms  2,340 requests   ·   last 24h  p50 39ms  p95 1.5s  51k requests"
	if !strings.Contains(out, want) {
		t.Errorf("traffic view lacks %q:\n%s", want, out)
	}
	if os.Getenv("PRINT_TUI") != "" {
		fmt.Println(out)
	}

	// Narrower, it shortens; and it never overflows.
	for w, want := range map[int]string{
		90: "1h  p50 42ms  p95 212ms  2,340 requests   ·   24h  p50 39ms  p95 1.5s  51k requests",
		60: "1h  p50 42ms  p95 212ms   ·   24h  p50 39ms  p95 1.5s",
	} {
		m := trafficModel(w, 30)
		m.Update(latencyMsg{l: sampleLatency()})
		if got := ansi.Strip(m.latencyLine()); got != want {
			t.Errorf("at %d columns: %q, want %q", w, got, want)
		}
		for _, l := range strings.Split(m.View(), "\n") {
			if lipgloss.Width(l) > w {
				t.Errorf("at %d columns, a line of %d: %q", w, lipgloss.Width(l), ansi.Strip(l))
			}
		}
	}

	// Nodes that didn't answer are named; a quiet window says so.
	l := sampleLatency()
	l.Missing, l.Hour = []string{"jokku3"}, types.LatencyWindow{}
	m.Update(latencyMsg{l: l})
	if got := ansi.Strip(m.latencyLine()); !strings.Contains(got, "last hour  no requests") || !strings.HasSuffix(got, "without jokku3") {
		t.Errorf("latency line: %q", got)
	}
}

func TestRequestCount(t *testing.T) {
	for n, want := range map[uint64]string{1: "1 request", 999: "999 requests", 2340: "2,340 requests", 51204: "51k requests", 2_400_000: "2.4M requests"} {
		if got := requestCount(n); got != want {
			t.Errorf("requestCount(%d) = %q, want %q", n, got, want)
		}
	}
}
