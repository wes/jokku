package splash

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wes/jokku/internal/types"
)

var now = time.Date(2026, 10, 10, 14, 0, 0, 0, time.UTC)

func controlNode() *Info {
	return &Info{
		Host: "jokku1", Role: "control", Running: true, Status: "ready", Version: "v0.8.3", Mesh: "10.210.1.1",
		Cluster: true, Apps: 15, Databases: 3, Asleep: 1, Stopped: 2, Nodes: 6, NodesUp: 5, Down: []string{"jokku4"},
		Unhealthy: []AppHealth{{"api", 0, 1}}, Instances: 19, Here: 8,
		LastDeploy: &types.Deploy{App: "blog", Status: "succeeded", CreatedAt: now.Add(-2 * time.Hour)},
		IP:         "192.168.86.113", CPUs: 8, Load: []float64{0.18, 0.15, 0.10}, MemoryMB: 29 * 1024, MemoryUsedMB: 3584,
		DiskMB: 387 * 1024, DiskUsedMB: 42 * 1024, DiskAvailMB: 345 * 1024, Uptime: 45*time.Hour + 51*time.Minute,
		Updates: 12, SecurityUpdates: 3, Now: now,
	}
}

func render(in *Info, o Options) []string {
	var b strings.Builder
	Render(&b, in, o)
	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = ansi.ReplaceAllString(l, "")
	}
	return out
}

func TestTheSplashFitsAnEightyColumnLogin(t *testing.T) {
	lines := render(controlNode(), Options{Color: true})
	text := plain(lines)
	for i, l := range text {
		if n := utf8.RuneCountInString(l); n > fullWidth {
			t.Errorf("line %d is %d columns: %q", i, n, l)
		}
	}
	// The sun stands beside the logo, a line taller at each end.
	for i, l := range text[:8] {
		r := []rune(l)
		pic := strings.TrimSpace(string(r[min(len(r), len(indent)+logoWidth+len(artGap)):]))
		if pic == "" || strings.Trim(pic, " ▀▄█") != "" || (i >= 1 && i <= 6) != strings.ContainsAny(l, "╗╝") {
			t.Errorf("art line %d: %q", i, l)
		}
	}
	if text[8] != "" || !strings.HasPrefix(text[9], "  jokku1  control node  ● ready  jokku v0.8.3") {
		t.Errorf("headline: %q", text[8:10])
	}
	if !strings.Contains(lines[9], "\x1b[38;5;114m● ready") {
		t.Errorf("ready isn't green: %q", lines[9])
	}
	if last := text[len(text)-1]; last != "  jokku help · jokku top · jokku apps:list · docs at https://jokku.dev" {
		t.Errorf("last line: %q", last)
	}
}

// TestTheSunIsQuarterBlocks: two pixels across and two down to a
// character, in at most two colors, an empty pixel counting as one.
func TestTheSunIsHalfBlocks(t *testing.T) {
	if len(sun) != 2*(len(logo)+2) {
		t.Errorf("the sun is %d pixels tall, want %d", len(sun), 2*(len(logo)+2))
	}
	for y, row := range sun {
		if len(row) != sunWidth {
			t.Errorf("row %d is %d pixels wide, want %d", y, len(row), sunWidth)
		}
		if left, right := len(row)-len(strings.TrimLeft(row, ".")), len(row)-len(strings.TrimRight(row, ".")); left != right && left != len(row) {
			t.Errorf("row %d isn't centered: %q", y, row)
		}
		for _, p := range []byte(row) {
			if _, ok := palette[p]; !ok && p != '.' {
				t.Errorf("row %d: %q isn't in the palette", y, p)
			}
		}
	}

	got := halfBlocks([]string{"yy.r.", "y.yo."})
	want := fg(yellowOrange) + "█" + reset + fg(yellowOrange) + "▀" + reset + fg(yellowOrange) + "▄" + reset +
		fg(redOrange) + bg(orange) + "▀" + reset + " "
	if len(got) != 1 || got[0] != want {
		t.Errorf("halfBlocks = %q, want %q", got, want)
	}
}

func TestTheGridPutsTheClusterLeftAndTheMachineRight(t *testing.T) {
	text := plain(render(controlNode(), Options{}))
	want := []string{
		"  apps       15 · 1 asleep · 2 stopped        ip      192.168.86.113",
		"  databases  3                                load    0.18 0.15 0.10 · 8 cpus",
		"  nodes      5 of 6 ready                     memory  3.5G of 29G",
		"  instances  19 · 8 on this node              disk    11% of 387G",
		"  mesh       10.210.1.1                       uptime  1 day, 21 hours",
		"  deployed   blog, 2 hours ago",
		"",
		"  ▲ jokku4 is down",
		"  ▲ api is unhealthy: 0 of 1 instances up",
		"  ▲ 12 updates available (3 security) · apt upgrade",
	}
	got := strings.Join(text, "\n")
	if !strings.Contains(got, strings.Join(want, "\n")) {
		t.Errorf("got:\n%s\nwant it to contain:\n%s", got, strings.Join(want, "\n"))
	}
	// The right column starts where the sun does.
	l := text[10]
	if col := utf8.RuneCountInString(l[:strings.Index(l, " ip ")+1]); col != len(indent)+logoWidth+len(artGap) {
		t.Errorf("right column at %d", col)
	}
	// Without color there is no picture, only the logo.
	if strings.ContainsAny(got, "▀▄") {
		t.Error("drew the sun without color")
	}
}

