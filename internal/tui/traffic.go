package tui

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/wes/jokku/internal/types"
)

// The traffic view: every request the cluster's proxies handle flies across
// its app's lane as a dot (colored by status) and lands on the instance that
// answered, with live rates and latencies around it.
//
// With effects on (f turns them off, for a slow connection) it draws about
// 30 frames a second: dots trail tails that fade into the track, slow
// requests crawl, busy lanes glow, landings light up their instance, a 5xx
// bursts where it lands, and the numbers roll rather than jump.

const (
	travelTime   = 1500 * time.Millisecond // a dot's trip across its lane
	frameEvery   = 33 * time.Millisecond
	liteFrame    = 80 * time.Millisecond // with effects off
	statsWindow  = 10 * time.Second
	statsEvery   = 200 * time.Millisecond // the numbers roll between
	historyLen   = 60                     // seconds of the header's sparklines
	maxParticles = 80                     // per lane; beyond that requests are counted, not drawn
	keepRecent   = 300
	slowFloorMS  = 100                     // a request faster than this is never slow
	landGlow     = 400 * time.Millisecond  // how long a landing lights up its instance
	burstTime    = 600 * time.Millisecond  // a 5xx's burst where it lands
	sweepEvery   = 2400 * time.Millisecond // the light sweeping along busy lanes
	rollTime     = 150 * time.Millisecond  // how quickly numbers roll to new values
	// The list of recent requests is for reading: redrawing it every frame
	// would only cost bandwidth.
	listEvery = 250 * time.Millisecond
)

// timeNow is the clock the animation runs on; tests move it themselves.
var timeNow = time.Now

type particle struct {
	status int
	born   time.Time
	trip   time.Duration // how long it takes to cross: slow requests take longer
	slow   bool          // slower than most of its app's requests: it trails amber
	target string        // where it lands
}

// landing is a request reaching the instance that answered it.
type landing struct {
	at     time.Time
	status int
}

// arrivals is what lights up at the end of a lane.
type arrivals struct {
	last    landing            // the latest request to land
	lastErr time.Time          // the latest 5xx
	by      map[string]landing // each instance's latest
}

// second is the requests of one second, for the sparklines.
type second struct {
	at      int64 // unix seconds
	n, errs int
	lat     []float64
	p95     float64
	done    bool // the second is over and p95 is final
}

// sparkData is the last historyLen seconds, oldest first.
type sparkData struct {
	at        int64 // the second it was made in
	reqs, p95 []float64
	errs      []float64 // how much each second failed, 0 to 1
}

// trafficStats are the numbers over the last statsWindow.
type trafficStats struct {
	at       time.Time
	n, errs  int
	rate     float64
	p50, p95 float64
	byApp    map[string]int
	byTarget map[string]map[string]int
	byNode   map[string]int
	appP95   map[string]float64
}

type traffic struct {
	cancel    context.CancelFunc
	ch        chan types.Request
	end       chan error
	err       error
	connected bool
	paused    bool
	lite      bool                  // effects off
	ticking   bool                  // frames are running
	lanes     map[string][]particle // app -> dots in flight
	arrivals  map[string]*arrivals  // app -> where its requests landed
	recent    []types.Request       // newest last
	listed    []types.Request       // recent, as the list shows it
	listedAt  time.Time
	window    []types.Request // the last statsWindow, for rates and latencies
	history   []second        // the last historyLen seconds, oldest first
	spark     sparkData
	total     int
	stats     *trafficStats
	shown     map[string]float64 // numbers as drawn, rolling toward their values
	lastFrame time.Time
}

func newTraffic() *traffic {
	return &traffic{lanes: map[string][]particle{}, arrivals: map[string]*arrivals{}, shown: map[string]float64{}}
}

type requestsMsg []types.Request
type trafficEndMsg struct{ err error }
type frameMsg struct{}
type trafficRetryMsg struct{}

func (tr *traffic) frame() tea.Cmd {
	every := frameEvery
	if tr.lite {
		every = liteFrame
	}
	return tea.Tick(every, func(time.Time) tea.Msg { return frameMsg{} })
}

// startFrames starts the animation unless it is running: frames stop by
// themselves once the view closes.
func (tr *traffic) startFrames() tea.Cmd {
	if tr.ticking {
		return nil
	}
	tr.ticking = true
	return tr.frame()
}

