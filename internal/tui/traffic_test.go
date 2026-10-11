package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/wes/jokku/internal/types"
)

func trafficModel(w, h int) *model {
	m := &model{ctx: context.Background(), cursor: map[view]int{}, st: sampleStatus(), at: time.Now(), view: trafficView}
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.tr = newTraffic()
	m.tr.connected = true
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

// TestTrafficLaneNames: the names column grows to fit a long name, up to
// a point, and a name lights up while its app has a request on the way.
func TestTrafficLaneNames(t *testing.T) {
	defer lipgloss.SetColorProfile(lipgloss.ColorProfile())
	lipgloss.SetColorProfile(termenv.TrueColor)
	clock := stopClock(t)
	long := func(w, h int) *model {
		m := trafficModel(w, h)
		m.st.Apps = append(m.st.Apps, types.AppView{Name: "uptime-limehosting"}, types.AppView{Name: "a-really-long-app-name-for-testing"})
		return m
	}
	m := long(160, 40)
	raw := m.View()
	lines := map[string]string{}
	for _, l := range strings.Split(ansi.Strip(raw), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			lines[f[0]] = l
		}
	}
	// 24 columns of names, then the machines and the rate, lined up.
	rate := -1
	for name, want := range map[string]string{
		"blog":                     "  blog" + strings.Repeat(" ", 20) + " ",
		"uptime-limehosting":       "  uptime-limehosting" + strings.Repeat(" ", 6) + " ",
		"a-really-long-app-name-…": "  a-really-long-app-name-… ",
	} {
		l := lines[name]
		if !strings.HasPrefix(l, want) {
			t.Errorf("%s's lane: %q, want it to start %q", name, l, want)
		}
		if i := utf8.RuneCountInString(l[:max(strings.Index(l, "0.0/s"), 0)]); rate == -1 {
			rate = i
		} else if i != rate {
			t.Errorf("%s's rate is at %d, not %d: %q", name, i, rate, l)
		}
	}
	// shop has requests on the way; blog has none.
	if !strings.Contains(raw, sAccent.Render("shop")) || strings.Contains(raw, sAccent.Render("blog")) {
		t.Error("only shop's name should be lit")
	}
	*clock = clock.Add(travelTime * 2)
	m.updateTraffic(frameMsg{})
	if strings.Contains(m.View(), sAccent.Render("shop")) {
		t.Error("shop's name is still lit after its requests landed")
	}

	// A narrow terminal keeps its lanes: the names go back to 14 columns.
	for _, w := range []int{60, 100} {
		m := long(w, 30)
		for _, l := range strings.Split(m.View(), "\n") {
			if lipgloss.Width(l) > w {
				t.Errorf("%d columns: a line of %d", w, lipgloss.Width(l))
				break
			}
		}
		if out := ansi.Strip(m.View()); w == 60 && !strings.Contains(out, "  uptime-limeho… 0      0.0/s") {
			t.Errorf("60 columns:\n%s", out)
		}
	}
}

// TestTrafficMachines: each app's machines after its name, a line for
// each process group when there's more than one, and sleeping machines
// dim.
func TestTrafficMachines(t *testing.T) {
	defer lipgloss.SetColorProfile(lipgloss.ColorProfile())
	lipgloss.SetColorProfile(termenv.TrueColor)
	m := trafficModel(160, 40)
	inst := func(app, name string, release int, state, desired string) types.InstanceView {
		return types.InstanceView{App: app, Instance: types.Instance{Name: name, Release: release, State: state, Desired: desired}}
	}
	m.st.Apps = append(m.st.Apps,
		types.AppView{Name: "sleepy", Release: 2, Asleep: true, Wanted: 2},
		types.AppView{Name: "home", Kind: types.KindExternal})
	m.st.Instances = append(m.st.Instances,
		inst("shop", "worker.1", 12, "healthy", "running"),
		inst("shop", "web.3", 11, "healthy", "running"), // the old release's, on its way out
		inst("shop", "web.4", 12, "healthy", "stopped"), // retiring
		inst("sleepy", "web.1", 2, "stopped", "running"),
		inst("sleepy", "web.2", 2, "stopped", "running"))
	raw := strings.Split(m.View(), "\n")
	plain := strings.Split(ansi.Strip(m.View()), "\n")
	find := func(prefix string) (string, string) {
		for i, l := range plain {
			if strings.HasPrefix(l, prefix) {
				return l, raw[i]
			}
		}
		t.Fatalf("no line starts %q:\n%s", prefix, strings.Join(plain, "\n"))
		return "", ""
	}
	field := func(l string) string { return strings.Join(strings.Fields(l)[1:3], " ") }

	shop, _ := find("  shop ")
	web, _ := find("    web ")
	worker, workerRaw := find("    worker ")
	if field(shop) != "3 ●●○" || field(web) != "2 ●○" || field(worker) != "1 ●" {
		t.Errorf("shop: %q, web: %q, worker: %q", shop, web, worker)
	}
	if strings.Contains(workerRaw, "┄") {
		t.Errorf("a process group's line has a lane: %q", worker)
	}
	// One process group: no lines under it.
	blog, _ := find("  blog ")
	if field(blog) != "1 ○" {
		t.Errorf("blog: %q", blog)
	}
	for i, l := range plain {
		if strings.HasPrefix(l, "  blog ") && strings.HasPrefix(plain[i+1], "    ") {
			t.Errorf("blog has a line under it: %q", plain[i+1])
		}
	}
	// Asleep: dim, not down, and said on the right.
	sleepy, sleepyRaw := find("  sleepy ")
	if field(sleepy) != "2 ○○" || !strings.HasSuffix(strings.TrimRight(sleepy, " "), "asleep") ||
		!strings.Contains(sleepyRaw, sDim.Render("○")) || strings.Contains(sleepyRaw, sBad.Render("○")) {
		t.Errorf("sleepy: %q", sleepy)
	}
	// No machines to count for a host no app serves, or an external app.
	for _, prefix := range []string{"  (no app) ", "  home "} {
		if l, _ := find(prefix); !strings.HasSuffix(strings.Fields(strings.TrimPrefix(l, prefix))[0], "/s") {
			t.Errorf("%q has machines: %q", prefix, l)
		}
	}

	// The overview shows a sleeping app's machines dim too.
	m.view = overview
	for _, l := range strings.Split(m.View(), "\n") {
		if strings.Contains(l, "sleepy") && (!strings.Contains(l, sDim.Render("○○")) || strings.Contains(l, sBad.Render("○"))) {
			t.Errorf("overview: %q", ansi.Strip(l))
		}
	}
}

