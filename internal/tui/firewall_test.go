package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/wes/jokku/internal/types"
)

func sampleFirewall() *types.Firewall {
	now := time.Now()
	return &types.Firewall{
		Settings: types.FirewallSettings{Enabled: true, NotFound: 20, Window: "10m", Ban: "1h", Rate: 50, Burst: 300, OffForApps: []string{"api"}},
		Bans: []types.FirewallBan{
			{Addr: "185.177.72.100", App: "joedesigns", At: now.Add(-12 * time.Minute), Until: now.Add(48*time.Minute + 30*time.Second),
				Blocked: 2919, Nodes: []string{"jokku-edge1"}, Paths: []string{"/.env", "/.git/config"}},
			{Addr: "2001:db8:1:2::/64", At: now.Add(-time.Minute), Until: now.Add(59 * time.Minute), Blocked: 3, Nodes: []string{"jokku1"}},
		},
		Watching: []types.FirewallSuspect{{Addr: "34.39.249.92", App: "wunl", NotFound: 12, Last: now.Add(-time.Minute)}},
		Slowed:   []types.FirewallSlowedAddr{{Addr: "31.59.160.3", FirewallSlowed: types.FirewallSlowed{App: "joedesigns", Limited: 134, Last: now}}},
		Minute:   types.FirewallCounts{Requests: 30, Blocked: 70, NotFound: 2},
		Hour:     types.FirewallCounts{Requests: 811, Blocked: 1234, Limited: 134, NotFound: 45, Bans: 3},
		Sites: []types.FirewallSite{
			{App: "joedesigns", Hour: types.FirewallCounts{Requests: 700, Blocked: 1200, Limited: 134, NotFound: 40, Bans: 2}},
			{App: "", Hour: types.FirewallCounts{Requests: 1, Blocked: 34, NotFound: 5, Bans: 1}},
		},
		Missing: []string{"jokku4"},
	}
}

func TestFirewallView(t *testing.T) {
	var unbanned []string
	m := &model{ctx: context.Background(), cursor: map[view]int{}, st: sampleStatus(), at: time.Now(), bkBusy: map[string]string{}}
	m.Update(tea.WindowSizeMsg{Width: 150, Height: 50})
	m.key("9")
	if m.view != firewallView {
		t.Fatalf("9 is view %v", m.view)
	}
	m.Update(firewallMsg{fw: sampleFirewall()})
	out := ansi.Strip(m.View())
	for _, want := range []string{
		"9 Firewall", "● on", "20 pages not found within 10m ban an address for 1h", "50 requests a second, bursts of 300", "off for api",
		"jokku4 run a release without the firewall",
		"REQUESTS 100", "BLOCKED 70 70%", "last minute",
		"BANNED (2)", "185.177.72.100", "joedesigns", "12m", "48m", "2919", "jokku-edge1", "/.env /.git/config",
		"2001:db8:1:2::/64",
		"CLOSE TO A BAN", "34.39.249.92", "12 of 20 not found",
		"SLOWED IN THE LAST HOUR", "31.59.160.3", "134 requests",
		"u unban",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// w: the last hour, site by site.
	m.key("w")
	out = ansi.Strip(m.View())
	for _, want := range []string{"last hour", "REQUESTS 2179", "BLOCKED 1234 57%", "SLOWED 134", "BANS 3",
		"joedesigns                    2034      1200  59%      134", "(no app's host)"} {
		if !strings.Contains(out, want) {
			t.Errorf("last hour: missing %q in:\n%s", want, out)
		}
	}

	// u lifts the selected ban.
	m.key("down")
	cmd := m.key("u")
	if cmd == nil || !strings.Contains(ansi.Strip(m.View()), "unbanning 2001:db8:1:2::/64") {
		t.Fatalf("u on the second ban:\n%s", ansi.Strip(m.View()))
	}
	unbanned = append(unbanned, "2001:db8:1:2::/64")
	m.Update(unbanMsg{addr: unbanned[0], had: true})
	if out := ansi.Strip(m.View()); !strings.Contains(out, "✔ unbanned 2001:db8:1:2::/64") {
		t.Errorf("after the unban:\n%s", out)
	}
	m.Update(unbanMsg{addr: "185.177.72.100", err: errors.New("no route to the control node")})
	if out := ansi.Strip(m.View()); !strings.Contains(out, "no route to the control node") {
		t.Errorf("a failed unban:\n%s", out)
	}

	off := sampleFirewall()
	off.Settings.Enabled = false
	m.Update(firewallMsg{fw: off})
	if out := ansi.Strip(m.View()); !strings.Contains(out, "The firewall is off") || strings.Contains(out, "185.177.72.100") {
		t.Errorf("off:\n%s", out)
	}
}
