package tui

import (
	"fmt"
	"slices"
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
		case backupsView:
			body = m.backupsView()
		case databasesView:
			body = m.databasesView()
		case firewallView:
			body = m.firewallView()
		case networkView:
			body = m.networkView()
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
	if t.InstancesAsleep > 0 {
		inst += sDim.Render(fmt.Sprintf(" · %d asleep", t.InstancesAsleep))
	}
	if t.InstancesStopped > 0 {
		inst += sDim.Render(fmt.Sprintf(" · %d stopped", t.InstancesStopped))
	}
	age := sDim.Render("updated " + ago(m.at))
	if m.err != nil {
		age = sBad.Render("stale: " + m.err.Error())
	}
	apps, ext, dbs := 0, 0, 0
	for _, a := range m.st.Apps {
		switch {
		case isDatabase(a):
			dbs++
		case isExternal(a):
			ext++
		default:
			apps++
		}
	}
	parts := []string{title, sDim.Render("control " + m.st.Control), sDim.Render(m.st.Version), nodes, plural(apps, "app")}
	if ext > 0 {
		parts = append(parts, fmt.Sprintf("%d external", ext))
	}
	if dbs > 0 {
		parts = append(parts, plural(dbs, "database"))
	}
	parts = append(parts, inst, age)
	return strings.Join(parts, sDim.Render("  ·  "))
}

// tabs is the row of views. On a terminal too narrow for every name, the
// tabs lose their padding, then every name but the current view's.
func (m *model) tabs() string {
	for compact := 0; ; compact++ {
		var out []string
		for i, name := range tabNames {
			label := fmt.Sprintf("%d %s", (i+1)%10, name)
			on := view(i) == m.view || (m.view == logsView && view(i) == m.back)
			if compact == 2 && !on {
				label = fmt.Sprint((i + 1) % 10)
			}
			style := sTab
			if on {
				style = sTabOn
			}
			if compact > 0 && !on {
				style = style.Padding(0, 0)
			}
			out = append(out, style.Render(label))
		}
		if m.view == logsView {
			out = append(out, sTabOn.Render("logs: "+m.logApp))
		}
		sep := ""
		if compact > 0 {
			sep = " "
		}
		row := strings.Join(out, sep)
		if compact == 2 || m.width == 0 || lipgloss.Width(row) <= m.width {
			return row
		}
	}
}

func (m *model) footer() string {
	keys := "0-9/tab views  ↑↓ move  enter drill in  l logs  esc back  r refresh  ? help  q quit"
	switch m.view {
	case databasesView:
		keys = "w minute/hour/day  ↑↓ move  enter instances  l logs  0-9 views  ? help  q quit"
	case firewallView:
		keys = "w minute/hour/day  ↑↓ move  u unban  0-9 views  ? help  q quit"
	case trafficView:
		keys = "0-9/tab views  p pause  c clear  f effects  ? help  q quit"
	case networkView:
		keys = "t test the network  0-9/tab views  ? help  q quit"
	case backupsView:
		keys = "space on/off  b back up now  s schedule  a auto-restore  ↑↓ move  0-9 views  ? help  q quit"
		if m.width < 100 {
			keys = "space on/off  b back up  s schedule  a auto-restore  ? help  q quit"
		}
	}
	if m.filterApp != "" || m.filterNode != "" {
		keys = "filtered: " + m.filterApp + m.filterNode + " (esc clears)  ·  " + keys
	}
	return sDim.Render(keys)
}

const helpText = `jokku top shows the cluster live, refreshed every two seconds.

  1 Overview    nodes, apps, databases and recent events at a glance
  2 Nodes       each machine: status, CPU, memory, memory promised to VMs
  3 Apps        each app: healthy instances, release, last deploy, domains,
                and under it the databases it uses; then every database
  4 Instances   each microVM: state, node, CPU, memory, uptime, restarts
  5 Events      deploys, crashes, nodes coming and going
  6 Traffic     every request live: dots fly across each app's lane, colored
                by status, and land on the instance and node that answered.
                Slow requests crawl, trailing amber; failures burst red. p
                pauses, c clears, f turns the effects off for a slow link
  7 Backups     what is backed up and what isn't: every volume and the cluster
                itself, their last and next backups, and what they keep. space
                turns backups on or off, b backs up now, s changes how often,
                a turns automatic restore on or off
  8 Databases   inside each database, a look every ten seconds: connections
                against its limit, queries a second, how long they take
                (p50, p95, p99; Postgres's average) and how often reads hit
                memory. w switches between the last minute, hour and day.
                Under it, who's connected, its users, its slowest queries
  9 Firewall    the addresses banned for asking for too many pages that
                don't exist (scanners), why and until when, those close to
                a ban or slowed for too many requests a second, and what was
                blocked, site by site. w switches between the last minute,
                hour and day; u lifts the selected ban
  0 Network     how fast the links between the nodes carry data and answer
                pings, the nodes that stand out, and how each compares with
                the test before (a new switch, say). t, twice, tests every
                link again: each is loaded in turn for a few seconds

  enter on a node or app shows its instances; l shows an app's logs;
  esc goes back. Press any key to close this help.`

