package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wes/jokku/internal/types"
)

// The Firewall view: the addresses the proxies banned and why, those close
// to a ban or being slowed, and what the firewall blocked, site by site,
// over the last minute, hour or day. u lifts the selected ban.

type firewallMsg struct {
	fw  *types.Firewall
	err error
}

type unbanMsg struct {
	addr string
	had  bool
	err  error
}

func (m *model) fetchFirewall() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		fw, err := m.api.Firewall(ctx)
		return firewallMsg{fw, err}
	}
}

func (m *model) unban(addr string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		had, err := m.api.Unban(ctx, addr)
		return unbanMsg{addr, had, err}
	}
}

// firewallKey handles the Firewall view's own keys.
func (m *model) firewallKey(k string) (tea.Cmd, bool) {
	switch k {
	case "w":
		m.fwWin = (m.fwWin + 1) % len(dbWindows)
		return nil, true
	case "u":
		if b, ok := m.selectedBan(); ok {
			m.flash("unbanning "+b.Addr+"...", false)
			return m.unban(b.Addr), true
		}
		return nil, true
	}
	return nil, false
}

func (m *model) selectedBan() (types.FirewallBan, bool) {
	c := m.cursor[firewallView]
	if m.fw == nil || c >= len(m.fw.Bans) {
		return types.FirewallBan{}, false
	}
	return m.fw.Bans[c], true
}

// fwSpan is a span's counts: the last minute, hour or day (fwWin).
func (m *model) fwSpan(minute, hour, day types.FirewallCounts) types.FirewallCounts {
	return [...]types.FirewallCounts{minute, hour, day}[m.fwWin]
}

func (m *model) firewallView() string {
	var b strings.Builder
	switch {
	case m.fw == nil && m.fwErr != nil:
		return sBad.Render("  Cannot read the firewall: " + m.fwErr.Error())
	case m.fw == nil:
		return sDim.Render("  Loading...")
	}
	fw, s := m.fw, m.fw.Settings
	b.WriteString(m.flashLine())
	if !s.Enabled {
		b.WriteString("  " + sWarn.Render("The firewall is off.") + sDim.Render(" Turn it on with: jokku firewall:enable --global") + "\n")
		return b.String()
	}
	rules := fmt.Sprintf("%d pages not found within %s ban an address for %s", s.NotFound, s.Window, s.Ban)
	if s.Rate > 0 {
		rules += fmt.Sprintf("  ·  %d requests a second, bursts of %d", s.Rate, s.Burst)
	}
	if len(s.OffForApps) > 0 {
		rules += "  ·  off for " + strings.Join(s.OffForApps, ", ")
	}
	b.WriteString("  " + sGood.Render("● on") + sDim.Render("  "+rules) + "\n")
	if len(fw.Missing) > 0 {
		b.WriteString("  " + sWarn.Render(strings.Join(fw.Missing, ", ")+" run a release without the firewall: jokku update") + "\n")
	}

	c := m.fwSpan(fw.Minute, fw.Hour, fw.Day)
	total := c.Requests + c.Blocked + c.Limited
	blocked := fmt.Sprint(c.Blocked)
	if total > 0 && c.Blocked > 0 {
		blocked += sDim.Render(fmt.Sprintf(" %.0f%%", float64(c.Blocked)/float64(total)*100))
	}
	b.WriteString(fmt.Sprintf("\n  %s %-8d %s %s   %s %-6d %s %-5d %s %-6d %s\n",
		sHead.Render("REQUESTS"), total, sHead.Render("BLOCKED"), pad(blocked, 12), sHead.Render("SLOWED"), c.Limited,
		sHead.Render("BANS"), c.Bans, sHead.Render("NOT FOUND"), c.NotFound, sAccent.Render(dbWindows[m.fwWin])))

	// The bans, newest first.
	b.WriteString("\n" + sHead.Render(fmt.Sprintf("  %-26s %-14s %-9s %-5s %8s  %-14s %s",
		fmt.Sprintf("BANNED (%d)", len(fw.Bans)), "SITE", "SINCE", "LEFT", "BLOCKED", "ON", "PAGES NOT FOUND")) + "\n")
	if len(fw.Bans) == 0 {
		b.WriteString(sDim.Render("  No address is banned.") + "\n")
	}
	paths := max(m.width-86, 16)
	for i, ban := range fw.Bans {
		line := fmt.Sprintf("  %s %-24s %-14s %-9s %-5s %8d  %-14s %s", sBad.Render("●"), trunc(ban.Addr, 24), trunc(orDash(ban.App), 14),
			since(ban.At), left(ban.Until), ban.Blocked, trunc(strings.Join(ban.Nodes, ","), 14),
			sDim.Render(trunc(strings.Join(ban.Paths, " "), paths)))
		b.WriteString(m.selectable(firewallView, i, line) + "\n")
	}

	if len(fw.Watching) > 0 {
		b.WriteString("\n" + sHead.Render("  CLOSE TO A BAN") + "\n")
		for _, w := range fw.Watching {
			b.WriteString(fmt.Sprintf("    %-24s %-14s %s %s\n", trunc(w.Addr, 24), trunc(orDash(w.App), 14),
				pad(fmt.Sprintf("%d of %d not found", w.NotFound, s.NotFound), 20), sDim.Render(ago(w.Last))))
		}
	}
	if len(fw.Slowed) > 0 {
		b.WriteString("\n" + sHead.Render("  SLOWED IN THE LAST HOUR") + "\n")
		for _, l := range fw.Slowed {
			b.WriteString(fmt.Sprintf("    %-24s %-14s %s %s\n", trunc(l.Addr, 24), trunc(orDash(l.App), 14),
				pad(fmt.Sprintf("%d requests", l.Limited), 20), sDim.Render(ago(l.Last))))
		}
	}

	if len(fw.Sites) > 0 {
		b.WriteString("\n" + sHead.Render(fmt.Sprintf("  %-26s %9s %14s %8s %10s %6s", "SITE", "REQUESTS", "BLOCKED", "SLOWED", "NOT FOUND", "BANS")) + "\n")
		for _, site := range fw.Sites {
			sc := m.fwSpan(site.Minute, site.Hour, site.Day)
			total := sc.Requests + sc.Blocked + sc.Limited
			if total == 0 {
				continue
			}
			name := site.App
			if name == "" {
				name = sDim.Render("(no app's host)")
			}
			blocked := fmt.Sprintf("%d", sc.Blocked)
			if sc.Blocked > 0 {
				blocked += fmt.Sprintf(" %3.0f%%", float64(sc.Blocked)/float64(total)*100)
			}
			b.WriteString(fmt.Sprintf("    %s %9d %14s %8d %10d %6d\n", pad(trunc(name, 24), 24), total, blocked, sc.Limited, sc.NotFound, sc.Bans))
		}
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// left is how long until t.
func left(t time.Time) string {
	if !time.Now().Before(t) {
		return "0s"
	}
	d := time.Until(t)
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
