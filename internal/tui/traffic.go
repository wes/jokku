package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wes/jokku/internal/types"
)

// The traffic view: every request the cluster's proxies handle flies across
// its app's lane as a dot (colored by status) and lands on the instance that
// answered, with live rates and latencies around it.

const (
	travelTime   = 1500 * time.Millisecond // a dot's trip across its lane
	frameEvery   = 80 * time.Millisecond
	statsWindow  = 10 * time.Second
	maxParticles = 80 // per lane; beyond that requests are counted, not drawn
	keepRecent   = 300
)

type particle struct {
	status int
	born   time.Time
}

type traffic struct {
	cancel    context.CancelFunc
	ch        chan types.Request
	end       chan error
	err       error
	connected bool
	paused    bool
	lanes     map[string][]particle // app -> dots in flight
	recent    []types.Request       // newest last
	window    []types.Request       // the last statsWindow, for rates and latencies
	total     int
}

type requestsMsg []types.Request
type trafficEndMsg struct{ err error }
type frameMsg struct{}
type trafficRetryMsg struct{}

func frame() tea.Cmd { return tea.Tick(frameEvery, func(time.Time) tea.Msg { return frameMsg{} }) }

// startTraffic opens the request stream; it runs only while the view is
// open.
func (m *model) startTraffic() tea.Cmd {
	if m.tr == nil {
		m.tr = &traffic{lanes: map[string][]particle{}}
	}
	if m.tr.cancel != nil {
		return nil
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
	return tea.Batch(waitRequests(ch, end), frame())
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
		now := time.Now()
		for _, r := range msg {
			tr.total++
			tr.recent = append(tr.recent, r)
			tr.window = append(tr.window, r)
			// History from before the view opened is listed, not animated.
			if !tr.paused && now.Sub(r.At) < travelTime {
				lane := laneName(r)
				if len(tr.lanes[lane]) < maxParticles {
					tr.lanes[lane] = append(tr.lanes[lane], particle{status: r.Status, born: now})
				}
			}
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
			return nil
		}
		now := time.Now()
		for lane, ps := range tr.lanes {
			kept := ps[:0]
			for _, p := range ps {
				if now.Sub(p.born) < travelTime {
					kept = append(kept, p)
				}
			}
			tr.lanes[lane] = kept
		}
		cut := 0
		for cut < len(tr.window) && now.Sub(tr.window[cut].At) > statsWindow {
			cut++
		}
		tr.window = tr.window[cut:]
		return frame()
	}
	return nil
}

func laneName(r types.Request) string {
	if r.App == "" {
		return "(no app)"
	}
	return r.App
}

