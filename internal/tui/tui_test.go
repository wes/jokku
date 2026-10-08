package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
			{App: "shop", CPUPercent: 12, MemoryUsed: 180, Instance: types.Instance{Name: "web.1", Node: "control", State: "healthy", IP: "10.210.1.2", Port: 5000, CPUs: 1, MemoryMB: 256, Desired: "running", StartedAt: now.Add(-time.Hour)}},
			{App: "shop", Instance: types.Instance{Name: "web.2", Node: "worker-2", State: "unknown", CPUs: 1, MemoryMB: 256, Desired: "running"}},
			{App: "blog", Instance: types.Instance{Name: "web.1", Node: "worker-1", State: "crashed", Restarts: 3, CPUs: 1, MemoryMB: 256, Desired: "running"}},
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