// Overview

func (m *model) overview() string {
	var b strings.Builder
	b.WriteString(sSection.Render("Nodes") + "\n")
	for _, n := range m.st.Nodes {
		cpu, memory, vms := bar(n.Metrics.CPUPercent, 10), bar(pct(n.Metrics.MemoryUsedMB, n.MemoryMB), 10), bar(pct(n.AllocatedMB, n.MemoryMB), 10)
		if m.width < 104 { // the disk has no room
			fmt.Fprintf(&b, "  %s %-16s cpu %s  mem %s  vms %s  %s\n",
				nodeDot(n), trunc(n.Name, 16), cpu, memory, vms, plural(n.Instances, "instance"))
			continue
		}
		disk := bar(pct(n.Metrics.DiskMB-n.Metrics.DiskFreeMB, n.Metrics.DiskMB), 10)
		fmt.Fprintf(&b, "  %s %-16s   cpu %s   mem %s   disk %s   vms %s   %s\n",
			nodeDot(n), trunc(n.Name, 16), cpu, memory, disk, vms, plural(n.Instances, "instance"))
	}
	b.WriteString(sSection.Render("Apps") + "\n")
	list := m.appList()
	if len(list) == 0 || !isApp(list[0]) {
		b.WriteString(sDim.Render("  no apps yet: git push jokku main") + "\n")
	}
	for i, a := range list {
		line := fmt.Sprintf("  %s %-20s %s v%-4d %s", appDot(a), trunc(a.Name, 20), pad(appDots(a), 12), a.Release, deploySummary(a.Deploy))
		if isExternal(a) {
			if i == 0 || !isExternal(list[i-1]) {
				b.WriteString(sSection.Render("External") + "\n")
			}
			line = fmt.Sprintf("  %s %-20s %s %s", appDot(a), trunc(a.Name, 20), pad(a.Target, 26), sDim.Render("via "+a.Via))
		}
		if isDatabase(a) {
			if i == 0 || !isDatabase(list[i-1]) {
				b.WriteString(sSection.Render("Databases") + "\n")
			}
			used := sDim.Render("not linked")
			if len(a.Links) > 0 {
				used = sDim.Render("used by ") + linkedApps(a)
			}
			line = fmt.Sprintf("  %s %-20s %s %-9s %s", appDot(a), trunc(dbName(a), 20), pad(appDots(a), 12), a.Kind, used)
		}
		b.WriteString(m.selectable(overview, i, line) + "\n")
	}
	if line := m.backupsLine(); line != "" {
		b.WriteString(sSection.Render("Backups") + "\n" + line)
	}
	b.WriteString(sSection.Render("Recent events") + "\n")
	b.WriteString(m.eventList(8))
	return b.String()
}

// Nodes

// nodes is a table of the nodes, its columns three spaces apart. When the
// terminal is too narrow for that, the addresses go (the selected node's
// is on the line under the table), then the columns close up to two.
func (m *model) nodes() string {
	var b strings.Builder
	head, lines, width := m.nodeTable(true, 3)
	if width > m.width {
		head, lines, width = m.nodeTable(false, 3)
	}
	if width > m.width {
		head, lines, _ = m.nodeTable(false, 2)
	}
	b.WriteString(sHead.Render(head) + "\n")
	for i, line := range lines {
		b.WriteString(m.selectable(nodesView, i, line) + "\n")
	}
	if n, ok := m.selectedNode(); ok {
		b.WriteString("\n" + sDim.Render(fmt.Sprintf("  %s · %s · %s · mesh %s · %d CPUs · disk %s free of %s · load %.2f",
			n.Name, n.Role, n.Address, n.MeshIP, n.CPUs, mem(n.Metrics.DiskFreeMB), mem(n.Metrics.DiskMB), n.Metrics.Load1)) + "\n")
		if n.CanRun != "" {
			b.WriteString("  " + sBad.Render("cannot run microVMs: "+n.CanRun) + "\n")
		}
	}
	return b.String()
}

