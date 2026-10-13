package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wes/jokku/internal/types"
)

func rules(notFound, rate, burst int, allow ...string) types.ProxyFirewall {
	return types.ProxyFirewall{NotFound: notFound, WindowSec: 600, BanSec: 3600, Rate: rate, Burst: burst, Allow: allow}
}

func newGuard(t *testing.T, p types.ProxyFirewall) *firewall {
	t.Helper()
	r, err := compileFirewall(p)
	if err != nil {
		t.Fatal(err)
	}
	f := newFirewall()
	f.setRules(r)
	return f
}

var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

// scan has addr ask for n pages that aren't there, a second apart from at.
func scan(f *firewall, addr string, n int, at time.Time) {
	for i := range n {
		now := at.Add(time.Duration(i) * time.Second)
		if v, _ := f.admit(ip(addr), "shop", now); v == fwPass {
			f.notFound(ip(addr), "shop", "shop.test", "/.env"+strconv.Itoa(i), now)
		}
	}
}

func TestFirewallBansScanners(t *testing.T) {
	f := newGuard(t, rules(3, 0, 1))
	scan(f, "203.0.113.7", 2, t0)
	if v, _ := f.admit(ip("203.0.113.7"), "shop", t0.Add(time.Minute)); v != fwPass {
		t.Fatalf("two pages not found of three: %v", v)
	}
	scan(f, "203.0.113.7", 1, t0.Add(time.Minute))
	v, wait := f.admit(ip("203.0.113.7"), "other", t0.Add(2*time.Minute))
	if v != fwBanned || wait != 59*time.Minute {
		t.Fatalf("after three: %v, wait %s", v, wait)
	}
	rep := f.snapshot(t0.Add(2 * time.Minute))
	if len(rep.Bans) != 1 {
		t.Fatalf("bans: %+v", rep.Bans)
	}
	b := rep.Bans[0]
	if b.Addr != "203.0.113.7" || b.App != "shop" || b.Host != "shop.test" || b.Blocked != 1 || !b.Until.Equal(t0.Add(time.Minute+time.Hour)) ||
		!slices.Equal(b.Paths, []string{"/.env0", "/.env1", "/.env0"}) {
		t.Errorf("ban: %+v", b)
	}
	// Blocked counts go to the app asked for; the ban to the one that
	// tipped it.
	if rep.Counts["shop"].Bans != 1 || rep.Counts["shop"].NotFound != 3 || rep.Counts["other"].Blocked != 1 || rep.Counts["shop"].Requests != 4 {
		t.Errorf("counts: %+v", rep.Counts)
	}
	if v, _ := f.admit(ip("203.0.113.7"), "shop", t0.Add(time.Minute+time.Hour)); v != fwPass {
		t.Errorf("an hour on, still %v", v)
	}

	// Pages not found ten minutes apart never add up to a ban.
	for i := range 10 {
		scan(f, "203.0.113.8", 1, t0.Add(time.Duration(i)*6*time.Minute))
	}
	if v, _ := f.admit(ip("203.0.113.8"), "shop", t0.Add(time.Hour)); v != fwPass {
		t.Errorf("a slow trickle of pages not found was banned")
	}
}

func TestFirewallLeavesAlone(t *testing.T) {
	f := newGuard(t, rules(2, 1, 1, "198.51.100.0/24", "2001:db8:aa::1"))
	for _, a := range []string{"192.168.86.159", "10.210.1.1", "172.16.0.9", "100.64.1.1", "127.0.0.1", "::1", "fd00::1",
		"::ffff:192.168.1.5", "198.51.100.77", "2001:db8:aa::1"} {
		for i := range 10 {
			v, _ := f.admit(ip(a), "shop", t0.Add(time.Duration(i)*time.Millisecond))
			if v != fwExempt {
				t.Errorf("%s: %v", a, v)
				break
			}
		}
	}
	if v, _ := f.admit(ip("2001:db8:aa::2"), "shop", t0); v != fwPass {
		t.Errorf("an address next to an allowed one: %v", v)
	}
}

