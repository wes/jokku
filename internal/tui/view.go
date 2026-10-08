package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/wes/jokku/internal/types"
)

func (m *model) View() string {
	if m.width == 0 {
		return "loading..."
	}
	var b strings.Builder
	b.WriteString(m.header() + "\n")
	b.WriteString(m.tabs() + "\n\n")
	body := ""
	switch {
	case m.help:
		body = helpText
	case m.st == nil && m.err != nil:
		body = sBad.Render("Cannot reach the control node: " + m.err.Error())
	case m.st == nil:
		body = sDim.Render("Loading...")
	default:
		switch m.view {
		case overview:
			body = m.overview()
		case nodesView:
			body = m.nodes()
		case appsView:
			body = m.apps()
		case instancesView:
			body = m.instanceList()
		case eventsView:
			body = m.eventList(m.height - 6)
		case trafficView:
			body = m.trafficView()
		case logsView:
			body = m.logView()
		}
	}
	b.WriteString(body)
	// Pin the key help to the bottom row.
	lines := strings.Count(b.String(), "\n") + 1
	if pad := m.height - lines - 1; pad > 0 {
		b.WriteString(strings.Repeat("\n", pad))
	}
	b.WriteString("\n" + m.footer())
	return clip(b.String(), m.width, m.height)
}

func (m *model) header() string {
	title := sTitle.Render("jokku top")
	if m.st == nil {
		return title
	}
	t := m.st.Totals
	nodes := fmt.Sprintf("%d/%d nodes", t.NodesReady, t.Nodes)
	if t.NodesReady < t.Nodes {
		nodes = sBad.Render(nodes)
	} else {
		nodes = sGood.Render(nodes)
	}
	inst := fmt.Sprintf("%d/%d instances healthy", t.InstancesHealthy, t.Instances)
	if t.InstancesHealthy < t.Instances {
		inst = sWarn.Render(inst)
	} else {
		inst = sGood.Render(inst)
	}
	age := sDim.Render("updated " + ago(m.at))
	if m.err != nil {
		age = sBad.Render("stale: " + m.err.Error())
	}
	parts := []string{title, sDim.Render("control " + m.st.Control), sDim.Render(m.st.Version), nodes,
		fmt.Sprintf("%d apps", t.Apps), inst, age}
	return strings.Join(parts, sDim.Render("  ·  "))
}

func (m *model) tabs() string {
	var out []string
	for i, name := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, name)
		if view(i) == m.view || (m.view == logsView && view(i) == m.back) {
			out = append(out, sTabOn.Render(label))
		} else {
			out = append(out, sTab.Render(label))
		}
	}
	if m.view == logsView {
		out = append(out, sTabOn.Render("logs: "+m.logApp))
	}
	return strings.Join(out, "")
}

func (m *model) footer() string {
	keys := "1-6/tab views  ↑↓ move  enter drill in  l logs  esc back  r refresh  ? help  q quit"
	if m.view == trafficView {
		keys = "1-6/tab views  p pause  c clear  ? help  q quit"
	}
	if m.filterApp != "" || m.filterNode != "" {
		keys = "filtered: " + m.filterApp + m.filterNode + " (esc clears)  ·  " + keys
	}
	return sDim.Render(keys)
}

const helpText = `jokku top shows the cluster live, refreshed every two seconds.

  1 Overview    nodes, apps and recent events at a glance
  2 Nodes       each machine: status, CPU, memory, memory promised to VMs
  3 Apps        each app: healthy instances, release, last deploy, domains
  4 Instances   each microVM: state, node, CPU, memory, uptime, restarts
  5 Events      deploys, crashes, nodes coming and going
  6 Traffic     every request live: dots fly across each app's lane, colored
                by status, and land on the instance and node that answered

  enter on a node or app shows its instances; l shows an app's logs;
  esc goes back. Press any key to close this help.`

// Overview

func (m *model) overview() string {
	var b strings.Builder
	b.WriteString(sSection.Render("Nodes") + "\n")
	for _, n := range m.st.Nodes {
		fmt.Fprintf(&b, "  %s %-16s cpu %s  mem %s  vms %s  %s\n",
			nodeDot(n), trunc(n.Name, 16),
			bar(n.Metrics.CPUPercent, 10), bar(pct(n.Metrics.MemoryUsedMB, n.MemoryMB), 10),
			bar(pct(n.AllocatedMB, n.MemoryMB), 10), plural(n.Instances, "instance"))
	}
	b.WriteString(sSection.Render("Apps") + "\n")
	if len(m.st.Apps) == 0 {
		b.WriteString(sDim.Render("  no apps yet: git push jokku main") + "\n")
	}
	for i, a := range m.st.Apps {
		line := fmt.Sprintf("  %s %-20s %s v%-4d %s", appDot(a), trunc(a.Name, 20), pad(dots(a.Healthy, a.Wanted), 12), a.Release, deploySummary(a.Deploy))
		b.WriteString(m.selectable(overview, i, line) + "\n")
	}
	b.WriteString(sSection.Render("Recent events") + "\n")
	b.WriteString(m.eventList(8))
	return b.String()
}