// stopClock runs the animation on a clock the test moves.
func stopClock(t *testing.T) *time.Time {
	clock := time.Now()
	timeNow = func() time.Time { return clock }
	t.Cleanup(func() { timeNow = time.Now })
	return &clock
}

func TestTrafficLaneEffects(t *testing.T) {
	clock := stopClock(t)
	tr := newTraffic()
	tr.lanes["shop"] = []particle{{status: 200, born: clock.Add(-750 * time.Millisecond), trip: travelTime, target: "web.1@control"}}
	pal := colors()
	lane := func() string { return ansi.Strip(tr.lane("shop", 60, .5, *clock, pal)) }

	out := lane()
	if w := lipgloss.Width(out); w != 60 {
		t.Errorf("the lane is %d wide, not 60", w)
	}
	dot, tail := strings.Index(out, "●"), strings.Index(out, "━")
	if dot < 0 || tail < 0 || tail > dot {
		t.Errorf("a dot mid-lane should trail a tail behind it: %q", out)
	}

	tr.lite = true
	if out := lane(); !strings.Contains(out, "●") || strings.ContainsAny(out, "━─") {
		t.Errorf("with effects off, only the dot: %q", out)
	}
	tr.lite = false

	// A 5xx that just landed bursts at the end of the lane and lights up
	// the arrow.
	tr.lanes["shop"] = nil
	tr.arrivals["shop"] = &arrivals{lastErr: clock.Add(-50 * time.Millisecond), by: map[string]landing{}}
	out = lane()
	if last := []rune(out)[59]; !strings.ContainsRune("✺✹✸✷✶", last) {
		t.Errorf("no burst at the end of the lane: %q", out)
	}
	if tr.arrow("shop", *clock, pal) == tr.arrow("shop", clock.Add(time.Second), pal) && lipgloss.ColorProfile() != termenv.Ascii {
		t.Error("the arrow doesn't light up")
	}
	*clock = clock.Add(burstTime)
	if out := lane(); strings.ContainsAny(out, "✺✹✸✷✶•·") {
		t.Errorf("the burst should be over: %q", out)
	}
}

func TestSlowRequestsCrawl(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		ms, p95  float64
		slow     bool
		min, max time.Duration
	}{
		{20, 50, false, travelTime * 9 / 10, travelTime * 11 / 10},
		{80, 50, false, travelTime * 9 / 10, travelTime * 11 / 10}, // over p95, but quick
		{400, 50, true, travelTime * 3 / 2 * 9 / 10, travelTime * 3 * 11 / 10},
		{60000, 50, true, travelTime * 3 * 9 / 10, travelTime * 3 * 11 / 10}, // crawls, but arrives
	} {
		p := newParticle(types.Request{Status: 200, DurationMS: c.ms, Instance: "web.1", InstanceNode: "n1"}, now, c.p95)
		if p.slow != c.slow || p.trip < c.min || p.trip > c.max {
			t.Errorf("%vms (p95 %vms): slow %v, trip %v; want slow %v, %v to %v", c.ms, c.p95, p.slow, p.trip, c.slow, c.min, c.max)
		}
		if p.target != "web.1@n1" {
			t.Errorf("target %q", p.target)
		}
	}
}