func TestFirewallIPv6BansTheSlash64(t *testing.T) {
	f := newGuard(t, rules(3, 0, 1))
	scan(f, "2001:db8:1:2::1", 2, t0)
	scan(f, "2001:db8:1:2:ffff::9", 1, t0.Add(10*time.Second))
	if v, _ := f.admit(ip("2001:db8:1:2:abcd::1"), "shop", t0.Add(time.Minute)); v != fwBanned {
		t.Errorf("another address in the /64: %v", v)
	}
	if v, _ := f.admit(ip("2001:db8:1:3::1"), "shop", t0.Add(time.Minute)); v != fwPass {
		t.Errorf("the next /64: %v", v)
	}
	if rep := f.snapshot(t0.Add(time.Minute)); len(rep.Bans) != 1 || rep.Bans[0].Addr != "2001:db8:1:2::/64" {
		t.Errorf("bans: %+v", rep.Bans)
	}
}

func TestFirewallRate(t *testing.T) {
	f := newGuard(t, rules(20, 10, 5))
	a := ip("203.0.113.9")
	for i := range 5 {
		if v, _ := f.admit(a, "shop", t0); v != fwPass {
			t.Fatalf("request %d of the burst: %v", i+1, v)
		}
	}
	v, wait := f.admit(a, "shop", t0)
	if v != fwLimited || wait != 100*time.Millisecond {
		t.Fatalf("over the burst: %v, wait %s", v, wait)
	}
	// Ten a second refill it, up to the burst.
	for i := range 5 {
		if v, _ := f.admit(a, "shop", t0.Add(time.Second)); v != fwPass {
			t.Fatalf("a second on, request %d: %v", i+1, v)
		}
	}
	if v, _ := f.admit(a, "shop", t0.Add(time.Second)); v != fwLimited {
		t.Errorf("the refill went past the burst")
	}
	rep := f.snapshot(t0.Add(time.Second))
	if rep.Counts["shop"].Limited != 2 || rep.Limited["203.0.113.9"].Limited != 2 || len(rep.Bans) != 0 {
		t.Errorf("report: %+v", rep)
	}
	if rep := f.snapshot(t0.Add(2 * time.Second)); rep.Counts != nil || rep.Limited != nil {
		t.Errorf("a report forgets what the last one said: %+v", rep)
	}
}

func TestFirewallUnbanAndAllow(t *testing.T) {
	p := rules(3, 0, 1)
	f := newGuard(t, p)
	scan(f, "203.0.113.7", 3, t0)
	scan(f, "203.0.113.8", 3, t0)
	// firewall:unban: the ban is lifted, with the pages not found that led
	// to it, whatever the unban's time says: here it's a moment before the
	// ban, as when the unban follows within the second (stored to the
	// second, before v0.8.15) or comes from a clock a little behind.
	p.Unbanned = map[string]time.Time{"203.0.113.7": t0.Add(-time.Second)}
	r, _ := compileFirewall(p)
	f.setRules(r)
	if v, _ := f.admit(ip("203.0.113.7"), "shop", t0.Add(2*time.Minute)); v != fwPass {
		t.Errorf("unbanned: %v", v)
	}
	scan(f, "203.0.113.7", 1, t0.Add(2*time.Minute))
	if v, _ := f.admit(ip("203.0.113.7"), "shop", t0.Add(3*time.Minute)); v != fwPass {
		t.Errorf("one page not found after the unban: %v", v)
	}
	// A ban after the unban stays, however often the rules load again.
	scan(f, "203.0.113.7", 3, t0.Add(3*time.Minute))
	f.setRules(r)
	if v, _ := f.admit(ip("203.0.113.7"), "shop", t0.Add(4*time.Minute)); v != fwBanned {
		t.Errorf("banned again after the unban: %v", v)
	}
	// Allowing a banned address lets it in.
	p.Allow = []string{"203.0.113.8/32"}
	r, _ = compileFirewall(p)
	f.setRules(r)
	if v, _ := f.admit(ip("203.0.113.8"), "shop", t0.Add(4*time.Minute)); v != fwExempt {
		t.Errorf("allowed after its ban: %v", v)
	}
	if rep := f.snapshot(t0.Add(4 * time.Minute)); len(rep.Bans) != 1 || rep.Bans[0].Addr != "203.0.113.7" {
		t.Errorf("bans: %+v", rep.Bans)
	}
}

