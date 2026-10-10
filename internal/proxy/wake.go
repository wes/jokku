package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"go.uber.org/zap"

	"github.com/wes/jokku/internal/types"
)

func init() {
	caddy.RegisterModule(WakeHandler{})
	caddy.RegisterModule(WakeUpstreams{})
}

// An app that sleeps when idle (ps:set sleep-after) goes through two
// modules. WakeHandler, in front of the reverse proxy, counts the app's
// requests, which this proxy reports to the control node so it knows the
// app is in use; a request that finds the app asleep asks the control node
// to wake it, and waits until the app has instances to go to. WakeUpstreams
// gives the reverse proxy those instances.
//
// Both use one record of each app's instances, shared by every config this
// proxy loads: Caddy keeps an old config's handlers for the requests they
// hold, so a request that arrived while the app slept goes on to the
// instances of the config loaded once it woke.

// Timings; variables so tests can shorten them.
var (
	// WakeTimeout is how long a request waits for an app to wake before it
	// gets the unreachable page.
	WakeTimeout = 60 * time.Second
	// ActivityEvery is how often the apps used are reported.
	ActivityEvery = 10 * time.Second
	// askAgain is how often a waiting request repeats its wake-up call,
	// in case one was lost.
	askAgain = 5 * time.Second
)

// Control is the control node's API, reached as this node.
type Control struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	Pin   string `json:"pin"`
}

// WakeHandler wakes a sleeping app for a request, and reports its use.
type WakeHandler struct {
	App     string   `json:"app"`
	Control *Control `json:"control,omitempty"` // nil: no one to wake it
}

func (WakeHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers.jokku_wake", New: func() caddy.Module { return new(WakeHandler) }}
}

func (h *WakeHandler) Provision(caddy.Context) error {
	if h.App == "" {
		return errors.New("jokku_wake: no app")
	}
	if h.Control != nil {
		sleepers.reportTo(*h.Control)
	}
	return nil
}

func (h *WakeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	defer sleepers.begin(h.App)()
	if err := sleepers.awake(r.Context(), h.App); err != nil {
		return caddyhttp.Error(http.StatusServiceUnavailable, err)
	}
	return next.ServeHTTP(w, r)
}

// WakeUpstreams is a sleeping app's reverse proxy's upstream source: its
// instances in the newest config.
type WakeUpstreams struct {
	App       string   `json:"app"`
	Upstreams []string `json:"upstreams"`
}

func (WakeUpstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.reverse_proxy.upstreams.jokku", New: func() caddy.Module { return new(WakeUpstreams) }}
}

func (u *WakeUpstreams) Provision(caddy.Context) error {
	if u.App == "" {
		return errors.New("jokku upstreams: no app")
	}
	sleepers.setUpstreams(u.App, u.Upstreams)
	return nil
}

func (u *WakeUpstreams) GetUpstreams(*http.Request) ([]*reverseproxy.Upstream, error) {
	addrs := sleepers.upstreams(u.App)
	out := make([]*reverseproxy.Upstream, len(addrs))
	for i, a := range addrs {
		out[i] = &reverseproxy.Upstream{Dial: a}
	}
	return out, nil
}

// sleepers is what this proxy knows of apps that sleep.
var sleepers = newSleepState()

type sleepState struct {
	mu      sync.Mutex
	ups     map[string][]string  // app -> its instances in the newest config
	changed chan struct{}        // closed when ups changes
	open    map[string]int       // app -> requests in progress
	used    map[string]bool      // apps with requests since the last report
	wake    map[string]bool      // apps to wake, not reported yet
	asked   map[string]time.Time // app -> when it was last asked to wake
	control *Control
	client  *http.Client
	running bool
	failing bool // the last report failed (logged once)
	kick    chan struct{}
}

func newSleepState() *sleepState {
	return &sleepState{
		ups: map[string][]string{}, changed: make(chan struct{}), open: map[string]int{},
		used: map[string]bool{}, wake: map[string]bool{}, asked: map[string]time.Time{}, kick: make(chan struct{}, 1),
	}
}

func (s *sleepState) setUpstreams(app string, ups []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Equal(s.ups[app], ups) {
		return
	}
	s.ups[app] = append([]string(nil), ups...)
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *sleepState) upstreams(app string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ups[app]
}

// begin counts a request to app until the returned func is called.
func (s *sleepState) begin(app string) func() {
	s.mu.Lock()
	s.open[app]++
	s.used[app] = true
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if s.open[app]--; s.open[app] <= 0 {
			delete(s.open, app)
		}
		s.mu.Unlock()
	}
}

// awake returns once app has instances to go to: right away unless it is
// asleep, otherwise once the control node has woken it and this proxy has
// its instances.
func (s *sleepState) awake(ctx context.Context, app string) error {
	deadline := time.NewTimer(WakeTimeout)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		if len(s.ups[app]) > 0 {
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		ask := time.Since(s.asked[app]) >= askAgain
		if ask {
			s.asked[app] = time.Now()
			s.wake[app] = true
		}
		s.mu.Unlock()
		if ask {
			s.reportSoon()
		}
		select {
		case <-changed:
		case <-time.After(askAgain):
		case <-deadline.C:
			return fmt.Errorf("%s did not wake up within %s", app, WakeTimeout)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// reportTo sets where reports go, and starts reporting.
func (s *sleepState) reportTo(c Control) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.control == nil || *s.control != c {
		s.control, s.client = &c, pinnedClient(c.Pin, 10*time.Second)
	}
	if !s.running {
		s.running = true
		go s.reportLoop()
	}
}

func (s *sleepState) reportSoon() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *sleepState) reportLoop() {
	t := time.NewTicker(ActivityEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-s.kick:
		}
		s.report()
	}
}

// report tells the control node which apps were used since the last
// report, or still have requests open, and which to wake. What it can't
// deliver goes with the next one.
func (s *sleepState) report() {
	s.mu.Lock()
	var act types.ProxyActivity
	for app, n := range s.open {
		if n > 0 {
			s.used[app] = true
		}
	}
	for app := range s.used {
		act.Active = append(act.Active, app)
	}
	for app := range s.wake {
		act.Wake = append(act.Wake, app)
	}
	s.used, s.wake = map[string]bool{}, map[string]bool{}
	ctl, client := s.control, s.client
	s.mu.Unlock()
	if ctl == nil || len(act.Active)+len(act.Wake) == 0 {
		return
	}
	sort.Strings(act.Active)
	sort.Strings(act.Wake)
	err := postActivity(ctl, client, act)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		for _, app := range act.Active {
			s.used[app] = true
		}
		for _, app := range act.Wake {
			s.wake[app] = true
		}
		if !s.failing {
			caddy.Log().Named("jokku_wake").Warn("cannot report to the control node; sleeping apps cannot wake until it's back", zap.Error(err))
		}
	}
	s.failing = err != nil
}

func postActivity(ctl *Control, client *http.Client, act types.ProxyActivity) error {
	b, err := json.Marshal(act)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(ctl.URL, "/")+"/v1/agent/activity", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+ctl.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

var (
	_ caddyhttp.MiddlewareHandler = (*WakeHandler)(nil)
	_ caddy.Provisioner           = (*WakeHandler)(nil)
	_ reverseproxy.UpstreamSource = (*WakeUpstreams)(nil)
	_ caddy.Provisioner           = (*WakeUpstreams)(nil)
)