func (m *model) nodeTable(addresses bool, gap int) (string, []string, int) {
	cols := []column{{head: "NAME"}, {head: "STATUS"}, {head: "ADDRESS"}, {head: "CPU"}, {head: "MEMORY USED"},
		{head: "DISK USED"}, {head: "PROMISED TO VMS"}, {head: "INST", right: true}, {head: "VERSION"}, {head: "SEEN"}}
	var rows [][]string
	for _, n := range m.st.Nodes {
		status := n.Status
		if n.CanRun != "" && status == "ready" {
			status = "no-vms"
		}
		disk := bar(0, 10) + " -"
		if n.Metrics.DiskMB > 0 {
			used := n.Metrics.DiskMB - n.Metrics.DiskFreeMB
			disk = bar(pct(used, n.Metrics.DiskMB), 10) + " " + size(used) + "/" + size(n.Metrics.DiskMB)
		}
		rows = append(rows, []string{
			nodeDot(n) + " " + trunc(n.Name, 16), status, trunc(n.Address, 15),
			bar(n.Metrics.CPUPercent, 10) + fmt.Sprintf(" %3.0f%%", n.Metrics.CPUPercent),
			bar(pct(n.Metrics.MemoryUsedMB, n.MemoryMB), 10) + " " + size(n.Metrics.MemoryUsedMB) + "/" + size(n.MemoryMB),
			disk,
			bar(pct(n.AllocatedMB, n.MemoryMB), 10) + " " + size(n.AllocatedMB),
			fmt.Sprint(n.Instances), trunc(n.Version, 10), ago(n.LastSeen),
		})
	}
	if !addresses {
		cols = slices.Delete(cols, 2, 3)
		for i := range rows {
			rows[i] = slices.Delete(rows[i], 2, 3)
		}
	}
	return table(cols, rows, gap)
}

// Apps

// apps lists the apps, each with the databases it uses under it, then the
// databases, each with the apps that use it.
func (m *model) apps() string {
	var b strings.Builder
	b.WriteString(sHead.Render(fmt.Sprintf("  %-22s %-10s %-14s %-6s %-26s %s", "APP", "STATUS", "INSTANCES", "REL", "LAST DEPLOY", "DOMAINS")) + "\n")
	list := m.appList()
	if len(list) == 0 || !isApp(list[0]) {
		b.WriteString(sDim.Render("  no apps yet: git push jokku main") + "\n")
	}
	uses := databasesOf(list)
	for i, a := range list {
		if isExternal(a) {
			if i == 0 || !isExternal(list[i-1]) {
				b.WriteString("\n" + sHead.Render(fmt.Sprintf("  %-22s %-40s %-18s %s", "EXTERNAL", "TARGET", "VIA", "DOMAINS")) + "\n")
			}
			line := fmt.Sprintf("  %s %-20s %s %s %s", appDot(a), trunc(a.Name, 20), pad(trunc(a.Target, 40), 40), pad(trunc(a.Via, 18), 18),
				strings.Join(a.Domains, " "))
			b.WriteString(m.selectable(appsView, i, line) + "\n")
			continue
		}
		if isDatabase(a) {
			if i == 0 || !isDatabase(list[i-1]) {
				b.WriteString("\n" + sHead.Render(fmt.Sprintf("  %-22s %-10s %-14s %-9s %-23s %s", "DATABASE", "STATUS", "INSTANCES", "ENGINE", "LAST DEPLOY", "USED BY")) + "\n")
			}
			used := sDim.Render("not linked")
			if len(a.Links) > 0 {
				used = linkedApps(a)
			}
			line := fmt.Sprintf("  %s %-20s %-10s %s %-9s %s %s", appDot(a), trunc(dbName(a), 20), appStatus(a),
				pad(appDots(a), 14), a.Kind, pad(deploySummary(a.Deploy), 23), used)
			b.WriteString(m.selectable(appsView, i, line) + "\n")
			continue
		}
		line := fmt.Sprintf("  %s %-20s %-10s %s %-6s %s %s", appDot(a), trunc(a.Name, 20), appStatus(a),
			pad(appDots(a), 14), fmt.Sprintf("v%d", a.Release), pad(deploySummary(a.Deploy), 26), strings.Join(a.Domains, " "))
		b.WriteString(m.selectable(appsView, i, line) + "\n")
		for _, u := range uses[a.Name] {
			fmt.Fprintf(&b, "    %s %-18s %-10s %s %s %s\n", sDim.Render("└"), trunc(dbName(u.db), 18), appStatus(u.db),
				pad(appDots(u.db), 14), pad(sDim.Render(u.db.Kind), 33), sDim.Render(u.varName))
		}
	}
	if a, ok := m.selectedApp(); ok && isDatabase(a) {
		b.WriteString("\n" + sDim.Render(fmt.Sprintf("  %s, a %s database · jokku db:%s:info %s", a.Name, a.Kind, a.Kind, dbName(a))) + "\n")
	}
	return b.String()
}