func TestTrafficLandings(t *testing.T) {
	clock := stopClock(t)
	m := trafficModel(160, 40)
	*clock = clock.Add(travelTime * 2)
	m.updateTraffic(frameMsg{})
	for lane, ps := range m.tr.lanes {
		if len(ps) > 0 {
			t.Errorf("%s: %d dots still in flight", lane, len(ps))
		}
	}
	a := m.tr.arrivals["shop"]
	if a == nil || a.lastErr.IsZero() {
		t.Fatalf("shop's landings weren't noted: %+v", a)
	}
	if len(a.by) != 2 {
		t.Errorf("landings on %d instances, not 2: %v", len(a.by), a.by)
	}

	// A failure stays lit for its moment while others land on the same
	// instance, then gives way.
	tr := newTraffic()
	land := func(status int, at time.Time) {
		tr.lanes["shop"] = []particle{{status: status, born: at.Add(-travelTime), trip: travelTime, target: "web.1"}}
		tr.land(at)
	}
	land(502, *clock)
	land(200, clock.Add(landGlow/2))
	if l := tr.arrivals["shop"].by["web.1"]; l.status != 502 {
		t.Errorf("the 502 was covered by a %d", l.status)
	}
	if l := tr.arrivals["shop"].last; l.status != 200 {
		t.Errorf("the lane's latest landing is a %d", l.status)
	}
	land(200, clock.Add(2*landGlow))
	if l := tr.arrivals["shop"].by["web.1"]; l.status != 200 {
		t.Errorf("the 502 is still lit")
	}
}

func TestTrafficSparklines(t *testing.T) {
	clock := stopClock(t)
	m := &model{ctx: context.Background(), cursor: map[view]int{}, st: sampleStatus(), at: *clock, view: trafficView}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.tr = newTraffic()
	var batch []types.Request
	for s := 1; s <= 3; s++ {
		for i := 0; i < s*2; i++ {
			r := types.Request{At: clock.Add(-time.Duration(s) * time.Second), App: "shop", Status: 200, DurationMS: float64(10 * s)}
			if s == 2 && i == 0 {
				r.Status = 500
			}
			batch = append(batch, r)
		}
	}
	batch = append(batch, types.Request{At: clock.Add(-2 * time.Hour), App: "shop", Status: 200}) // too old to count
	m.updateTraffic(requestsMsg(batch))
	m.updateTraffic(frameMsg{})
	d := m.tr.spark
	n := historyLen
	// The current second isn't over, so the last column is a second ago.
	if d.reqs[n-1] != 2 || d.reqs[n-2] != 4 || d.reqs[n-3] != 6 || d.reqs[n-4] != 0 {
		t.Errorf("requests a second: %v", d.reqs[n-4:])
	}
	if d.p95[n-1] != 10 || d.p95[n-3] != 30 {
		t.Errorf("p95 a second: %v", d.p95[n-4:])
	}
	if d.errs[n-2] == 0 || d.errs[n-1] != 0 {
		t.Errorf("failures a second: %v", d.errs[n-4:])
	}
	if out := ansi.Strip(m.View()); !strings.Contains(out, "█▆▃ req/s") || !strings.Contains(out, " p95") {
		t.Errorf("no sparklines:\n%s", out)
	}
}

func TestTrafficFramesDontMultiply(t *testing.T) {
	m := trafficModel(100, 30)
	m.tr.ticking = false
	if m.tr.startFrames() == nil {
		t.Fatal("frames didn't start")
	}
	if m.tr.startFrames() != nil {
		t.Error("a second run of frames started")
	}
	// Leaving the view stops them, and coming back starts them again.
	m.view = overview
	if m.updateTraffic(frameMsg{}) != nil || m.tr.ticking {
		t.Error("frames kept running outside the view")
	}
	m.view = trafficView
	if m.tr.startFrames() == nil {
		t.Error("frames didn't start again")
	}
}

func TestTrafficNumbersRoll(t *testing.T) {
	clock := stopClock(t)
	m := trafficModel(160, 40)
	total := float64(m.tr.total)
	if s := m.tr.shown["total"]; s <= 0 || s >= total {
		t.Errorf("after one frame the total should be rolling up to %v, not at %v", total, s)
	}
	for range 30 {
		*clock = clock.Add(frameEvery)
		m.updateTraffic(frameMsg{})
	}
	if s := m.tr.shown["total"]; s != total {
		t.Errorf("the total should have settled at %v, not %v", total, s)
	}
	m.tr.lite = true
	m.tr.shown["total"] = 1
	if !strings.Contains(ansi.Strip(m.View()), fmt.Sprintf("%d total", m.tr.total)) {
		t.Error("with effects off, numbers shouldn't roll")
	}

	m.trafficKey("c")
	if m.tr.total != 0 || len(m.tr.shown) != 0 || len(m.tr.history) != 0 || len(m.tr.arrivals) != 0 || len(m.tr.listed) != 0 {
		t.Error("c should clear everything")
	}
}
