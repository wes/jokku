package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/wes/jokku/internal/types"
)

func trafficModel(w, h int) *model {
	m := &model{ctx: context.Background(), cursor: map[view]int{}, st: sampleStatus(), at: time.Now(), view: trafficView}
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.tr = &traffic{lanes: map[string][]particle{}, connected: true}
	now := time.Now()
	var batch []types.Request
	for i := 0; i < 40; i++ {
		r := types.Request{At: now.Add(-time.Duration(i*20) * time.Millisecond), App: "shop", Method: "GET",
			Path: fmt.Sprintf("/products/%d", i), Status: 200, DurationMS: float64(5 + i), Node: "control",
			Instance: "web.1", InstanceNode: "control", Client: "203.0.113.9"}
		if i%3 == 0 {
			r.Instance, r.InstanceNode, r.Node = "web.2", "worker-1", "worker-1"
		}
		if i%10 == 0 {
			r.Status = 502
		}
		batch = append(batch, r)
	}
	batch = append(batch, types.Request{At: now, Host: "nope.test", Method: "GET", Path: "/", Status: 404, DurationMS: 0.3, Node: "control"})
	m.updateTraffic(requestsMsg(batch))
	m.updateTraffic(frameMsg{})
	return m
}

func TestTrafficView(t *testing.T) {
	for _, size := range [][2]int{{160, 40}, {100, 30}, {60, 15}} {
		m := trafficModel(size[0], size[1])
		out := m.View()
		lines := strings.Split(out, "\n")
		if len(lines) > size[1] {
			t.Errorf("%dx%d: %d lines", size[0], size[1], len(lines))
		}
		for _, l := range lines {
			if lipgloss.Width(l) > size[0] {
				t.Errorf("%dx%d: line of %d columns", size[0], size[1], lipgloss.Width(l))
				break
			}
		}
	}
	m := trafficModel(180, 40)
	out := ansi.Strip(m.View())
	for _, want := range []string{"req/s", "p95", "errors", "shop", "(no app)", "web.1@control", "web.2@worker-1", "404 unknown host", "/products/39", "requests by node"} {
		if !strings.Contains(out, want) {
			t.Errorf("traffic view is missing %q", want)
		}
	}
	if !strings.Contains(out, "●") || !strings.Contains(out, "✖") {
		t.Error("no requests in flight are drawn")
	}
	m.trafficKey("p")
	if !strings.Contains(ansi.Strip(m.View()), "paused") {
		t.Error("pause not shown")
	}
	if os.Getenv("PRINT_TUI") != "" {
		fmt.Println(out)
	}
}
