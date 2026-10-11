// Package splash draws the screen a Jokku server greets you with when you
// log in: the logo, a striped sun, and live figures about the server and its
// cluster. It also installs and removes it as the login banner (motd.go).
package splash

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wes/jokku/internal/types"
)

// Info is everything the splash shows. Gather fills it in.
type Info struct {
	Host    string
	Role    string // control, worker or edge; empty where jokku isn't set up
	Running bool   // the jokku daemon answers
	Status  string // this node's status on the control node: ready, down or draining
	Version string
	Mesh    string // this node's address on the cluster's network

	// Cluster is set when the control node answered; the figures below are
	// the whole cluster's.
	Cluster                          bool
	Apps, Databases, Asleep, Stopped int
	Unhealthy                        []AppHealth
	Nodes, NodesUp, Edges            int
	Down                             []string // nodes that stopped reporting
	Instances                        int
	LastDeploy                       *types.Deploy

	// Here is the instances on this node.
	Here int
	// A worker's or an edge's view of the cluster, from its own files.
	Control string // the control node's address
	Peers   int    // the other nodes it knows of
	Routes  int    // apps an edge routes

	// This machine.
	IP                       string
	CPUs                     int
	Load                     []float64 // over 1, 5 and 15 minutes
	MemoryMB, MemoryUsedMB   int
	DiskMB, DiskUsedMB       int
	DiskAvailMB              int
	Uptime                   time.Duration
	Updates, SecurityUpdates int // packages waiting, from update-notifier

	Now time.Time
}

// AppHealth is an app with fewer healthy instances than it wants.
type AppHealth struct {
	Name            string
	Healthy, Wanted int
}

// Options say what the splash is drawn on.
type Options struct {
	Color bool
	// Width is the terminal's width in columns, or 0 when it isn't known,
	// as at login; the full splash takes 78.
	Width int
}

const (
	indent    = "  "
	artGap    = "   "
	fullWidth = 78 // the grid's right column lines up with the sun
	labelWide = 10
	leftWide  = logoWidth + len(artGap) - labelWide - 1 // values in the left column
	rightWide = 7                                       // labels in the right column
	green     = 114
	yellow    = 221
	red       = 203
	docs      = "https://jokku.dev"
)

// Render draws the splash.
func Render(w io.Writer, in *Info, o Options) {
	p := painter{o.Color}
	var lines []string
	lines = append(lines, art(p, o)...)
	lines = append(lines, "", indent+headline(p, in), "")
	left, right := rows(in)
	lines = append(lines, grid(p, left, right, len(left) > 0 && (o.Width == 0 || o.Width >= fullWidth))...)
	if a := alerts(p, in); len(a) > 0 {
		lines = append(lines, "")
		lines = append(lines, a...)
	}
	lines = append(lines, "", indent+hints(p, in))
	for _, l := range lines {
		fmt.Fprintln(w, strings.TrimRight(l, " "))
	}
}

// art is the logo with the sun to its right, the sun a line taller at
// each end. Narrow terminals get the logo alone, or the name.
func art(p painter, o Options) []string {
	switch {
	case o.Width > 0 && o.Width < len(indent)+logoWidth:
		return []string{"", indent + p.paint(bold+fg(orange), "JOKKU")}
	case !o.Color || (o.Width > 0 && o.Width < len(indent)+logoWidth+len(artGap)+sunWidth):
		lines := []string{""}
		for i, l := range logo {
			lines = append(lines, indent+p.paint(fg(logoShades[i]), l))
		}
		return lines
	}
	pic := quarterBlocks(sun)
	lines := make([]string, len(pic))
	for i, t := range pic {
		l := strings.Repeat(" ", logoWidth)
		if i > 0 && i <= len(logo) {
			l = p.paint(fg(logoShades[i-1]), logo[i-1])
		}
		lines[i] = indent + l + artGap + t
	}
	return lines
}