// startTraffic opens the request stream; it runs only while the view is
// open.
func (m *model) startTraffic() tea.Cmd {
	if m.tr == nil {
		m.tr = newTraffic()
	}
	frames := m.tr.startFrames()
	if m.tr.cancel != nil {
		return frames
	}
	ctx, cancel := context.WithCancel(m.ctx)
	ch := make(chan types.Request, 2048)
	end := make(chan error, 1)
	m.tr.cancel, m.tr.ch, m.tr.end, m.tr.err = cancel, ch, end, nil
	go func() {
		err := m.api.Requests(ctx, "", 100, true, func(r types.Request) {
			select {
			case ch <- r:
			default: // the UI is behind; drop rather than block the stream
			}
		})
		end <- err
		close(ch)
	}()
	m.tr.connected = true
	return tea.Batch(waitRequests(ch, end), frames)
}

func (m *model) stopTraffic() {
	if m.tr != nil && m.tr.cancel != nil {
		m.tr.cancel()
		m.tr.cancel = nil
	}
}

// waitRequests delivers the next batch of requests from the stream.
func waitRequests(ch chan types.Request, end chan error) tea.Cmd {
	return func() tea.Msg {
		r, ok := <-ch
		if !ok {
			return trafficEndMsg{<-end}
		}
		batch := []types.Request{r}
		for len(batch) < 500 {
			select {
			case r, ok := <-ch:
				if !ok {
					return requestsMsg(batch)
				}
				batch = append(batch, r)
			default:
				return requestsMsg(batch)
			}
		}
		return requestsMsg(batch)
	}
}

func (m *model) updateTraffic(msg tea.Msg) tea.Cmd {
	tr := m.tr
	switch msg := msg.(type) {
	case requestsMsg:
		now := timeNow()
		for _, r := range msg {
			tr.total++
			tr.recent = append(tr.recent, r)
			tr.window = append(tr.window, r)
			tr.count(r, now)
		}
		for _, r := range msg {
			// History from before the view opened is listed, not animated.
			lane := laneName(r)
			if tr.paused || now.Sub(r.At) >= travelTime || len(tr.lanes[lane]) >= maxParticles {
				continue
			}
			var p95 float64
			if tr.stats != nil {
				p95 = tr.stats.appP95[lane]
			}
			tr.lanes[lane] = append(tr.lanes[lane], newParticle(r, now, p95))
		}
		if len(tr.recent) > keepRecent {
			tr.recent = tr.recent[len(tr.recent)-keepRecent:]
		}
		if tr.ch == nil {
			return nil
		}
		return waitRequests(tr.ch, tr.end)
	case trafficEndMsg:
		tr.cancel, tr.ch, tr.connected = nil, nil, false
		tr.err = msg.err
		if tr.err == nil {
			tr.err = fmt.Errorf("the request stream ended")
		}
		if m.view == trafficView {
			return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return trafficRetryMsg{} })
		}
	case trafficRetryMsg:
		if m.view == trafficView {
			return m.startTraffic()
		}
	case frameMsg:
		if m.view != trafficView {
			tr.ticking = false
			return nil
		}
		now := timeNow()
		tr.land(now)
		cut := 0
		for cut < len(tr.window) && now.Sub(tr.window[cut].At) > statsWindow {
			cut++
		}
		tr.window = tr.window[cut:]
		if tr.stats == nil || now.Sub(tr.stats.at) >= statsEvery {
			tr.stats = tr.summarize(now)
		}
		tr.sparks(now)
		tr.roll(now)
		if now.Sub(tr.listedAt) >= listEvery {
			tr.listed, tr.listedAt = tr.recent[max(len(tr.recent)-100, 0):], now
		}
		return tr.frame()
	}
	return nil
}

// newParticle sets a request off across its lane. Speeds vary a little, so
// dots don't move in lockstep, and a request slower than most of its app's
// (its p95, and at least slowFloorMS) crawls, the slower the slower it was.
func newParticle(r types.Request, now time.Time, p95 float64) particle {
	trip := float64(travelTime) * (0.9 + 0.2*rand.Float64())
	limit := max(p95, slowFloorMS)
	slow := r.DurationMS > limit
	if slow {
		trip *= min(1.5+0.5*math.Log2(r.DurationMS/limit), 3)
	}
	return particle{status: r.Status, born: now, trip: time.Duration(trip), slow: slow, target: target(r)}
}