// linkedApps is the apps a database is linked to.
func linkedApps(a types.AppView) string {
	var names []string
	for _, l := range a.Links {
		names = append(names, l.App)
	}
	return strings.Join(names, ", ")
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
		return highlight(pad(line, m.width))
	}
	return line
}

// highlight puts the selected row's background behind all of a line. Each
// styled part of it ends in a reset, which would end the background too,
// so it starts again after each one.
func highlight(line string) string {
	on, _, _ := strings.Cut(sSelected.Render(" "), " ")
	if on == "" {
		return line // no colors
	}
	line = strings.ReplaceAll(line, "\x1b[0m", "\x1b[0m"+on)
	line = strings.ReplaceAll(line, "\x1b[m", "\x1b[m"+on)
	return on + line + "\x1b[0m"
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

// appDots is an app's instances as dots. A sleeping or stopped app's are
// dim, rather than red as if they were down.
func appDots(a types.AppView) string {
	if (a.Asleep || a.Stopped) && a.Healthy == 0 && a.Wanted > 0 {
		if a.Wanted > 10 {
			return sDim.Render(fmt.Sprintf("○ 0/%d", a.Wanted))
		}
		return sDim.Render(strings.Repeat("○", a.Wanted))
	}
	return dots(a.Healthy, a.Wanted)
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
	if isExternal(a) {
		return sAccent.Render("→")
	}
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
	case isExternal(a):
		return "external"
	case a.Release == 0:
		return "new"
	case a.Stopped:
		return "stopped"
	case a.Asleep:
		return "asleep"
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

// size is mb in a few characters: megabytes under a gigabyte, a tenth of a
// gigabyte under ten, whole gigabytes, then tenths of a terabyte.
func size(mb int) string {
	switch {
	case mb < 1024:
		return fmt.Sprintf("%dM", mb)
	case mb < 10*1024:
		return fmt.Sprintf("%.1fG", float64(mb)/1024)
	case mb < 1024*1024:
		return fmt.Sprintf("%.0fG", float64(mb)/1024)
	}
	return fmt.Sprintf("%.1fT", float64(mb)/(1024*1024))
}

// column is a table's column: its heading, and whether its cells line up
// on the right.
type column struct {
	head  string
	right bool
}

// table lays rows of cells out under the headings, each column as wide as
// its widest cell and gap spaces from the next, indented two. It returns
// the heading line, a line per row, and how wide the table is.
func table(cols []column, rows [][]string, gap int) (string, []string, int) {
	widths := make([]int, len(cols))
	heads := make([]string, len(cols))
	for i, c := range cols {
		widths[i], heads[i] = lipgloss.Width(c.head), c.head
	}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], lipgloss.Width(c))
		}
	}
	line := func(cells []string) string {
		var b strings.Builder
		b.WriteString("  ")
		for i, c := range cells {
			if i > 0 {
				b.WriteString(strings.Repeat(" ", gap))
			}
			fill := strings.Repeat(" ", widths[i]-lipgloss.Width(c))
			switch {
			case cols[i].right:
				b.WriteString(fill + c)
			case i < len(cells)-1:
				b.WriteString(c + fill)
			default:
				b.WriteString(c)
			}
		}
		return b.String()
	}
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = line(r)
	}
	width := 2 + gap*(len(cols)-1)
	for _, w := range widths {
		width += w
	}
	return line(heads), lines, width
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