func TestNarrowTerminalsGetLess(t *testing.T) {
	text := plain(render(controlNode(), Options{Color: true, Width: 60}))
	got := strings.Join(text, "\n")
	if strings.ContainsAny(got, "▀▄") || !strings.Contains(got, "██╗") {
		t.Errorf("60 columns should have the logo but no sun:\n%s", got)
	}
	if !strings.Contains(got, "\n  deployed   blog, 2 hours ago\n  ip         192.168.86.113\n") {
		t.Errorf("60 columns should have one column of figures:\n%s", got)
	}
	text = plain(render(controlNode(), Options{Color: true, Width: 40}))
	if text[1] != "  JOKKU" {
		t.Errorf("40 columns: %q", text[:3])
	}
}

func TestWorkersAndEdgesShowWhatTheyKnow(t *testing.T) {
	worker := &Info{Host: "jokku3", Role: "worker", Running: true, Version: "v0.8.3", Mesh: "10.210.3.1",
		Control: "192.168.86.113", Peers: 5, Here: 4, IP: "192.168.86.115", Now: now}
	got := strings.Join(plain(render(worker, Options{})), "\n")
	for _, want := range []string{
		"  jokku3  worker node  ● running  jokku v0.8.3",
		"  control    192.168.86.113                   ip      192.168.86.115",
		"  nodes      6 in the cluster",
		"  instances  4 on this node",
		"  mesh       10.210.3.1",
		"  jokku commands run on the control node · docs at https://jokku.dev",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("worker splash is missing %q:\n%s", want, got)
		}
	}

	edge := &Info{Host: "edge1", Role: "edge", Running: true, Version: "v0.8.3", Control: "10.210.1.1", Peers: 3, Routes: 1, Now: now}
	got = strings.Join(plain(render(edge, Options{})), "\n")
	for _, want := range []string{"  edge1  edge  ● running", "  routes     1 app", "  nodes      3 behind it"} {
		if !strings.Contains(got, want) {
			t.Errorf("edge splash is missing %q:\n%s", want, got)
		}
	}
}

func TestTheSplashSaysWhenJokkuIsDown(t *testing.T) {
	in := &Info{Host: "jokku1", Role: "control", Version: "v0.8.3", IP: "192.168.86.113", Now: now}
	text := plain(render(in, Options{}))
	if !strings.HasPrefix(text[8], "  jokku1  control node  ● jokku is not running") {
		t.Errorf("headline: %q", text[8])
	}
	if text[10] != "  ip         192.168.86.113" {
		t.Errorf("the machine's figures should still show: %q", text[10:])
	}
}

func TestFailedDeploysAndFullDisksStandOut(t *testing.T) {
	in := controlNode()
	in.LastDeploy = &types.Deploy{App: "blog", Status: "failed", CreatedAt: now.Add(-3 * time.Minute)}
	in.DiskAvailMB = 2 * 1024
	in.DiskUsedMB = 380 * 1024
	lines := render(in, Options{Color: true})
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"\x1b[38;5;221mblog failed, 3 minutes ago",
		"\x1b[38;5;221m100% of 387G",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, strings.Join(plain(lines), "\n"))
		}
	}
}

func TestFormatting(t *testing.T) {
	for mb, want := range map[int]string{512: "512M", 3584: "3.5G", 29 * 1024: "29G", 387*1024 + 300: "387G", 2 * 1024 * 1024: "2.0T"} {
		if got := size(mb); got != want {
			t.Errorf("size(%d) = %q, want %q", mb, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		5 * time.Minute: "5 minutes", time.Hour: "1 hour", 3*time.Hour + time.Minute: "3 hours, 1 minute",
		24 * time.Hour: "1 day", 45*time.Hour + 51*time.Minute: "1 day, 21 hours", 30 * 24 * time.Hour: "30 days",
	} {
		if got := uptime(d); got != want {
			t.Errorf("uptime(%s) = %q, want %q", d, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", time.Minute: "1 minute ago", 47 * time.Hour: "47 hours ago", 72 * time.Hour: "3 days ago",
	} {
		if got := ago(now, now.Add(-d)); got != want {
			t.Errorf("ago(%s) = %q, want %q", d, got, want)
		}
	}
}
