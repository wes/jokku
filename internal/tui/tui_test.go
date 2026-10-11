package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/wes/jokku/internal/types"
)

func sampleStatus() *types.ClusterStatus {
	now := time.Now()
	return &types.ClusterStatus{
		Version: "v0.1.0", Control: "control", At: now,
		Totals: types.ClusterTotals{NodesReady: 2, Nodes: 3, Apps: 2, InstancesHealthy: 3, Instances: 4},
		Nodes: []types.NodeView{
			{Node: types.Node{Name: "control", Role: "control", Status: "ready", Address: "192.168.1.10", MeshIP: "10.210.1.1", CPUs: 8, MemoryMB: 16384, LastSeen: now},
				Version: "v0.1.0", Metrics: types.NodeMetrics{CPUPercent: 42, MemoryUsedMB: 6000, DiskMB: 100000, DiskFreeMB: 60000}, AllocatedMB: 1024, Instances: 3},
			{Node: types.Node{Name: "worker-1", Role: "worker", Status: "ready", CPUs: 4, MemoryMB: 8192, LastSeen: now},
				Version: "v0.1.0", Metrics: types.NodeMetrics{CPUPercent: 91}, AllocatedMB: 512, Instances: 1},
			{Node: types.Node{Name: "worker-2", Role: "worker", Status: "down", CPUs: 4, MemoryMB: 8192, LastSeen: now.Add(-5 * time.Minute)},
				Version: "v0.0.9", CanRun: "this CPU is too old for Firecracker"},
		},
		Apps: []types.AppView{
			{Name: "shop", Release: 12, Domains: []string{"shop.example.com"}, Healthy: 2, Wanted: 3,
				Deploy: &types.Deploy{Status: types.StatusSucceeded, CreatedAt: now.Add(-time.Hour)}},
			{Name: "blog", Release: 3, Healthy: 1, Wanted: 1},
		},
		Instances: []types.InstanceView{
			{App: "shop", CPUPercent: 12, MemoryUsed: 180, Instance: types.Instance{Name: "web.1", Release: 12, Node: "control", State: "healthy", IP: "10.210.1.2", Port: 5000, CPUs: 1, MemoryMB: 256, Desired: "running", StartedAt: now.Add(-time.Hour)}},
			{App: "shop", Instance: types.Instance{Name: "web.2", Release: 12, Node: "worker-2", State: "unknown", CPUs: 1, MemoryMB: 256, Desired: "running"}},
			{App: "blog", Instance: types.Instance{Name: "web.1", Release: 3, Node: "worker-1", State: "crashed", Restarts: 3, CPUs: 1, MemoryMB: 256, Desired: "running"}},
		},
		Events: []types.ClusterEvent{
			{At: now, Kind: "node", Node: "worker-2", Message: "worker-2 stopped reporting"},
			{At: now, Kind: "deploy", App: "shop", Message: "deployed v12"},
		},
	}
}

func TestViewsRender(t *testing.T) {
	for _, size := range [][2]int{{160, 45}, {80, 24}, {40, 10}} {
		m := &model{ctx: context.Background(), cursor: map[view]int{}, st: sampleStatus(), at: time.Now()}
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, k := range []string{"1", "2", "j", "j", "j", "enter", "3", "k", "enter", "esc", "4", "G", "5", "?", "x", "tab", "shift+tab"} {
			m.key(k)
			out := m.View()
			if lines := strings.Split(out, "\n"); len(lines) > size[1] {
				t.Errorf("%dx%d after %q: %d lines, taller than the terminal", size[0], size[1], k, len(lines))
			}
			for _, l := range strings.Split(out, "\n") {
				if w := lipgloss.Width(l); w > size[0] {
					t.Errorf("%dx%d after %q: a %d column line, wider than the terminal", size[0], size[1], k, w)
					break
				}
			}
		}
	}

	m := &model{ctx: context.Background(), cursor: map[view]int{}, st: sampleStatus(), at: time.Now()}
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	for v, want := range map[string][]string{
		"1": {"control", "worker-1", "shop", "worker-2 stopped reporting"},
		"2": {"worker-2", "down", "v0.0.9"},
		"3": {"shop", "degraded", "shop.example.com"},
		"4": {"web.1", "crashed", "unknown", "10.210.1.2:5000"},
		"5": {"deployed v12"},
	} {
		m.key(v)
		out := m.View()
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("view %s is missing %q", v, w)
			}
		}
	}
	// Enter on a node filters the instance list to it.
	m.key("2")
	m.key("j") // worker-1
	m.key("enter")
	if m.view != instancesView || m.filterNode != "worker-1" || len(m.instances()) != 1 {
		t.Errorf("drill into node: view %v filter %q instances %d", m.view, m.filterNode, len(m.instances()))
	}
}

// TestTheSelectedRowIsSubtle: the selected row is a neutral shade in every
// color mode, never the red the nearest of 256 colors to a brown would be,
// and it's behind the whole row, styled parts and all.
func TestTheSelectedRowIsSubtle(t *testing.T) {
	defer lipgloss.SetColorProfile(lipgloss.ColorProfile())
	defer lipgloss.SetHasDarkBackground(lipgloss.HasDarkBackground())
	for _, c := range []struct {
		profile termenv.Profile
		dark    bool
		on      string
	}{
		{termenv.TrueColor, true, "\x1b[48;2;47;42;36m"},
		{termenv.TrueColor, false, "\x1b[48;2;236;229;219m"},
		{termenv.ANSI256, true, "\x1b[48;5;236m"},
		{termenv.ANSI256, false, "\x1b[48;5;254m"},
		{termenv.ANSI, true, "\x1b[100m"},
		{termenv.ANSI, false, "\x1b[47m"},
	} {
		lipgloss.SetColorProfile(c.profile)
		lipgloss.SetHasDarkBackground(c.dark)
		m := &model{ctx: context.Background(), cursor: map[view]int{}, st: sampleStatus(), at: time.Now()}
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
		var rows []string
		for _, l := range strings.Split(m.View(), "\n") {
			if strings.Contains(l, c.on) {
				rows = append(rows, l)
			}
			for _, red := range []string{"48;5;52m", "48;5;224m", "\x1b[41m", "\x1b[101m"} {
				if strings.Contains(l, red) {
					t.Errorf("profile %d, dark %v: a red background in %q", c.profile, c.dark, l)
				}
			}
		}
		if len(rows) != 1 || !strings.Contains(rows[0], "shop") {
			t.Errorf("profile %d, dark %v: selected rows %q", c.profile, c.dark, rows)
			continue
		}
		parts := strings.Split(strings.TrimSuffix(rows[0], "\x1b[0m"), "\x1b[0m")
		for _, p := range parts[1:] {
			if !strings.HasPrefix(p, c.on) {
				t.Errorf("profile %d, dark %v: the background stops partway: %q", c.profile, c.dark, rows[0])
				break
			}
		}
	}
}