// Nodes

func (m *model) nodes() string {
	var b strings.Builder
	b.WriteString(sHead.Render(fmt.Sprintf("  %-18s %-9s %-15s %-16s %-21s %-21s %4s  %-10s %s",
		"NAME", "STATUS", "ADDRESS", "CPU", "MEMORY USED", "PROMISED TO VMS", "INST", "VERSION", "SEEN")) + "\n")
	for i, n := range m.st.Nodes {
		status := n.Status
		if n.CanRun != "" && status == "ready" {
			status = "no-vms"
		}
		line := fmt.Sprintf("  %s %-16s %-9s %-15s %s %4.0f%%  %s %-8s %s %-8s %4d  %-10s %s",
			nodeDot(n), trunc(n.Name, 16), status, trunc(n.Address, 15),
			bar(n.Metrics.CPUPercent, 10), n.Metrics.CPUPercent,
			bar(pct(n.Metrics.MemoryUsedMB, n.MemoryMB), 10), mem(n.Metrics.MemoryUsedMB)+"/"+mem(n.MemoryMB),
			bar(pct(n.AllocatedMB, n.MemoryMB), 10), mem(n.AllocatedMB),
			n.Instances, trunc(n.Version, 10), ago(n.LastSeen))
		b.WriteString(m.selectable(nodesView, i, line) + "\n")
	}
	if n, ok := m.selectedNode(); ok {
		b.WriteString("\n" + sDim.Render(fmt.Sprintf("  %s · %s · mesh %s · %d CPUs · disk %s free of %s · load %.2f",
			n.Name, n.Role, n.MeshIP, n.CPUs, mem(n.Metrics.DiskFreeMB), mem(n.Metrics.DiskMB), n.Metrics.Load1)) + "\n")
		if n.CanRun != "" {
			b.WriteString("  " + sBad.Render("cannot run microVMs: "+n.CanRun) + "\n")
		}
	}
	return b.String()
}

// Apps

func (m *model) apps() string {
	var b strings.Builder
	b.WriteString(sHead.Render(fmt.Sprintf("  %-22s %-10s %-14s %-6s %-26s %s", "APP", "STATUS", "INSTANCES", "REL", "LAST DEPLOY", "DOMAINS")) + "\n")
	if len(m.st.Apps) == 0 {
		b.WriteString(sDim.Render("  no apps yet: git push jokku main") + "\n")
	}
	for i, a := range m.st.Apps {
		domains := strings.Join(a.Domains, " ")
		line := fmt.Sprintf("  %s %-20s %-10s %s %-6s %-26s %s", appDot(a), trunc(a.Name, 20), appStatus(a),
			pad(dots(a.Healthy, a.Wanted), 14), fmt.Sprintf("v%d", a.Release), trunc(deploySummary(a.Deploy), 26), domains)
		b.WriteString(m.selectable(appsView, i, line) + "\n")
	}
	return b.String()
}

// Instances

func (m *model) instanceList() string {
	var b strings.Builder
	b.WriteString(sHead.Render(fmt.Sprintf("  %-18s %-10s %-14s %-9s %-17s %-15s %-20s %-8s %s",
		"APP", "PROCESS", "NODE", "STATE", "CPU", "MEMORY", "ADDRESS", "UPTIME", "RESTARTS")) + "\n")
	list := m.instances()
	if len(list) == 0 {
		b.WriteString(sDim.Render("  no instances") + "\n")
	}
	start, end := window(m.cursor[instancesView], len(list), m.height-8)
	for i := start; i < end; i++ {
		in := list[i]
		state := in.State
		if in.Desired == "stopped" {
			state = "retiring"
		}
		uptime := "-"
		if !in.StartedAt.IsZero() && in.State == "healthy" {
			uptime = since(in.StartedAt)
		}
		addr := "-"
		if in.IP != "" {
			addr = fmt.Sprintf("%s:%d", in.IP, in.Port)
		}
		line := fmt.Sprintf("  %-18s %-10s %-14s %s %s %4.0f%% %-15s %-20s %-8s %d",
			trunc(in.App, 18), trunc(in.Name, 10), trunc(in.Node, 14), stateText(state, 9),
			bar(in.CPUPercent/float64(max(in.CPUs, 1)), 10), in.CPUPercent,
			mem(in.MemoryUsed)+" of "+mem(in.MemoryMB), addr, uptime, in.Restarts)
		b.WriteString(m.selectable(instancesView, i, line) + "\n")
	}
	return b.String()
}

// Events

