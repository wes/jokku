package proxy

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/wes/jokku/internal/types"
)

func init() {
	caddy.RegisterModule(FirewallHandler{})
}

// The firewall (types.ProxyFirewall) is a handler at the front of every
// route a visitor reaches: on an edge, or on a node for the apps reached
// straight at it. A node's relay routes skip it, since the edge that sent
// the request checked it. It bans an address that gets too many pages not
// found within a window, the way vulnerability scanners do, and answers
// 429 to one over the rate. Private addresses are never banned nor slowed.
//
// Every route's handler shares one record of the addresses (guard), which
// outlives config reloads, and reports to the control node what it did.

// FirewallReportEvery is how often the firewall reports; a variable so
// tests can shorten it.
var FirewallReportEvery = 10 * time.Second

const (
	// fwMaxClients caps the addresses tracked: past it, new ones go through
	// unchecked rather than the proxy running out of memory.
	fwMaxClients = 200_000
	fwPaths      = 5 // pages not found kept, for the ban's reason
	fwWatchMin   = 3 // pages not found that make an address worth watching
	fwWatchTop   = 10
	fwSweepEvery = time.Minute
)

// FirewallHandler is the firewall in front of one app's routes (App "" for
// hosts no app serves).
type FirewallHandler struct {
	App     string              `json:"app,omitempty"`
	Rules   types.ProxyFirewall `json:"rules"`
	Control *Control            `json:"control,omitempty"` // where reports go; nil for nowhere
}

func (FirewallHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers.jokku_firewall", New: func() caddy.Module { return new(FirewallHandler) }}
}

func (h *FirewallHandler) Provision(caddy.Context) error {
	rules, err := compileFirewall(h.Rules)
	if err != nil {
		return fmt.Errorf("jokku_firewall: %w", err)
	}
	guard.setRules(rules)
	if h.Control != nil {
		guard.reportTo(*h.Control)
	}
	return nil
}

func (h *FirewallHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	addr, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return next.ServeHTTP(w, r)
	}
	verdict, wait := guard.admit(addr, h.App, time.Now())
	switch verdict {
	case fwBanned:
		logFirewall(r, types.FirewallBanned)
		return refuse(w, http.StatusForbidden, wait,
			"This address is blocked for asking for too many pages that don't exist. Try again in "+humanWait(wait)+".\n")
	case fwLimited:
		logFirewall(r, types.FirewallLimited)
		return refuse(w, http.StatusTooManyRequests, wait, "Too many requests from this address: slow down.\n")
	case fwExempt:
		return next.ServeHTTP(w, r)
	}
	rec := caddyhttp.NewResponseRecorder(w, nil, nil)
	err = next.ServeHTTP(rec, r)
	status := rec.Status()
	var he caddyhttp.HandlerError
	if errors.As(err, &he) {
		status = he.StatusCode
	}
	// A page of the site asking for what isn't there (a broken image) is
	// the site's mistake, not the visitor's.
	if status == http.StatusNotFound && !sameSite(r) {
		guard.notFound(addr, h.App, r.Host, requestPath(r), time.Now())
	}
	return err
}

func refuse(w http.ResponseWriter, status int, wait time.Duration, msg string) error {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Retry-After", strconv.Itoa(max(int(wait.Round(time.Second)/time.Second), 1)))
	w.WriteHeader(status)
	_, err := w.Write([]byte(msg))
	return err
}

// logFirewall marks the request's access log line with what the firewall
// did (LogFirewall).
func logFirewall(r *http.Request, what string) {
	if extra, ok := r.Context().Value(caddyhttp.ExtraLogFieldsCtxKey).(*caddyhttp.ExtraLogFields); ok {
		extra.Add(zap.String(LogFirewall, what))
	}
}

func humanWait(d time.Duration) string {
	if m := int(d.Round(time.Minute) / time.Minute); m > 1 {
		return strconv.Itoa(m) + " minutes"
	}
	return "a minute"
}

