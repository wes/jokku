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

// sampleNetTest is five nodes over the mesh, where jokku3's port came up
// at 100 Mbit/s (slow) when slow is set, and one echo to jokku4 got lost.
func sampleNetTest(finished time.Time, slow bool) types.NetTest {
	nodes := []string{"jokku1", "jokku2", "jokku3", "jokku4", "jokku5"}
	t := types.NetTest{Started: finished.Add(-90 * time.Second), Finished: finished, Seconds: 3, Nodes: nodes}
	for _, from := range nodes {
		for _, to := range nodes {
			if from == to {
				continue
			}
			l := types.NetLink{From: from, To: to, Mbps: 896, Ping: types.PingStats{Sent: 100, Received: 100, Min: 0.6, Avg: 0.98, Max: 1.4, Jitter: 0.1}}
			if slow && (from == "jokku3" || to == "jokku3") {
				l.Mbps = 94
			}
			if from == "jokku1" && to == "jokku4" {
				l.Ping.Received = 99
			}
			t.Links = append(t.Links, l)
		}
	}
	return t
}

func TestNetworkView(t *testing.T) {
	m := &model{ctx: context.Background(), cursor: map[view]int{}, st: sampleStatus(), at: time.Now(), bkBusy: map[string]string{}}
	m.Update(tea.WindowSizeMsg{Width: 150, Height: 50})
	m.key("0")
	if m.view != networkView {
		t.Fatalf("0 is view %v", m.view)
	}
	m.Update(netTestsMsg{nt: &types.NetTests{}})
	if out := ansi.Strip(m.View()); !strings.Contains(out, "No network test yet. Press t") {
		t.Errorf("before any test:\n%s", out)
	}

	now := time.Now()
	m.Update(netTestsMsg{nt: &types.NetTests{Tests: []types.NetTest{
		sampleNetTest(now.Add(-4*time.Minute), true),
		sampleNetTest(now.Add(-2*time.Hour), false),
	}}})
	out := ansi.Strip(m.View())
	for _, want := range []string{
		"0 Network", "Tested 4m ago", "in 1m30s · 5 nodes, 20 links over the mesh, 3s each",
		"usually carries 896 Mbit/s and answers in 0.98 ms", "jokku3 sends at 10% of the usual speed",
		"THROUGHPUT", "from  jokku1", "ROUND TRIP", "0.98 1✗",
		"VS. 2H AGO", "-90% speed, same ms", "same speed, same ms",
		"t test the network",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}

	// t asks before loading every link; t again starts the test.
	if cmd := m.key("t"); cmd != nil || !strings.Contains(m.bkFlash, "t again to test") || !strings.Contains(m.bkFlash, "1m30s") {
		t.Errorf("first t: %q", m.bkFlash)
	}
	if cmd := m.key("t"); cmd == nil {
		t.Error("the second t started nothing")
	}

	// While it runs: progress, and the links measured so far.
	running := sampleNetTest(now, false)
	running.Links = running.Links[:7]
	m.Update(netTestsMsg{nt: &types.NetTests{
		Running: &types.NetTestProgress{Started: now.Add(-31 * time.Second), Done: 7, Total: 20, From: "jokku2", To: "jokku4", Test: &running},
		Tests:   []types.NetTest{sampleNetTest(now.Add(-4*time.Minute), true)},
	}})
	out = ansi.Strip(m.View())
	for _, want := range []string{"● Testing jokku2 → jokku4", "7 of 20 links", "31s", "…"} {
		if !strings.Contains(out, want) {
			t.Errorf("running: missing %q in:\n%s", want, out)
		}
	}
	if m.key("t"); !strings.Contains(m.bkFlash, "a test is running") {
		t.Errorf("t while running: %q", m.bkFlash)
	}
}

// On a narrow terminal the tabs give up their padding, then their names,
// but the current view's tab always shows its name.
func TestTabsFitNarrowTerminals(t *testing.T) {
	m := &model{ctx: context.Background(), cursor: map[view]int{}, st: sampleStatus(), at: time.Now(), bkBusy: map[string]string{}}
	for _, width := range []int{160, 100, 80} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
		m.key("0")
		row := ansi.Strip(m.tabs())
		if w := ansi.StringWidth(row); w > width {
			t.Errorf("%d columns: the tabs take %d: %s", width, w, row)
		}
		if !strings.Contains(row, "0 Network") {
			t.Errorf("%d columns: %s", width, row)
		}
	}
}
