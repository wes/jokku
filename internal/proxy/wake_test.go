package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/wes/jokku/internal/types"
)

// fakeControl answers a proxy the way the control node does: it takes
// activity reports, and when asked to wake an app it "starts" it by loading
// a config that routes to its instance, as the node's agent would.
type fakeControl struct {
	mu     sync.Mutex
	active map[string]bool
	wakes  []string
	wake   func(app string) // nil: never wakes anything
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer node-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/agent/activity":
		var act types.ProxyActivity
		if err := json.NewDecoder(r.Body).Decode(&act); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		for _, app := range act.Active {
			f.active[app] = true
		}
		f.wakes = append(f.wakes, act.Wake...)
		f.mu.Unlock()
		for _, app := range act.Wake {
			if f.wake != nil {
				go f.wake(app)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	// The certificate store, which Caddy may look in: empty.
	case r.URL.Path == "/v1/agent/certs/list":
		io.WriteString(w, "[]")
	case r.URL.Path == "/v1/agent/certs/lock" || r.Method == http.MethodPut || r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeControl) saw() (active, wakes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for app := range f.active {
		active = append(active, app)
	}
	slices.Sort(active)
	return active, append([]string(nil), f.wakes...)
}

// TestSleepingAppWakesForARequest runs the embedded Caddy with a route to a
// sleeping app: a request asks the control node to wake it, waits, and is
// answered by the instance the next config routes to.
func TestSleepingAppWakesForARequest(t *testing.T) {
	defer func(timeout, every time.Duration) { WakeTimeout, ActivityEvery = timeout, every }(WakeTimeout, ActivityEvery)
	WakeTimeout, ActivityEvery = 2*time.Second, 200*time.Millisecond

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from "+r.Host)
	}))
	defer backend.Close()
	dir, err := os.MkdirTemp("", "jkw") // unix socket paths must stay short
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	httpPort := freePort(t)
	admin := filepath.Join(dir, "admin.sock")

	ctl := &fakeControl{active: map[string]bool{}}
	srv := httptest.NewUnstartedServer(ctl)
	srv.StartTLS()
	defer srv.Close()
	settings := Settings{
		DataDir: dir, HTTPPort: httpPort, HTTPSPort: freePort(t), AdminSocket: admin,
		Storage: &ClusterStorage{URL: srv.URL, Token: "node-token", Pin: spkiPin(srv.Certificate()), Cache: filepath.Join(dir, "mirror")},
		Routes: []Route{
			{App: "web", Hosts: []string{"web.test"}, Sleeps: true},
			{App: "late", Hosts: []string{"late.test"}, Sleeps: true}, // never wakes
			{App: "plain", Hosts: []string{"plain.test"}, Upstreams: []string{strings.TrimPrefix(backend.URL, "http://")}},
		},
	}
	var woke sync.Once
	ctl.wake = func(app string) {
		if app != "web" {
			return
		}
		woke.Do(func() {
			time.Sleep(300 * time.Millisecond) // the instance boots
			s := settings
			s.Routes = slices.Clone(settings.Routes)
			s.Routes[0].Upstreams = []string{strings.TrimPrefix(backend.URL, "http://")}
			cfg, err := Config(s)
			if err == nil {
				err = Load(context.Background(), admin, dir, cfg)
			}
			if err != nil {
				t.Errorf("loading the woken config: %v", err)
			}
		})
	}

	cfg, err := Config(Settings{DataDir: dir, HTTPPort: httpPort, HTTPSPort: settings.HTTPSPort, AdminSocket: admin})
	if err != nil {
		t.Fatal(err)
	}
	if err := caddy.Load(cfg, true); err != nil {
		t.Fatal(err)
	}
	defer caddy.Stop()
	if cfg, err = Config(settings); err != nil {
		t.Fatal(err)
	}
	if err := Load(context.Background(), admin, dir, cfg); err != nil {
		t.Fatal(err)
	}

	get := func(host string) (int, string, time.Duration) {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(httpPort)+"/", nil)
		req.Host = host
		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), time.Since(start)
	}

	code, body, took := get("web.test")
	if code != 200 || body != "hello from web.test" {
		t.Fatalf("request to a sleeping app: %d %q", code, body)
	}
	if took < 300*time.Millisecond {
		t.Errorf("answered in %s, before the app woke", took)
	}
	if _, wakes := ctl.saw(); !slices.Contains(wakes, "web") {
		t.Errorf("the control node was not asked to wake web: %v", wakes)
	}
	// Awake now: no waiting.
	if code, _, took := get("web.test"); code != 200 || took > 200*time.Millisecond {
		t.Errorf("request to the woken app: %d in %s", code, took)
	}
	if code, _, _ := get("plain.test"); code != 200 {
		t.Errorf("an app that doesn't sleep: %d", code)
	}

	// An app that doesn't wake in time gets the unreachable page.
	code, body, took = get("late.test")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "reach this site") {
		t.Errorf("app that never wakes: %d %q", code, body)
	}
	if took < WakeTimeout || took > WakeTimeout+2*time.Second {
		t.Errorf("app that never wakes answered after %s, want about %s", took, WakeTimeout)
	}

	// Both were reported as used; the app that doesn't sleep wasn't.
	deadline := time.Now().Add(3 * time.Second)
	for {
		active, _ := ctl.saw()
		if fmt.Sprint(active) == "[late web]" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("apps reported as used: %v, want [late web]", active)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestOpenRequestsKeepAnAppInUse: a long request (a websocket, say) is
// reported on every report while it lasts, and a report that fails is
// sent again with the next.
func TestOpenRequestsKeepAnAppInUse(t *testing.T) {
	ctl := &fakeControl{active: map[string]bool{}}
	srv := httptest.NewUnstartedServer(ctl)
	srv.StartTLS()
	defer srv.Close()
	s := newSleepState()
	s.control = &Control{URL: srv.URL, Token: "node-token", Pin: spkiPin(srv.Certificate())}
	s.client = pinnedClient(s.control.Pin, 5*time.Second)
	reported := func() []string {
		ctl.mu.Lock()
		defer ctl.mu.Unlock()
		var out []string
		for app := range ctl.active {
			out = append(out, app)
		}
		slices.Sort(out)
		ctl.active = map[string]bool{}
		return out
	}

	done := s.begin("chat")
	s.begin("blog")()
	s.report()
	if got := reported(); !slices.Equal(got, []string{"blog", "chat"}) {
		t.Errorf("first report: %v", got)
	}
	s.report()
	if got := reported(); !slices.Equal(got, []string{"chat"}) {
		t.Errorf("while chat's request is open: %v", got)
	}
	done()
	s.report()
	if got := reported(); len(got) != 0 {
		t.Errorf("once it's done: %v", got)
	}

	// The control node is unreachable: what it missed goes next time.
	s.begin("blog")()
	s.mu.Lock()
	s.wake["blog"] = true
	good := s.control.Token
	s.control.Token = "wrong"
	s.mu.Unlock()
	s.report()
	if got := reported(); len(got) != 0 {
		t.Fatalf("a refused report counted: %v", got)
	}
	s.mu.Lock()
	s.control.Token = good
	s.mu.Unlock()
	s.report()
	if got := reported(); !slices.Equal(got, []string{"blog"}) {
		t.Errorf("after the failed report: %v", got)
	}
	if _, wakes := ctl.saw(); !slices.Equal(wakes, []string{"blog"}) {
		t.Errorf("wakes after the failed report: %v", wakes)
	}
}