// land retires the dots that have arrived, noting where they landed.
func (tr *traffic) land(now time.Time) {
	for lane, ps := range tr.lanes {
		kept := ps[:0]
		for _, p := range ps {
			if now.Sub(p.born) < p.trip {
				kept = append(kept, p)
				continue
			}
			a := tr.arrivals[lane]
			if a == nil {
				a = &arrivals{by: map[string]landing{}}
				tr.arrivals[lane] = a
			}
			l := landing{at: p.born.Add(p.trip), status: p.status}
			if l.at.After(a.last.at) {
				a.last = l
			}
			if p.status >= 500 {
				a.lastErr = l.at
			}
			// A failure stays lit for its moment, even if others land on it.
			if cur, ok := a.by[p.target]; !ok || cur.status < 500 || p.status >= 500 || l.at.Sub(cur.at) > landGlow {
				a.by[p.target] = l
			}
		}
		tr.lanes[lane] = kept
	}
}

// count adds a request to its second of history.
func (tr *traffic) count(r types.Request, now time.Time) {
	at := r.At.Unix()
	if at < now.Unix()-historyLen {
		return
	}
	i, found := slices.BinarySearchFunc(tr.history, at, func(s second, at int64) int { return int(s.at - at) })
	if !found {
		tr.history = slices.Insert(tr.history, i, second{at: at})
	}
	s := &tr.history[i]
	s.n++
	if r.Status >= 500 {
		s.errs++
	}
	if !s.done && len(s.lat) < 5000 {
		s.lat = append(s.lat, r.DurationMS)
	}
}

// sparks remakes the sparklines' data once a second, from the seconds that
// are over.
func (tr *traffic) sparks(now time.Time) {
	cur := now.Unix()
	if tr.spark.at == cur {
		return
	}
	cut := 0
	for cut < len(tr.history) && tr.history[cut].at < cur-historyLen {
		cut++
	}
	tr.history = tr.history[cut:]
	d := sparkData{at: cur, reqs: make([]float64, historyLen), p95: make([]float64, historyLen), errs: make([]float64, historyLen)}
	for i := range tr.history {
		s := &tr.history[i]
		if s.at >= cur {
			continue
		}
		if !s.done {
			s.p95, s.lat, s.done = percentile(s.lat, 95), nil, true
		}
		j := int(s.at - (cur - historyLen))
		// Even a few failures show, and a quarter failing is all red.
		d.reqs[j], d.p95[j], d.errs[j] = float64(s.n), s.p95, clamp01(math.Sqrt(4*float64(s.errs)/float64(max(s.n, 1))))
	}
	tr.spark = d
}

func (tr *traffic) summarize(now time.Time) *trafficStats {
	st := &trafficStats{at: now, n: len(tr.window), byApp: map[string]int{}, byTarget: map[string]map[string]int{},
		byNode: map[string]int{}, appP95: map[string]float64{}}
	lat := make([]float64, 0, len(tr.window))
	byApp := map[string][]float64{}
	for _, r := range tr.window {
		lat = append(lat, r.DurationMS)
		if r.Status >= 500 {
			st.errs++
		}
		lane := laneName(r)
		st.byApp[lane]++
		byApp[lane] = append(byApp[lane], r.DurationMS)
		if st.byTarget[lane] == nil {
			st.byTarget[lane] = map[string]int{}
		}
		st.byTarget[lane][target(r)]++
		st.byNode[r.Node]++
	}
	st.rate = float64(st.n) / statsWindow.Seconds()
	st.p50, st.p95 = percentile(lat, 50), percentile(lat, 95)
	for lane, v := range byApp {
		st.appP95[lane] = percentile(v, 95)
	}
	return st
}

// roll moves the numbers on screen toward their values, so they count up
// and down rather than jump.
func (tr *traffic) roll(now time.Time) {
	dt := min(now.Sub(tr.lastFrame), 200*time.Millisecond)
	tr.lastFrame = now
	k := 1 - math.Exp(-float64(dt)/float64(rollTime))
	st := tr.stats
	want := map[string]float64{"rate": st.rate, "p50": st.p50, "p95": st.p95, "total": float64(tr.total)}
	for lane, n := range st.byApp {
		want["lane:"+lane] = float64(n) / statsWindow.Seconds()
	}
	for key := range tr.shown {
		if _, ok := want[key]; !ok {
			want[key] = 0
		}
	}
	for key, v := range want {
		s := tr.shown[key]
		s += (v - s) * k
		if math.Abs(v-s) < 0.05 {
			if v == 0 {
				delete(tr.shown, key)
				continue
			}
			s = v
		}
		tr.shown[key] = s
	}
}

// num is a number as it is drawn: rolling, with effects on.
func (tr *traffic) num(key string, v float64) float64 {
	if s, ok := tr.shown[key]; ok && !tr.lite {
		return s
	}
	return v
}