func headline(p painter, in *Info) string {
	parts := []string{p.paint(bold+fg(orange), in.Host)}
	switch in.Role {
	case "control":
		parts = append(parts, "control node")
	case "worker":
		parts = append(parts, "worker node")
	case "edge":
		parts = append(parts, "edge")
	}
	switch {
	case !in.Running:
		parts = append(parts, p.paint(fg(red), "● jokku is not running"))
	case in.Status == "down":
		parts = append(parts, p.paint(fg(red), "● down"))
	case in.Status == "draining":
		parts = append(parts, p.paint(fg(yellow), "● draining"))
	case in.Status == "ready":
		parts = append(parts, p.paint(fg(green), "● ready"))
	default:
		parts = append(parts, p.paint(fg(green), "● running"))
	}
	if in.Version != "" {
		parts = append(parts, p.paint(dim, "jokku "+in.Version))
	}
	return strings.Join(parts, "  ")
}

type cell struct {
	label, value string
	warn         bool
}

// rows are the grid's two columns: the cluster on the left, this machine
// on the right.
func rows(in *Info) (left, right []cell) {
	switch {
	case in.Cluster:
		apps := fmt.Sprint(in.Apps)
		if in.Apps == 0 {
			apps = "none yet"
		}
		if in.Asleep > 0 {
			apps += fmt.Sprintf(" · %d asleep", in.Asleep)
		}
		if in.Stopped > 0 {
			apps += fmt.Sprintf(" · %d stopped", in.Stopped)
		}
		nodes := fmt.Sprintf("%d of %d ready", in.NodesUp, in.Nodes)
		if in.Edges > 0 {
			nodes += " · " + count(in.Edges, "edge", "edges")
		}
		left = append(left, cell{label: "apps", value: apps})
		if in.Databases > 0 {
			left = append(left, cell{label: "databases", value: fmt.Sprint(in.Databases)})
		}
		left = append(left,
			cell{label: "nodes", value: nodes, warn: in.NodesUp < in.Nodes},
			cell{label: "instances", value: fmt.Sprintf("%d · %d on this node", in.Instances, in.Here)})
	case in.Role == "worker" || in.Role == "edge":
		left = append(left, cell{label: "control", value: orUnknown(in.Control)})
		if in.Role == "edge" {
			left = append(left,
				cell{label: "routes", value: count(in.Routes, "app", "apps")},
				cell{label: "nodes", value: fmt.Sprintf("%d behind it", in.Peers)})
		} else {
			left = append(left,
				cell{label: "nodes", value: fmt.Sprintf("%d in the cluster", in.Peers+1)},
				cell{label: "instances", value: fmt.Sprintf("%d on this node", in.Here)})
		}
	}
	if in.Mesh != "" {
		left = append(left, cell{label: "mesh", value: in.Mesh})
	}
	if d := in.LastDeploy; d != nil {
		v, warn := d.App+", "+ago(in.Now, d.CreatedAt), false
		switch d.Status {
		case "failed":
			v, warn = d.App+" failed, "+ago(in.Now, d.CreatedAt), true
		case "pending", "building", "deploying":
			v = d.App + ", " + d.Status + " now"
		}
		left = append(left, cell{label: "deployed", value: v, warn: warn})
	}

	if in.IP != "" {
		right = append(right, cell{label: "ip", value: in.IP})
	}
	if len(in.Load) == 3 {
		right = append(right, cell{label: "load", value: fmt.Sprintf("%.2f %.2f %.2f · %d cpus", in.Load[0], in.Load[1], in.Load[2], in.CPUs),
			warn: in.CPUs > 0 && in.Load[1] > float64(in.CPUs)})
	}
	if in.MemoryMB > 0 {
		right = append(right, cell{label: "memory", value: size(in.MemoryUsedMB) + " of " + size(in.MemoryMB),
			warn: in.MemoryUsedMB*10 >= in.MemoryMB*9})
	}
	if used := in.DiskUsedMB + in.DiskAvailMB; used > 0 {
		pct := (in.DiskUsedMB*100 + used - 1) / used // rounded up, as df does
		right = append(right, cell{label: "disk", value: fmt.Sprintf("%d%% of %s", pct, size(in.DiskMB)), warn: pct >= 90})
	}
	if in.Uptime > 0 {
		right = append(right, cell{label: "uptime", value: uptime(in.Uptime)})
	}
	return left, right
}