func (m *model) trafficKey(k string) bool {
	if m.view != trafficView || m.tr == nil {
		return false
	}
	switch k {
	case "p":
		m.tr.paused = !m.tr.paused
		return true
	case "c":
		m.tr.lanes, m.tr.recent, m.tr.window, m.tr.total = map[string][]particle{}, nil, nil, 0
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
	now := time.Now()

	// Headline numbers over the last statsWindow.
	var lat []float64
	errs, byApp, byTarget, byNode := 0, map[string]int{}, map[string]map[string]int{}, map[string]int{}
	for _, r := range tr.window {
		lat = append(lat, r.DurationMS)
		if r.Status >= 500 {
			errs++
		}
		lane := laneName(r)
		byApp[lane]++
		target := r.Instance
		switch {
		case target != "":
			target += "@" + r.InstanceNode
		case r.Status == 404 && r.App == "":
			target = "404 unknown host"
		case r.App != "":
			target = fmt.Sprintf("%d (no instance)", r.Status)
		}
		if byTarget[lane] == nil {
			byTarget[lane] = map[string]int{}
		}
		byTarget[lane][target]++
		byNode[r.Node]++
	}
	secs := statsWindow.Seconds()
	rate := float64(len(tr.window)) / secs
	headline := fmt.Sprintf("  %s  ·  p50 %s  p95 %s  ·  %s  ·  %d total",
		sTitle.Render(fmt.Sprintf("%.1f req/s", rate)), fmtMS(percentile(lat, 50)), fmtMS(percentile(lat, 95)),
		errRate(errs, len(tr.window)), tr.total)
	switch {
	case tr.err != nil && !tr.connected:
		headline += "  ·  " + sBad.Render("reconnecting: "+tr.err.Error())
	case tr.paused:
		headline += "  ·  " + sWarn.Render("paused (p)")
	}
	b.WriteString(headline + "\n\n")

	// One lane per app: known apps first, then any other host seen.
	lanes := map[string]bool{}
	if m.st != nil {
		for _, a := range m.st.Apps {
			lanes[a.Name] = true
		}
	}
	for l := range byApp {
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
		label := fmt.Sprintf("  %-*s %5.1f/s ", labelW, trunc(name, labelW), float64(byApp[name])/secs)
		b.WriteString(label + m.lane(tr.lanes[name], laneW, now) + sDim.Render(" ▶ ") + targets(byTarget[name], secs, rightW) + "\n")
	}

	// Which nodes' proxies receive the traffic.
	if len(byNode) > 0 {
		var parts []string
		for _, n := range sortedKeys(byNode) {
			parts = append(parts, fmt.Sprintf("%s %s", n, sGood.Render(fmt.Sprintf("%.1f/s", float64(byNode[n])/secs))))
		}
		b.WriteString("\n  " + sHead.Render("requests by node  ") + strings.Join(parts, sDim.Render("  ·  ")) + "\n")
	}

	// The latest requests.
	b.WriteString("\n" + sSection.Render("Recent requests") + "\n")
	rows := max(m.height-len(names)-14, 3)
	start := max(len(tr.recent)-rows, 0)
	if len(tr.recent) == 0 {
		b.WriteString(sDim.Render("  waiting for requests...") + "\n")
	}
	for i := len(tr.recent) - 1; i >= start; i-- {
		r := tr.recent[i]
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

// lane draws one app's track with its requests in flight.
func (m *model) lane(ps []particle, width int, now time.Time) string {
	cells := make([]int, width) // 0 empty, else the status of the dot there
	for _, p := range ps {
		x := int(float64(width-1) * float64(now.Sub(p.born)) / float64(travelTime))
		if x >= 0 && x < width && (cells[x] == 0 || p.status >= 500) {
			cells[x] = p.status
		}
	}
	var b, run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			b.WriteString(sDim.Render(run.String()))
			run.Reset()
		}
	}
	for _, st := range cells {
		if st == 0 {
			run.WriteString("┄")
			continue
		}
		flush()
		b.WriteString(dot(st))
	}
	flush()
	return b.String()
}

func dot(status int) string {
	switch {
	case status >= 500:
		return sBad.Render("✖")
	case status >= 400:
		return sWarn.Render("●")
	case status >= 300:
		return sTitle.Render("●")
	}
	return sGood.Render("●")
}

// targets lists where an app's requests landed: each instance (and node)
// with a bar and its rate.
func targets(hits map[string]int, secs float64, width int) string {
	if len(hits) == 0 {
		return sDim.Render("idle")
	}
	names := sortedKeys(hits)
	sort.SliceStable(names, func(i, j int) bool { return hits[names[i]] > hits[names[j]] })
	most := hits[names[0]]
	var parts []string
	used := 0
	for _, n := range names {
		barW := max(1, 6*hits[n]/max(most, 1))
		part := fmt.Sprintf("%s %s %.1f/s", trunc(n, 22), sGood.Render(strings.Repeat("▮", barW)), float64(hits[n])/secs)
		if used > 0 && used+len(n)+16 > width {
			parts = append(parts, sDim.Render(fmt.Sprintf("+%d more", len(names)-len(parts))))
			break
		}
		parts = append(parts, part)
		used += len(n) + 16
	}
	return strings.Join(parts, "  ")
}

func statusText(code int) string {
	s := fmt.Sprintf("%d", code)
	switch {
	case code >= 500:
		return sBad.Render(s)
	case code >= 400:
		return sWarn.Render(s)
	case code >= 300:
		return sTitle.Render(s)
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