func laneName(r types.Request) string {
	if r.App == "" {
		return "(no app)"
	}
	return r.App
}

// target is where a request landed: the instance that answered and its
// node, or why nothing did.
func target(r types.Request) string {
	switch {
	case r.Instance != "":
		return r.Instance + "@" + r.InstanceNode
	case r.Status == 404 && r.App == "":
		return "404 unknown host"
	case r.App != "":
		return fmt.Sprintf("%d (no instance)", r.Status)
	}
	return ""
}

func (m *model) trafficKey(k string) bool {
	if m.view != trafficView || m.tr == nil {
		return false
	}
	tr := m.tr
	switch k {
	case "p":
		tr.paused = !tr.paused
		return true
	case "f":
		tr.lite = !tr.lite
		return true
	case "c":
		tr.lanes, tr.arrivals, tr.shown = map[string][]particle{}, map[string]*arrivals{}, map[string]float64{}
		tr.recent, tr.listed, tr.window, tr.history, tr.spark, tr.total, tr.stats = nil, nil, nil, nil, sparkData{}, 0, nil
		return true
	}
	return false
}

func (m *model) trafficView() string {
	tr := m.tr
	if tr == nil {
		return sDim.Render("  connecting...")
	}
	var b strings.Builder
	now := timeNow()
	pal := colors()
	st := tr.stats
	if st == nil {
		st = tr.summarize(now)
	}
	secs := statsWindow.Seconds()

	// Headline numbers over the last statsWindow, and under them the last
	// minute's requests and p95, a second at a time: on a line of their own,
	// which changes only once a second.
	rate := sTitle.Render(fmt.Sprintf("%.1f req/s", tr.num("rate", st.rate)))
	lat := fmt.Sprintf("p50 %s  p95 %s", fmtMS(tr.num("p50", st.p50)), fmtMS(tr.num("p95", st.p95)))
	rest := fmt.Sprintf("  ·  %s  ·  %d total", errRate(st.errs, st.n), int(math.Round(tr.num("total", float64(tr.total)))))
	switch {
	case tr.err != nil && !tr.connected:
		rest += "  ·  " + sBad.Render("reconnecting: "+tr.err.Error())
	case tr.paused:
		rest += "  ·  " + sWarn.Render("paused (p)")
	}
	if tr.lite {
		rest += "  ·  " + sDim.Render("effects off (f)")
	}
	b.WriteString("  " + rate + "  ·  " + lat + rest + "\n")
	if d := tr.spark; d.at != 0 {
		reqW, latW := sparkWidths(m.width - 17)
		var line []string
		if reqW > 0 {
			line = append(line, spark(d.reqs[historyLen-reqW:], d.errs[historyLen-reqW:],
				pal.track.mix(pal.accent, .4), pal.accent.mix(pal.flash, .3), pal.bad, pal.track)+sDim.Render(" req/s"))
		}
		if latW > 0 {
			line = append(line, spark(d.p95[historyLen-latW:], nil, pal.track.mix(pal.cyan, .4), pal.cyan, pal.bad, pal.track)+sDim.Render(" p95"))
		}
		b.WriteString("  " + strings.Join(line, "   "))
	}
	b.WriteString("\n\n")

	// One lane per app: known apps first, then any other host seen.
	lanes := map[string]bool{}
	if m.st != nil {
		for _, a := range m.st.Apps {
			lanes[a.Name] = true
		}
	}
	for l := range st.byApp {
		lanes[l] = true
	}
	for l, ps := range tr.lanes {
		if len(ps) > 0 {
			lanes[l] = true
		}
	}
	names := make([]string, 0, len(lanes))
	for l := range lanes {
		names = append(names, l)
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == "(no app)") != (names[j] == "(no app)") {
			return names[j] == "(no app)"
		}
		return names[i] < names[j]
	})
	labelW := 14
	rightW := max(m.width*35/100, 30)
	laneW := max(m.width-labelW-rightW-8, 10)
	if len(names) == 0 {
		b.WriteString(sDim.Render("  no apps yet") + "\n")
	}
	for _, name := range names {
		perSec := float64(st.byApp[name]) / secs
		label := fmt.Sprintf("  %-*s %5.1f/s ", labelW, trunc(name, labelW), tr.num("lane:"+name, perSec))
		heat := clamp01(math.Log10(1+perSec) / 2) // 1/s a little, 100/s all the way
		b.WriteString(label + tr.lane(name, laneW, heat, now, pal) + tr.arrow(name, now, pal) +
			tr.targets(name, st.byTarget[name], secs, rightW, now, pal) + "\n")
	}

	// Which nodes' proxies receive the traffic.
	if len(st.byNode) > 0 {
		var parts []string
		for _, n := range sortedKeys(st.byNode) {
			parts = append(parts, fmt.Sprintf("%s %s", n, sGood.Render(fmt.Sprintf("%.1f/s", float64(st.byNode[n])/secs))))
		}
		b.WriteString("\n  " + sHead.Render("requests by node  ") + strings.Join(parts, sDim.Render("  ·  ")) + "\n")
	}

	// The latest requests.
	b.WriteString("\n" + sSection.Render("Recent requests") + "\n")
	rows := max(m.height-len(names)-15, 3)
	start := max(len(tr.listed)-rows, 0)
	if len(tr.listed) == 0 {
		b.WriteString(sDim.Render("  waiting for requests...") + "\n")
	}
	for i := len(tr.listed) - 1; i >= start; i-- {
		r := tr.listed[i]
		target := r.Instance
		if target != "" {
			target += "@" + r.InstanceNode
		}
		fmt.Fprintf(&b, "  %s  %-14s %-6s %-30s %s %7s  %-18s %s\n",
			sDim.Render(r.At.Local().Format("15:04:05")), trunc(laneName(r), 14), r.Method, trunc(r.Path, 30),
			statusText(r.Status), fmtMS(r.DurationMS), trunc(target, 18), sDim.Render("via "+r.Node+"  "+r.Client))
	}
	return b.String()
}

