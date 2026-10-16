package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/wes/jokku/internal/nettest"
	"github.com/wes/jokku/internal/types"
)

// The Network view: the latest test of the links between the nodes, how
// fast each one carried data and how quickly it answered pings, the nodes
// that stand out, and how each node compares with the test before. t (twice)
// runs a new test, which loads each link in turn for a few seconds.

type netTestsMsg struct {
	nt  *types.NetTests
	err error
}

type netStartMsg struct{ err error }

// ntConfirmFor is how long a first t waits for the second.
const ntConfirmFor = 5 * time.Second

func (m *model) fetchNetTests() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		nt, err := m.api.NetTests(ctx)
		return netTestsMsg{nt, err}
	}
}

func (m *model) startNetTest() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		_, err := m.api.StartNetTest(ctx, types.NetTestRequest{})
		return netStartMsg{err}
	}
}

// networkKey handles the Network view's own keys.
func (m *model) networkKey(k string) (tea.Cmd, bool) {
	if k != "t" {
		return nil, false
	}
	switch {
	case m.nt != nil && m.nt.Running != nil:
		m.flash("a test is running", true)
	case time.Since(m.ntConfirm) < ntConfirmFor:
		m.ntConfirm = time.Time{}
		m.flash("starting the test...", false)
		return m.startNetTest(), true
	default:
		m.ntConfirm = time.Now()
		m.flash(fmt.Sprintf("t again to test: each link is loaded in turn, about %s in all", m.netTestTook()), true)
	}
	return nil, true
}

// netTestTook guesses how long a test takes, from the last one.
func (m *model) netTestTook() string {
	if m.nt != nil && len(m.nt.Tests) > 0 {
		t := m.nt.Tests[0]
		return roundDur(t.Finished.Sub(t.Started))
	}
	if m.st != nil {
		n := 0
		for _, node := range m.st.Nodes {
			if node.Role != types.RoleEdge {
				n++
			}
		}
		return roundDur(time.Duration(n*(n-1)) * 4500 * time.Millisecond)
	}
	return "a minute"
}

func roundDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	}
	d = d.Round(time.Second)
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func (m *model) networkView() string {
	switch {
	case m.nt == nil && m.ntErr != nil:
		return sBad.Render("  Cannot read the network tests: " + m.ntErr.Error())
	case m.nt == nil:
		return sDim.Render("  Loading...")
	}
	var b strings.Builder
	b.WriteString(m.flashLine())
	nt := m.nt
	var test, before *types.NetTest
	switch {
	case nt.Running != nil:
		test = nt.Running.Test
		if len(nt.Tests) > 0 {
			before = &nt.Tests[0]
		}
	case len(nt.Tests) > 0:
		test = &nt.Tests[0]
		if len(nt.Tests) > 1 {
			before = &nt.Tests[1]
		}
	}
	if test == nil {
		b.WriteString("  No network test yet. Press " + sAccent.Render("t") + " to test every link between the nodes:\n")
		b.WriteString(sDim.Render("  each one carries as much as it can for a few seconds, in turn, and is pinged 100 times.") + "\n")
		return b.String()
	}
	r := nettest.Read(test)

	if p := nt.Running; p != nil {
		width := 24
		done := 0
		if p.Total > 0 {
			done = p.Done * width / p.Total
		}
		b.WriteString(fmt.Sprintf("  %s %s   %s %s  %s\n", sAccent.Render("● Testing"), sAccent.Render(p.From+" → "+p.To),
			sAccent.Render(strings.Repeat("█", done))+sDim.Render(strings.Repeat("░", width-done)),
			fmt.Sprintf("%d of %d links", p.Done, p.Total), sDim.Render(since(p.Started))))
	} else {
		b.WriteString(fmt.Sprintf("  Tested %s %s\n", ago(test.Finished),
			sDim.Render(fmt.Sprintf("in %s · %d nodes, %d links over the mesh, %gs each", roundDur(test.Finished.Sub(test.Started)),
				len(test.Nodes), len(test.Links), test.Seconds))))
	}
	if r.Mbps > 0 {
		line := fmt.Sprintf("  A link here usually carries %s and answers in %s", sAccent.Render(fmt.Sprintf("%.0f Mbit/s", r.Mbps)),
			sAccent.Render(fmt.Sprintf("%.2f ms", r.RTT)))
		var odd []string
		for _, n := range r.Nodes {
			if n.Grade != nettest.Good {
				odd = append(odd, gradeStyle(n.Grade).Render(n.Name+" "+n.Problems[0]))
			}
		}
		switch {
		case len(odd) > 0:
			line += sDim.Render("  ·  ") + strings.Join(odd, sDim.Render("  ·  "))
		case nt.Running == nil:
			line += sDim.Render("  ·  ") + sGood.Render("every node alike")
		}
		b.WriteString(line + "\n")
	}
	if len(test.Skipped) > 0 {
		var left []string
		for _, name := range sortedNames(test.Skipped) {
			left = append(left, name+" ("+test.Skipped[name]+")")
		}
		b.WriteString("  " + sWarn.Render("Left out: "+strings.Join(left, ", ")) + "\n")
	}

	links := map[string]types.NetLink{}
	for _, l := range test.Links {
		links[l.From+">"+l.To] = l
	}
	nameW := 6
	for _, n := range test.Nodes {
		nameW = max(nameW, len(n))
	}
	colW := max(nameW, 7) + 2
	matrix := func(title, note string, cell func(l types.NetLink) string) {
		b.WriteString("\n  " + sHead.Render(title) + sDim.Render("  "+note) + "\n")
		head := strings.Repeat(" ", nameW+7)
		for _, to := range test.Nodes {
			head += fmt.Sprintf("%*s", colW, to)
		}
		b.WriteString(sHead.Render(head) + "\n")
		for i, from := range test.Nodes {
			label := "      "
			if i == 0 {
				label = "from  "
			}
			line := "  " + sHead.Render(label) + pad(from, nameW-1)
			for _, to := range test.Nodes {
				l, ok := links[from+">"+to]
				switch {
				case from == to:
					line += rpad(sDim.Render("·"), colW)
				case !ok:
					line += rpad(sDim.Render("…"), colW)
				default:
					line += rpad(cell(l), colW)
				}
			}
			b.WriteString(line + "\n")
		}
	}
	matrix("THROUGHPUT", "Mbit/s, from each row's node to each column's", func(l types.NetLink) string {
		if l.Mbps == 0 {
			return sBad.Render("failed")
		}
		return gradeStyle(r.Grade(l)).Render(fmt.Sprintf("%.0f", l.Mbps))
	})
	matrix("ROUND TRIP", "ms on average over 100 pings, and how many got no answer", func(l types.NetLink) string {
		p := l.Ping
		if p.Received == 0 {
			if p.Sent == 0 {
				return sDim.Render("-")
			}
			return sBad.Render("no answer")
		}
		s := fmt.Sprintf("%.2f", p.Avg)
		if lost := p.Sent - p.Received; lost > 0 {
			s += fmt.Sprintf(" %d✗", lost)
		}
		return gradeStyle(pingGrade(p, r.RTT)).Render(s)
	})

	// Each node, and how it compares with the test before.
	var prev map[string]nettest.Node
	if before != nil {
		prev = map[string]nettest.Node{}
		for _, n := range nettest.Read(before).Nodes {
			prev[n.Name] = n
		}
	}
	head := fmt.Sprintf("  %-*s %9s %9s %11s %6s", nameW, "NODE", "SENDS", "RECEIVES", "ROUND TRIP", "LOST")
	if prev != nil {
		head += fmt.Sprintf("   %-22s", "VS. "+strings.ToUpper(ago(before.Finished)))
	}
	b.WriteString("\n" + sHead.Render(head) + "\n")
	for _, n := range r.Nodes {
		lost := fmt.Sprint(n.Lost)
		if n.Lost > 0 {
			lost = sWarn.Render(lost)
		}
		line := fmt.Sprintf("  %s %9s %9s %11s %s", pad(gradeStyle(n.Grade).Render(n.Name), nameW),
			mbpsOrDash(n.Out), mbpsOrDash(n.In), fmt.Sprintf("%.2f ms", n.RTT), rpad(lost, 6))
		if prev != nil {
			change := sDim.Render("not tested")
			if p, ok := prev[n.Name]; ok {
				change = speedChange(p.Out+p.In, n.Out+n.In) + sDim.Render(", ") + rttChange(p.RTT, n.RTT)
			}
			line += "   " + pad(change, 22)
		}
		if len(n.Problems) > 0 {
			line += "  " + gradeStyle(n.Grade).Render(strings.Join(n.Problems, "; "))
		}
		b.WriteString(line + "\n")
	}

	var failed []string
	for _, l := range test.Links {
		if l.Error != "" {
			failed = append(failed, fmt.Sprintf("%s → %s: %s", l.From, l.To, l.Error))
		}
	}
	if len(failed) > 0 {
		b.WriteString("\n" + sHead.Render("  PROBLEMS") + "\n")
		for i, f := range failed {
			if i == 5 {
				b.WriteString(sDim.Render(fmt.Sprintf("    and %d more: jokku network:report", len(failed)-5)) + "\n")
				break
			}
			b.WriteString("    " + sBad.Render(trunc(f, max(m.width-6, 40))) + "\n")
		}
	}
	return b.String()
}

func gradeStyle(g nettest.Grade) lipgloss.Style {
	switch g {
	case nettest.Bad:
		return sBad
	case nettest.Warn:
		return sWarn
	}
	return lipgloss.NewStyle()
}

// pingGrade grades a link's round trips alone, for the round trip table.
func pingGrade(p types.PingStats, usual float64) nettest.Grade {
	r := nettest.Report{RTT: usual}
	return r.Grade(types.NetLink{Mbps: 1, Ping: p})
}

func mbpsOrDash(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f", v)
}

// speedChange is how a node's throughput moved between two tests.
func speedChange(before, now float64) string {
	if before == 0 || now == 0 {
		return sDim.Render("-")
	}
	pct := (now - before) / before * 100
	s := fmt.Sprintf("%+.0f%% speed", pct)
	switch {
	case pct <= -15:
		return sBad.Render(s)
	case pct >= 15:
		return sGood.Render(s)
	case pct > -2 && pct < 2:
		return sDim.Render("same speed")
	}
	return s
}

// rttChange is how a node's round trips moved between two tests.
func rttChange(before, now float64) string {
	if before == 0 || now == 0 {
		return sDim.Render("-")
	}
	d := now - before
	s := fmt.Sprintf("%+.2f ms", d)
	switch {
	case d >= 0.5:
		return sWarn.Render(s)
	case d > -0.05 && d < 0.05:
		return sDim.Render("same ms")
	}
	return s
}

// rpad right-aligns s (which may contain styling) in width columns.
func rpad(s string, width int) string {
	if w := lipgloss.Width(s); w < width {
		return strings.Repeat(" ", width-w) + s
	}
	return s
}

func sortedNames(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