// grid lays the rows out side by side, the right column starting under
// the sun, or one under the other when the terminal is too narrow (or
// there's nothing to put on the left).
func grid(p painter, left, right []cell, wide bool) []string {
	if !wide {
		var lines []string
		for _, c := range append(left, right...) {
			lines = append(lines, indent+p.cell(c, labelWide, 0))
		}
		return lines
	}
	var lines []string
	for i := 0; i < max(len(left), len(right)); i++ {
		var l, r string
		if i < len(left) {
			l = p.cell(left[i], labelWide, leftWide)
		} else {
			l = strings.Repeat(" ", labelWide+1+leftWide)
		}
		if i < len(right) {
			r = p.cell(right[i], rightWide, 0)
		}
		lines = append(lines, indent+l+r)
	}
	return lines
}

// alerts are what needs a look: nodes that are down, apps short of
// healthy instances, and system updates.
func alerts(p painter, in *Info) []string {
	var out []string
	warn := func(format string, args ...any) {
		out = append(out, indent+p.paint(fg(yellow), "▲ "+fmt.Sprintf(format, args...)))
	}
	switch n := len(in.Down); {
	case n == 1:
		warn("%s is down", in.Down[0])
	case n > 1:
		warn("%d nodes are down: %s", n, list(in.Down, 4))
	}
	switch n := len(in.Unhealthy); {
	case n == 1:
		a := in.Unhealthy[0]
		warn("%s is unhealthy: %d of %d instances up", a.Name, a.Healthy, a.Wanted)
	case n > 1:
		names := make([]string, n)
		for i, a := range in.Unhealthy {
			names[i] = a.Name
		}
		warn("%d apps are unhealthy: %s", n, list(names, 4))
	}
	if in.Updates > 0 {
		security := ""
		if in.SecurityUpdates > 0 {
			security = fmt.Sprintf(" (%d security)", in.SecurityUpdates)
		}
		out = append(out, indent+p.paint(fg(yellow), "▲ "+count(in.Updates, "update", "updates")+" available"+security)+
			p.paint(dim, " · apt upgrade"))
	}
	return out
}

func hints(p painter, in *Info) string {
	cmds := "jokku help · jokku top · jokku apps:list"
	if in.Role == "worker" || in.Role == "edge" {
		cmds = "jokku commands run on the control node"
	}
	return p.paint(dim, cmds+" · docs at ") + p.paint(fg(orange), docs)
}

type painter struct{ color bool }

func (p painter) paint(style, s string) string {
	if !p.color || s == "" {
		return s
	}
	return style + s + reset
}

// cell is a label and its value, padded to the widths given (0: none).
func (p painter) cell(c cell, labelWidth, valueWidth int) string {
	v := c.value
	if valueWidth > 0 {
		v = pad(clip(v, valueWidth-1), valueWidth)
	}
	if c.warn {
		v = p.paint(fg(yellow), v)
	}
	return p.paint(dim, pad(c.label, labelWidth)) + " " + v
}

func pad(s string, n int) string {
	return s + strings.Repeat(" ", max(0, n-utf8.RuneCountInString(s)))
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func list(names []string, most int) string {
	if len(names) <= most {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:most], ", ") + fmt.Sprintf(" and %d more", len(names)-most)
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// size is megabytes the way df -h puts them.
func size(mb int) string {
	switch {
	case mb < 1024:
		return fmt.Sprintf("%dM", mb)
	case mb < 10*1024:
		return fmt.Sprintf("%.1fG", float64(mb)/1024)
	case mb < 1024*1024:
		return fmt.Sprintf("%dG", (mb+512)/1024)
	}
	return fmt.Sprintf("%.1fT", float64(mb)/(1024*1024))
}

func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return count(int(d.Minutes()), "minute", "minutes") + " ago"
	case d < 48*time.Hour:
		return count(int(d.Hours()), "hour", "hours") + " ago"
	}
	return count(int(d.Hours()/24), "day", "days") + " ago"
}

// uptime is how long the machine has been up, to the minute under a day
// and to the hour after.
func uptime(d time.Duration) string {
	days, hours, minutes := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	switch {
	case days > 0 && hours > 0:
		return count(days, "day", "days") + ", " + count(hours, "hour", "hours")
	case days > 0:
		return count(days, "day", "days")
	case hours > 0 && minutes > 0:
		return count(hours, "hour", "hours") + ", " + count(minutes, "minute", "minutes")
	case hours > 0:
		return count(hours, "hour", "hours")
	}
	return count(minutes, "minute", "minutes")
}