// sparkWidths shares room between the two sparklines, dropping the p95 one,
// then both, when there isn't enough.
func sparkWidths(room int) (reqs, p95 int) {
	switch {
	case room >= 16:
		return min(historyLen, room/2), min(historyLen, room/2)
	case room >= 8:
		return room, 0
	}
	return 0, 0
}

// lane draws one app's track with its requests in flight. With effects on,
// each dot trails a tail that fades into the track, the track glows with
// the lane's traffic (heat, from 0 to 1) under a sweep of light, and a 5xx
// bursts where it lands.
func (tr *traffic) lane(name string, width int, heat float64, now time.Time, pal *palette) string {
	type cell struct {
		r     rune
		c     rgb
		level float64
		layer int // the track 0, a tail 1, a dot 2, a failed one 3, a burst 4
	}
	cells := make([]cell, width)
	track := make([]rgb, width)
	sweep := float64(now.UnixNano()%int64(sweepEvery))/float64(sweepEvery)*float64(width+16) - 8
	for i := range cells {
		track[i] = pal.dim
		if !tr.lite {
			// In steps: every color change costs bytes on the wire.
			s := math.Round(4*clamp01(1-math.Abs(float64(i)-sweep)/8)) / 4
			track[i] = pal.track.mix(pal.glow, heat*(.45+.55*s*s))
		}
		cells[i] = cell{r: '┄', c: track[i]}
	}
	put := func(i int, r rune, to rgb, level float64, layer int) {
		if i < 0 || i >= width {
			return
		}
		if c := cells[i]; layer > c.layer || layer == c.layer && level > c.level {
			cells[i] = cell{r, track[i].mix(to, level), level, layer}
		}
	}

	tail := math.Max(4, math.Min(12, float64(width)/9))
	for _, p := range tr.lanes[name] {
		prog := float64(now.Sub(p.born)) / float64(p.trip)
		if prog < 0 || prog >= 1 {
			continue
		}
		x := prog * float64(width-1)
		head := int(x + .5)
		color, glyph, layer := statusColor(p.status, pal), '●', 2
		if p.status >= 500 {
			glyph, layer = '✖', 3
		}
		if tr.lite {
			put(head, glyph, color, 1, layer)
			continue
		}
		// Fade in setting off, and a little landing.
		a := math.Min(1, prog/.06)
		if prog > .9 {
			a *= 1 - .4*(prog-.9)/.1
		}
		put(head, glyph, color.mix(pal.flash, pal.core), a, layer)
		tc, n := color, tail
		if p.slow {
			tc, n = pal.accent, tail*1.5
		}
		// The tail's cells are lit by how far they are behind the dot's true
		// position, between cells, so it slides rather than steps.
		for i := head - 1; i >= 0; i-- {
			t := 1 - (x-float64(i))/n
			if t <= 0 {
				break
			}
			level := a * t * t
			g := '╌'
			switch {
			case level > .45:
				g = '━'
			case level > .18:
				g = '─'
			}
			put(i, g, tc, level, 1)
		}
	}

	if a := tr.arrivals[name]; a != nil && !tr.lite {
		if e := now.Sub(a.lastErr); e >= 0 && e < burstTime {
			t := float64(e) / float64(burstTime)
			burst := []rune("✺✹✸✷✶·")
			put(width-1, burst[min(int(t*float64(len(burst))), len(burst)-1)], pal.bad, 1-t*t, 4)
			// Sparks thrown back up the lane.
			for _, speed := range []float64{4, 9} {
				g := '•'
				if t > .4 {
					g = '·'
				}
				put(width-2-int(t*speed), g, pal.bad, (1-t)*(1-t), 4)
			}
		}
	}

	var p painter
	for _, c := range cells {
		p.put(c.r, c.c)
	}
	return p.String()
}