func (m *model) eventList(limit int) string {
	var b strings.Builder
	events := m.st.Events
	if len(events) == 0 {
		return sDim.Render("  nothing has happened yet") + "\n"
	}
	selectable := m.view == eventsView
	start, end := 0, min(len(events), max(limit, 1))
	if selectable {
		start, end = window(m.cursor[eventsView], len(events), limit)
	}
	for i := start; i < end; i++ {
		e := events[i]
		who := e.App
		if who == "" {
			who = e.Node
		}
		msg := e.Message
		switch {
		case strings.Contains(msg, "failed") || strings.Contains(msg, "crashed") || strings.Contains(msg, "stopped reporting") || strings.Contains(msg, "cannot"):
			msg = sBad.Render(msg)
		case strings.HasPrefix(msg, "deployed") || strings.Contains(msg, "recovered") || strings.Contains(msg, "is back") || strings.Contains(msg, "joined"):
			msg = sGood.Render(msg)
		case strings.HasPrefix(msg, "moving") || strings.HasPrefix(msg, "draining"):
			msg = sWarn.Render(msg)
		}
		line := fmt.Sprintf("  %s  %-8s %-16s %s", sDim.Render(e.At.Local().Format("Jan 02 15:04:05")), e.Kind, trunc(who, 16), msg)
		if selectable {
			line = m.selectable(eventsView, i, line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// Logs

func (m *model) logView() string {
	if m.logErr != nil {
		return sBad.Render("  " + m.logErr.Error())
	}
	if m.logs == nil {
		return sDim.Render("  loading logs...")
	}
	if len(m.logs) == 0 {
		return sDim.Render("  no output yet")
	}
	// Follow the end unless the user scrolled up.
	h := max(m.height-6, 1)
	if m.cursor[logsView] == 0 || m.cursor[logsView] >= len(m.logs)-2 {
		m.cursor[logsView] = len(m.logs) - 1
	}
	start, end := window(m.cursor[logsView], len(m.logs), h)
	var b strings.Builder
	for _, l := range m.logs[start:end] {
		b.WriteString("  " + l + "\n")
	}
	return b.String()
}

// Pieces

func (m *model) selectable(v view, i int, line string) string {
	if m.view == v && m.cursor[v] == i {
		return sSelected.Render(pad(line, m.width))
	}
	return line
}

// window returns the range of rows to show so the cursor stays visible.
func window(cursor, n, height int) (int, int) {
	if height <= 0 || n <= height {
		return 0, n
	}
	start := cursor - height/2
	start = max(0, min(start, n-height))
	return start, start + height
}

func bar(percent float64, width int) string {
	percent = max(0, min(percent, 100))
	full := int(percent/100*float64(width) + 0.5)
	style := sGood
	switch {
	case percent >= 85:
		style = sBad
	case percent >= 60:
		style = sWarn
	}
	return style.Render(strings.Repeat("█", full)) + sDim.Render(strings.Repeat("░", width-full))
}

func dots(healthy, wanted int) string {
	if wanted == 0 {
		return sDim.Render("none")
	}
	if wanted > 10 {
		return fmt.Sprintf("%s %d/%d", stateDot(healthy == wanted), healthy, wanted)
	}
	return sGood.Render(strings.Repeat("●", min(healthy, wanted))) + sBad.Render(strings.Repeat("○", max(wanted-healthy, 0)))
}

func nodeDot(n types.NodeView) string {
	switch {
	case n.Status == "down":
		return sBad.Render("●")
	case n.Status == "draining" || n.CanRun != "":
		return sWarn.Render("●")
	}
	return sGood.Render("●")
}

func appDot(a types.AppView) string {
	switch appStatus(a) {
	case "running":
		return sGood.Render("●")
	case "degraded":
		return sWarn.Render("●")
	case "down":
		return sBad.Render("●")
	}
	return sDim.Render("●")
}

func appStatus(a types.AppView) string {
	switch {
	case a.Release == 0:
		return "new"
	case a.Stopped:
		return "stopped"
	case a.Wanted == 0:
		return "scaled 0"
	case a.Healthy == 0:
		return "down"
	case a.Healthy < a.Wanted:
		return "degraded"
	}
	return "running"
}

func stateDot(ok bool) string {
	if ok {
		return sGood.Render("●")
	}
	return sWarn.Render("●")
}

func stateText(state string, width int) string {
	s := pad(state, width)
	switch state {
	case "healthy":
		return sGood.Render(s)
	case "failed", "crashed", "unknown":
		return sBad.Render(s)
	case "retiring", "stopped":
		return sDim.Render(s)
	}
	return sWarn.Render(s)
}

func deploySummary(d *types.Deploy) string {
	if d == nil {
		return "never"
	}
	when := ago(d.CreatedAt)
	switch d.Status {
	case types.StatusSucceeded:
		return sGood.Render("ok") + " " + when
	case types.StatusFailed:
		return sBad.Render("failed") + " " + when
	}
	return sWarn.Render(d.Status) + " " + when
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func pct(used, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

func mem(mb int) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1fG", float64(mb)/1024)
	}
	return fmt.Sprintf("%dM", mb)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return since(t) + " ago"
}

func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// pad pads s (which may contain styling) to width visible columns.
func pad(s string, width int) string {
	if w := lipgloss.Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// clip keeps the frame inside the terminal so it never scrolls.
func clip(s string, width, height int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		lines = append(lines[:height-1], lines[len(lines)-1])
	}
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			lines[i] = lipgloss.NewStyle().MaxWidth(width).Render(l)
		}
	}
	return strings.Join(lines, "\n")
}