func TestFirewallWatchesAndForgets(t *testing.T) {
	f := newGuard(t, rules(20, 0, 1))
	scan(f, "203.0.113.1", 5, t0)
	scan(f, "203.0.113.2", 7, t0)
	scan(f, "203.0.113.3", 2, t0) // too few to watch
	rep := f.snapshot(t0.Add(10 * time.Second))
	if len(rep.Watching) != 2 || rep.Watching[0].Addr != "203.0.113.2" || rep.Watching[0].NotFound != 7 || rep.Watching[0].App != "shop" ||
		rep.Watching[1].Addr != "203.0.113.1" {
		t.Errorf("watching: %+v", rep.Watching)
	}
	// Ten minutes on, the pages not found have run out, and once the
	// sweep comes round the addresses are forgotten.
	if rep := f.snapshot(t0.Add(11 * time.Minute)); len(rep.Watching) != 0 {
		t.Errorf("still watching: %+v", rep.Watching)
	}
	f.admit(ip("203.0.113.9"), "shop", t0.Add(12*time.Minute))
	f.mu.Lock()
	n := len(f.clients)
	f.mu.Unlock()
	if n != 1 {
		t.Errorf("%d addresses remembered, want only the new one", n)
	}
}

func TestFirewallKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7": "203.0.113.7", "::ffff:203.0.113.7": "203.0.113.7", "203.0.113.7/32": "203.0.113.7",
		"2001:db8:1:2::5": "2001:db8:1:2::/64", "2001:db8:1:2::/64": "2001:db8:1:2::/64",
	} {
		if got, err := FirewallKey(in); err != nil || got != want {
			t.Errorf("FirewallKey(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"203.0.113.0/24", "2001:db8::/48", "example.com", ""} {
		if got, err := FirewallKey(in); err == nil {
			t.Errorf("FirewallKey(%q) = %q", in, got)
		}
	}
}

// fwControl takes firewall reports the way the control node does.
type fwControl struct {
	mu      sync.Mutex
	reports []types.FirewallReport
}

func (c *fwControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Header.Get("Authorization") != "Bearer node-token":
		w.WriteHeader(http.StatusUnauthorized)
	case r.URL.Path == "/v1/agent/firewall":
		var rep types.FirewallReport
		if json.NewDecoder(r.Body).Decode(&rep) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.reports = append(c.reports, rep)
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/v1/agent/certs/list":
		io.WriteString(w, "[]")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (c *fwControl) bans() []types.FirewallBan {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reports) == 0 {
		return nil
	}
	return c.reports[len(c.reports)-1].Bans
}

// TestFirewallInCaddy: a scanner's pages not found get it banned by the
// address it connects from, whatever X-Forwarded-For it sends; a site's
// own broken links don't count; the ban reaches the control node and the
// access log.
func TestFirewallInCaddy(t *testing.T) {
	defer func(e func(netip.Addr) bool, every time.Duration) { exempt, FirewallReportEvery = e, every }(exempt, FirewallReportEvery)
	exempt, FirewallReportEvery = func(netip.Addr) bool { return false }, 100*time.Millisecond
	guard = newFirewall()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "home")
	}))
	defer backend.Close()
	ctl := &fwControl{}
	srv := httptest.NewUnstartedServer(ctl)
	srv.StartTLS()
	defer srv.Close()
	dir, _ := os.MkdirTemp("", "jkf")
	defer os.RemoveAll(dir)
	logged := watchAccessLog(t)
	port, _ := startProxy(t, Settings{
		Storage:  &ClusterStorage{URL: srv.URL, Token: "node-token", Pin: spkiPin(srv.Certificate()), Cache: filepath.Join(dir, "mirror")},
		Routes:   []Route{{App: "web", Hosts: []string{"web.test"}, Upstreams: []string{strings.TrimPrefix(backend.URL, "http://")}}},
		Firewall: &types.ProxyFirewall{NotFound: 3, WindowSec: 600, BanSec: 3600, Rate: 1000, Burst: 1000},
	})
	get := func(host, path string, header ...string) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(port)+path, nil)
		req.Host = host
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}

	if code, body, _ := get("web.test", "/"); code != 200 || body != "home" {
		t.Fatalf("home page: %d %q", code, body)
	}
	for range 5 {
		if code, _, _ := get("web.test", "/broken.png", "Referer", "https://web.test/about"); code != 404 {
			t.Fatalf("a broken image: %d", code)
		}
	}
	if code, _, _ := get("web.test", "/"); code != 200 {
		t.Fatalf("broken images of the site's own pages got the visitor banned: %d", code)
	}
	// Scanning, claiming to be someone else.
	for _, p := range []string{"/.env", "/.git/config", "/wp-login.php"} {
		if code, _, _ := get("web.test", p, "X-Forwarded-For", "198.51.100.1", "Referer", "https://elsewhere.test/"); code != 404 {
			t.Fatalf("%s: %d", p, code)
		}
	}
	code, body, h := get("web.test", "/", "X-Forwarded-For", "198.51.100.2")
	if code != http.StatusForbidden || !strings.Contains(body, "blocked") || h.Get("Retry-After") != "3600" {
		t.Fatalf("after three pages not found: %d %q Retry-After %q", code, body, h.Get("Retry-After"))
	}
	// Hosts no app serves are behind it too.
	if code, _, _ := get("nobody.test", "/"); code != http.StatusForbidden {
		t.Errorf("a host no app serves: %d", code)
	}

	// A ban is reported at once, then again with what it blocked.
	deadline := time.Now().Add(5 * time.Second)
	for b := ctl.bans(); (len(b) == 0 || b[0].Blocked < 2) && time.Now().Before(deadline); b = ctl.bans() {
		time.Sleep(50 * time.Millisecond)
	}
	bans := ctl.bans()
	if len(bans) != 1 || bans[0].Addr != "127.0.0.1" || bans[0].App != "web" || bans[0].Blocked != 2 ||
		!slices.Equal(bans[0].Paths, []string{"/.env", "/.git/config", "/wp-login.php"}) {
		t.Errorf("the control node was told: %+v", bans)
	}
	var blocked []types.Request
	for _, r := range logged() {
		if r.Firewall != "" {
			blocked = append(blocked, r)
		}
	}
	if len(blocked) != 2 || blocked[0].Firewall != types.FirewallBanned || blocked[0].App != "web" || blocked[0].Status != 403 ||
		blocked[0].Client != "127.0.0.1" {
		t.Errorf("logged as blocked: %+v", blocked)
	}
}

// TestFirewallSkipsRelays: a node takes an edge's word, the edge having
// checked the request.
func TestFirewallSkipsRelays(t *testing.T) {
	defer func(e func(netip.Addr) bool) { exempt = e }(exempt)
	exempt = func(netip.Addr) bool { return false }
	guard = newFirewall()
	backend := httptest.NewServer(http.NotFoundHandler())
	defer backend.Close()
	port, _ := startProxy(t, Settings{
		Edges:    []string{"127.0.0.0/8"},
		Routes:   []Route{{App: "web", Hosts: []string{"web.test"}, Upstreams: []string{strings.TrimPrefix(backend.URL, "http://")}}},
		Firewall: &types.ProxyFirewall{NotFound: 2, WindowSec: 600, BanSec: 3600, Burst: 1},
	})
	for i := range 5 {
		req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(port)+"/.env", nil)
		req.Host = "web.test"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("relayed request %d: %d", i+1, resp.StatusCode)
		}
	}
}