// arrow points a lane at its instances and flashes as requests land.
func (tr *traffic) arrow(name string, now time.Time, pal *palette) string {
	c := pal.dim
	if a := tr.arrivals[name]; a != nil && !tr.lite {
		if e := now.Sub(a.lastErr); e >= 0 && e < burstTime {
			c = c.mix(pal.bad, 1-float64(e)/float64(burstTime))
		} else if e := now.Sub(a.last.at); e >= 0 && e < landGlow {
			c = c.mix(statusColor(a.last.status, pal), 1-float64(e)/float64(landGlow))
		}
	}
	var p painter
	p.put('▶', c)
	return " " + p.String() + " "
}

// targets lists where an app's requests landed: each instance (and node)
// with a bar and its rate, lit up for a moment as a request lands on it.
func (tr *traffic) targets(lane string, hits map[string]int, secs float64, width int, now time.Time, pal *palette) string {
	if len(hits) == 0 {
		return sDim.Render("idle")
	}
	names := sortedKeys(hits)
	sort.SliceStable(names, func(i, j int) bool { return hits[names[i]] > hits[names[j]] })
	most := hits[names[0]]
	var parts []string
	used := 0
	for _, n := range names {
		if used > 0 && used+len(n)+16 > width {
			parts = append(parts, sDim.Render(fmt.Sprintf("+%d more", len(names)-len(parts))))
			break
		}
		name := trunc(n, 22)
		if a := tr.arrivals[lane]; a != nil && !tr.lite {
			if l, ok := a.by[n]; ok && now.Sub(l.at) >= 0 && now.Sub(l.at) < landGlow {
				t := 1 - float64(now.Sub(l.at))/float64(landGlow)
				to := pal.flash
				if l.status >= 400 {
					to = statusColor(l.status, pal)
				}
				name = lipgloss.NewStyle().Foreground(lipgloss.Color(pal.fg.mix(to, t).hex())).Bold(t > .5).Render(name)
			}
		}
		barW := max(1, 6*hits[n]/max(most, 1))
		parts = append(parts, fmt.Sprintf("%s %s %.1f/s", name, sGood.Render(strings.Repeat("▮", barW)), float64(hits[n])/secs))
		used += len(n) + 16
	}
	return strings.Join(parts, "  ")
}

// statusColor is a dot's color: a 304 is as good as a 200.
func statusColor(status int, pal *palette) rgb {
	switch {
	case status >= 500:
		return pal.bad
	case status >= 400:
		return pal.warn
	case status >= 300 && status != 304:
		return pal.cyan
	}
	return pal.good
}

func statusText(code int) string {
	s := fmt.Sprintf("%d", code)
	switch {
	case code >= 500:
		return sBad.Render(s)
	case code >= 400:
		return sWarn.Render(s)
	case code >= 300:
		return sCyan.Render(s)
	}
	return sGood.Render(s)
}

func errRate(errs, total int) string {
	if total == 0 {
		return sDim.Render("no errors")
	}
	pct := float64(errs) / float64(total) * 100
	s := fmt.Sprintf("%.1f%% errors", pct)
	if errs > 0 {
		return sBad.Render(s)
	}
	return sGood.Render(s)
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(float64(len(s)-1) * p / 100)
	return s[i]
}

func fmtMS(ms float64) string {
	switch {
	case ms == 0:
		return "-"
	case ms < 10:
		return fmt.Sprintf("%.1fms", ms)
	case ms < 1000:
		return fmt.Sprintf("%.0fms", ms)
	}
	return fmt.Sprintf("%.1fs", ms/1000)
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