// sameSite says whether the request came from one of its site's own pages.
func sameSite(r *http.Request) bool {
	ref := r.Header.Get("Referer")
	if ref == "" {
		return false
	}
	u, err := url.Parse(ref)
	if err != nil {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return u.Hostname() != "" && strings.EqualFold(u.Hostname(), host)
}

// requestPath is the page asked for as the visitor sent it, made safe to
// show.
func requestPath(r *http.Request) string {
	p := r.RequestURI
	if p == "" {
		p = r.URL.RequestURI()
	}
	p = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, p)
	if len(p) > 120 {
		p = p[:120] + "…"
	}
	return p
}

// FirewallKey is the address a ban is for: an IPv4 address, or the /64 an
// IPv6 address is in, since anyone with one has the rest of it. It takes
// an address or such a /64.
func FirewallKey(s string) (string, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		if p.Addr().Is4() && p.Bits() == 32 || p.Addr().Is6() && p.Bits() == 64 {
			return firewallKey(p.Addr()), nil
		}
		return "", fmt.Errorf("%s is a range: give an address", s)
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return "", fmt.Errorf("%q isn't an IP address", s)
	}
	return firewallKey(a), nil
}

func firewallKey(a netip.Addr) string {
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

// FirewallExempt says whether the firewall leaves an address alone however
// many requests it makes: a private one (a home network, the mesh, a
// tailnet), or one that never reaches the internet.
func FirewallExempt(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || cgnat.Contains(a)
}

type fwRules struct {
	notFound    int
	window, ban time.Duration
	rate, burst float64
	allow       []netip.Prefix
	unbanned    map[string]time.Time
}

func compileFirewall(p types.ProxyFirewall) (fwRules, error) {
	r := fwRules{notFound: p.NotFound, window: time.Duration(p.WindowSec) * time.Second, ban: time.Duration(p.BanSec) * time.Second,
		rate: float64(p.Rate), burst: float64(max(p.Burst, 1)), unbanned: p.Unbanned}
	if r.notFound < 1 || r.window <= 0 || r.ban <= 0 || r.rate < 0 {
		return r, fmt.Errorf("invalid rules: %+v", p)
	}
	for _, s := range p.Allow {
		pre, err := ParseAllow(s)
		if err != nil {
			return r, err
		}
		r.allow = append(r.allow, pre)
	}
	return r, nil
}

// ParseAllow reads an address or range the firewall lets alone.
func ParseAllow(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q isn't an IP address or range", s)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// guard is this proxy's firewall.
var guard = newFirewall()

type fwVerdict int

const (
	fwPass    fwVerdict = iota // checked and let through
	fwExempt                   // let through unchecked
	fwBanned                   // refused: banned
	fwLimited                  // refused: over the rate
)

type firewall struct {
	mu      sync.Mutex
	rules   fwRules
	ready   bool // rules have been set
	clients map[string]*fwClient
	// applied are the unbans acted on (address -> the unban's time): each
	// lifts the ban its address has when this proxy first hears of it.
	// Comparing a ban's time with the unban's instead would depend on the
	// two machines' clocks, and on the unban being a moment later.
	applied map[string]time.Time
	counts  map[string]*types.FirewallCounts // by app, since the last report
	limited map[string]*types.FirewallSlowed // by address, since the last report
	swept   time.Time

	control *Control
	client  *http.Client
	running bool
	failing bool
	kick    chan struct{}
}

// fwClient is what the firewall knows of one address.
type fwClient struct {
	misses []time.Time // pages not found within the window, oldest first
	paths  []string    // the last of them
	tokens float64     // requests it may make now
	filled time.Time   // when tokens was last topped up
	seen   time.Time
	app    string // of the last page not found
	ban    *types.FirewallBan
}

func newFirewall() *firewall {
	return &firewall{clients: map[string]*fwClient{}, applied: map[string]time.Time{}, counts: map[string]*types.FirewallCounts{},
		limited: map[string]*types.FirewallSlowed{}, kick: make(chan struct{}, 1)}
}

func (f *firewall) setRules(r fwRules) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules, f.ready = r, true
	for k, t := range r.unbanned {
		if f.applied[k].Equal(t) {
			continue
		}
		f.applied[k] = t
		if c := f.clients[k]; c != nil {
			c.ban, c.misses, c.paths = nil, nil, nil
		}
	}
	for k := range f.applied {
		if _, ok := r.unbanned[k]; !ok {
			delete(f.applied, k)
		}
	}
	for k, c := range f.clients {
		if c.ban != nil && f.allowed(keyAddr(k)) {
			c.ban = nil
		}
	}
}

// keyAddr is the address a firewallKey stands for (the start of an IPv6
// /64).
func keyAddr(k string) netip.Addr {
	if p, err := netip.ParsePrefix(k); err == nil {
		return p.Addr()
	}
	a, _ := netip.ParseAddr(k)
	return a
}

// exempt is FirewallExempt; tests, whose requests come from 127.0.0.1,
// change it.
var exempt = FirewallExempt

func (f *firewall) allowed(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || exempt(a) {
		return true
	}
	for _, p := range f.rules.allow {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (f *firewall) count(app string) *types.FirewallCounts {
	c := f.counts[app]
	if c == nil {
		c = &types.FirewallCounts{}
		f.counts[app] = c
	}
	return c
}

// admit decides on a request from addr to app, and how long a refused one
// should wait.
func (f *firewall) admit(addr netip.Addr, app string, now time.Time) (fwVerdict, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ready {
		return fwExempt, 0
	}
	f.sweep(now)
	if f.allowed(addr) {
		f.count(app).Requests++
		return fwExempt, 0
	}
	k := firewallKey(addr)
	c := f.clients[k]
	if c == nil {
		if len(f.clients) >= fwMaxClients {
			f.count(app).Requests++
			return fwExempt, 0
		}
		c = &fwClient{tokens: f.rules.burst, filled: now}
		f.clients[k] = c
	}
	c.seen = now
	if c.ban != nil {
		if now.Before(c.ban.Until) {
			c.ban.Blocked++
			f.count(app).Blocked++
			return fwBanned, c.ban.Until.Sub(now)
		}
		c.ban = nil
	}
	if f.rules.rate > 0 {
		c.tokens = min(f.rules.burst, c.tokens+now.Sub(c.filled).Seconds()*f.rules.rate)
		c.filled = now
		if c.tokens < 1 {
			f.count(app).Limited++
			s := f.limited[k]
			if s == nil {
				s = &types.FirewallSlowed{}
				f.limited[k] = s
			}
			s.App, s.Last = app, now
			s.Limited++
			return fwLimited, time.Duration((1 - c.tokens) / f.rules.rate * float64(time.Second))
		}
		c.tokens--
	}
	f.count(app).Requests++
	return fwPass, 0
}

// notFound counts a page addr asked app for that isn't there, banning it
// at the rules' number.
func (f *firewall) notFound(addr netip.Addr, app, host, path string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count(app).NotFound++
	k := firewallKey(addr)
	c := f.clients[k]
	if c == nil || c.ban != nil {
		return
	}
	c.misses = append(within(c.misses, now.Add(-f.rules.window)), now)
	c.paths, c.app = append(c.paths, path), app
	if len(c.paths) > fwPaths {
		c.paths = c.paths[len(c.paths)-fwPaths:]
	}
	if len(c.misses) < f.rules.notFound {
		return
	}
	c.ban = &types.FirewallBan{Addr: k, App: app, Host: host, Paths: append([]string(nil), c.paths...), At: now, Until: now.Add(f.rules.ban)}
	c.misses, c.paths = nil, nil
	f.count(app).Bans++
	f.reportSoon()
}

// within is ts from since on (ts is in order).
func within(ts []time.Time, since time.Time) []time.Time {
	i := sort.Search(len(ts), func(i int) bool { return !ts[i].Before(since) })
	return append(ts[:0], ts[i:]...)
}

// sweep forgets the addresses with nothing left to remember.
func (f *firewall) sweep(now time.Time) {
	if now.Sub(f.swept) < fwSweepEvery {
		return
	}
	f.swept = now
	idle := max(f.rules.window, time.Duration(f.rules.burst/max(f.rules.rate, 1)*float64(time.Second)))
	for k, c := range f.clients {
		if c.ban != nil && !now.Before(c.ban.Until) {
			c.ban = nil
		}
		if c.ban == nil && now.Sub(c.seen) > idle {
			delete(f.clients, k)
		}
	}
}

// snapshot is a report of what the firewall did since the last one, which
// it then forgets.
func (f *firewall) snapshot(now time.Time) types.FirewallReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	rep := types.FirewallReport{Bans: []types.FirewallBan{}}
	for k, c := range f.clients {
		if c.ban != nil && now.Before(c.ban.Until) {
			b := *c.ban
			b.Paths = append([]string(nil), b.Paths...)
			rep.Bans = append(rep.Bans, b)
			continue
		}
		c.misses = within(c.misses, now.Add(-f.rules.window))
		if len(c.misses) >= fwWatchMin {
			rep.Watching = append(rep.Watching, types.FirewallSuspect{Addr: k, App: c.app, NotFound: len(c.misses), Last: c.misses[len(c.misses)-1]})
		}
	}
	sort.Slice(rep.Bans, func(i, j int) bool { return rep.Bans[i].At.After(rep.Bans[j].At) })
	sort.Slice(rep.Watching, func(i, j int) bool {
		a, b := rep.Watching[i], rep.Watching[j]
		return a.NotFound > b.NotFound || a.NotFound == b.NotFound && a.Addr < b.Addr
	})
	if len(rep.Watching) > fwWatchTop {
		rep.Watching = rep.Watching[:fwWatchTop]
	}
	if len(f.counts) > 0 {
		rep.Counts = map[string]types.FirewallCounts{}
		for app, c := range f.counts {
			rep.Counts[app] = *c
		}
		f.counts = map[string]*types.FirewallCounts{}
	}
	if len(f.limited) > 0 {
		rep.Limited = map[string]types.FirewallSlowed{}
		for k, s := range f.limited {
			rep.Limited[k] = *s
		}
		f.limited = map[string]*types.FirewallSlowed{}
	}
	return rep
}

// unsent puts back the counts of a report that didn't get through, for the
// next one.
func (f *firewall) unsent(rep types.FirewallReport) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for app, c := range rep.Counts {
		f.count(app).Add(c)
	}
	for k, s := range rep.Limited {
		if cur := f.limited[k]; cur != nil {
			cur.Limited += s.Limited
		} else {
			s := s
			f.limited[k] = &s
		}
	}
}

// reportTo sets where reports go, and starts reporting.
func (f *firewall) reportTo(c Control) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.control == nil || *f.control != c {
		f.control, f.client = &c, pinnedClient(c.Pin, 10*time.Second)
	}
	if !f.running {
		f.running = true
		go f.reportLoop()
	}
}

func (f *firewall) reportSoon() {
	select {
	case f.kick <- struct{}{}:
	default:
	}
}

func (f *firewall) reportLoop() {
	t := time.NewTicker(FirewallReportEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-f.kick:
		}
		f.report()
	}
}

func (f *firewall) report() {
	rep := f.snapshot(time.Now())
	f.mu.Lock()
	ctl, client := f.control, f.client
	f.mu.Unlock()
	if ctl == nil {
		return
	}
	err := postAgent(ctl, client, "/v1/agent/firewall", rep)
	if err != nil {
		f.unsent(rep)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil && !f.failing {
		caddy.Log().Named("jokku_firewall").Warn("cannot report to the control node", zap.Error(err))
	}
	f.failing = err != nil
}

var (
	_ caddyhttp.MiddlewareHandler = (*FirewallHandler)(nil)
	_ caddy.Provisioner           = (*FirewallHandler)(nil)
)
