package cluster

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wes/jokku/internal/proxy"
	"github.com/wes/jokku/internal/types"
)

// The firewall's settings are the "firewall" properties: the rules and the
// addresses let alone for every app (app ""), and "enabled" per app too.
// The proxies apply them (proxy.FirewallHandler) and report what they did,
// which the control node keeps in memory: the bans each has now, and the
// counts of the last minute, hour and day, starting over when it restarts.

// FirewallPlugin is the properties plugin of the firewall's settings.
const FirewallPlugin = "firewall"

const (
	fwRecent    = 5 * time.Minute // reports kept, for the last minute
	fwStale     = 2 * time.Minute // a proxy that hasn't reported for this long has stopped
	fwEventSoon = 2 * time.Minute // a ban older than this when first heard of was heard of before a restart
	fwTop       = 10
)

// Firewall setting limits.
var (
	fwMinWindow, fwMaxWindow = time.Minute, 24 * time.Hour
	fwMinBan, fwMaxBan       = time.Minute, 30 * 24 * time.Hour
)

type fwTracker struct {
	mu      sync.Mutex
	nodes   map[string]fwNodeView
	recent  []fwPoint
	minutes map[int64]map[string]*types.FirewallCounts // Unix minute -> app -> counts, the last day
	slowed  map[string]*types.FirewallSlowed           // address -> over the rate, the last hour
	evented map[string]time.Time                       // bans told of in an event -> until
}

type fwNodeView struct {
	at       time.Time
	bans     []types.FirewallBan
	watching []types.FirewallSuspect
}

type fwPoint struct {
	at     time.Time
	counts map[string]types.FirewallCounts
}

// FirewallSettings reads the firewall's settings.
func (c *Controller) FirewallSettings(ctx context.Context) (types.FirewallSettings, error) {
	s, err := c.firewallRules(ctx)
	if err != nil {
		return s, err
	}
	apps, err := c.Store.Apps(ctx)
	if err != nil {
		return s, err
	}
	for _, a := range apps {
		if off, err := c.firewallOff(ctx, a.Name); err != nil {
			return s, err
		} else if off {
			s.OffForApps = append(s.OffForApps, a.Name)
		}
	}
	return s, nil
}

// firewallRules is the firewall's settings for every app.
func (c *Controller) firewallRules(ctx context.Context) (types.FirewallSettings, error) {
	p, err := c.Store.Properties(ctx, "", FirewallPlugin)
	if err != nil {
		return types.FirewallSettings{}, err
	}
	return types.FirewallSettings{Enabled: p["enabled"] != "false",
		NotFound: intOr(p["not-found"], types.DefaultFirewallNotFound),
		Window:   shortDuration(durationOr(p["window"], types.DefaultFirewallWindow)),
		Ban:      shortDuration(durationOr(p["ban"], types.DefaultFirewallBan)),
		Rate:     intOr(p["rate"], types.DefaultFirewallRate), Burst: intOr(p["burst"], types.DefaultFirewallBurst),
		Allow: SplitList(p["allow"])}, nil
}

func intOr(v string, def int) int {
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

func durationOr(v string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return def
}

// proxyFirewall is the firewall's rules for the proxies; nil when it's off.
func (c *Controller) proxyFirewall(ctx context.Context) (*types.ProxyFirewall, error) {
	p, err := c.Store.Properties(ctx, "", FirewallPlugin)
	if err != nil || p["enabled"] == "false" {
		return nil, err
	}
	ban := durationOr(p["ban"], types.DefaultFirewallBan)
	fw := &types.ProxyFirewall{NotFound: intOr(p["not-found"], types.DefaultFirewallNotFound),
		WindowSec: int(durationOr(p["window"], types.DefaultFirewallWindow) / time.Second), BanSec: int(ban / time.Second),
		Rate: intOr(p["rate"], types.DefaultFirewallRate), Burst: intOr(p["burst"], types.DefaultFirewallBurst), Allow: SplitList(p["allow"])}
	// Only unbans younger than a ban matter: older bans have ended.
	for addr, at := range unbanned(p["unbanned"]) {
		if time.Since(at) < ban {
			if fw.Unbanned == nil {
				fw.Unbanned = map[string]time.Time{}
			}
			fw.Unbanned[addr] = at
		}
	}
	return fw, nil
}

// unbanned reads the "unbanned" property: address=Unix seconds, ...
func unbanned(v string) map[string]time.Time {
	out := map[string]time.Time{}
	for _, item := range SplitList(v) {
		addr, at, ok := strings.Cut(item, "=")
		if sec, err := strconv.ParseInt(at, 10, 64); ok && err == nil {
			out[addr] = time.Unix(sec, 0).UTC()
		}
	}
	return out
}

// firewallOff says whether an app opted out of the firewall.
func (c *Controller) firewallOff(ctx context.Context, app string) (bool, error) {
	p, err := c.Store.Properties(ctx, app, FirewallPlugin)
	return p["enabled"] == "false", err
}

// SetFirewall changes the firewall's settings, or with an app whether it
// is on for it.
func (c *Controller) SetFirewall(ctx context.Context, app string, p types.FirewallPatch) error {
	values := map[string]string{}
	if p.Enabled != nil {
		values["enabled"] = map[bool]string{true: "", false: "false"}[*p.Enabled]
	}
	if app != "" {
		if p.NotFound != nil || p.Window != nil || p.Ban != nil || p.Rate != nil || p.Burst != nil || p.Allow != nil {
			return badSetting("the firewall's rules are the same for every app: set them without an app")
		}
		if _, err := c.Store.App(ctx, app); err != nil {
			return err
		}
	}
	if p.NotFound != nil {
		if *p.NotFound < 1 || *p.NotFound > 100_000 {
			return badSetting("not-found is a number of pages, from 1")
		}
		values["not-found"] = strconv.Itoa(*p.NotFound)
	}
	for _, d := range []struct {
		key      string
		v        *string
		min, max time.Duration
	}{{"window", p.Window, fwMinWindow, fwMaxWindow}, {"ban", p.Ban, fwMinBan, fwMaxBan}} {
		if d.v == nil {
			continue
		}
		v, err := time.ParseDuration(*d.v)
		if err != nil || v < d.min || v > d.max {
			return badSetting("%s is a duration from %s to %s, such as 10m or 1h", d.key, shortDuration(d.min), shortDuration(d.max))
		}
		values[d.key] = shortDuration(v)
	}
	if p.Rate != nil {
		if *p.Rate < 0 {
			return badSetting("rate is requests a second, 0 for no limit")
		}
		values["rate"] = strconv.Itoa(*p.Rate)
	}
	if p.Burst != nil {
		if *p.Burst < 1 {
			return badSetting("burst is a number of requests, from 1")
		}
		values["burst"] = strconv.Itoa(*p.Burst)
	}
	if p.Allow != nil {
		var allow []string
		for _, a := range *p.Allow {
			pre, err := proxy.ParseAllow(strings.TrimSpace(a))
			if err != nil {
				return badSetting("%v", err)
			}
			s := pre.String()
			if pre.IsSingleIP() {
				s = pre.Addr().String()
			}
			if !slices.Contains(allow, s) {
				allow = append(allow, s)
			}
		}
		values["allow"] = strings.Join(allow, ",")
	}
	if len(values) == 0 {
		return nil
	}
	if err := c.Store.SetProperties(ctx, app, FirewallPlugin, values); err != nil {
		return err
	}
	c.Changed()
	return nil
}

// BadSetting is a firewall setting out of bounds.
type BadSetting struct{ msg string }

func (e *BadSetting) Error() string { return e.msg }

func badSetting(format string, args ...any) error { return &BadSetting{fmt.Sprintf(format, args...)} }

// Unban lifts addr's bans on every proxy, reporting whether it had one.
func (c *Controller) Unban(ctx context.Context, addr string) (bool, error) {
	key, err := proxy.FirewallKey(addr)
	if err != nil {
		return false, badSetting("%v", err)
	}
	p, err := c.Store.Properties(ctx, "", FirewallPlugin)
	if err != nil {
		return false, err
	}
	ban := durationOr(p["ban"], types.DefaultFirewallBan)
	now := time.Now()
	list := unbanned(p["unbanned"])
	list[key] = now
	var items []string
	for a, at := range list {
		if now.Sub(at) < ban {
			items = append(items, a+"="+strconv.FormatInt(at.Unix(), 10))
		}
	}
	sort.Strings(items)
	if err := c.Store.SetProperty(ctx, "", FirewallPlugin, "unbanned", strings.Join(items, ",")); err != nil {
		return false, err
	}
	had := c.fw.forget(key)
	if had {
		c.Store.AddEvent(ctx, "firewall", "", "", "unbanned %s", key)
	}
	c.Changed()
	return had, nil
}

// forget drops key's bans until the proxies say otherwise.
func (t *fwTracker) forget(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	had := false
	for n, v := range t.nodes {
		v.bans = slices.DeleteFunc(slices.Clone(v.bans), func(b types.FirewallBan) bool {
			if b.Addr == key {
				had = true
				return true
			}
			return false
		})
		t.nodes[n] = v
	}
	return had
}

// FirewallReport takes in what a node's proxy says it did.
func (c *Controller) FirewallReport(ctx context.Context, node string, rep types.FirewallReport) error {
	s, err := c.firewallRules(ctx)
	if err != nil {
		return err
	}
	if !s.Enabled {
		// A proxy remembers its bans, which it no longer applies.
		rep.Bans, rep.Watching = nil, nil
	}
	for _, b := range c.fw.record(node, rep, time.Now()) {
		paths := ""
		if len(b.Paths) > 0 {
			paths = ": " + strings.Join(b.Paths, " ")
		}
		c.Store.AddEvent(ctx, "firewall", b.App, node, "banned %s for %s: %d pages not found within %s%s",
			b.Addr, s.Ban, s.NotFound, s.Window, paths)
	}
	return nil
}

// record keeps rep, returning the bans not heard of before.
func (t *fwTracker) record(node string, rep types.FirewallReport, now time.Time) []types.FirewallBan {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.nodes == nil {
		t.nodes, t.minutes, t.slowed, t.evented = map[string]fwNodeView{}, map[int64]map[string]*types.FirewallCounts{},
			map[string]*types.FirewallSlowed{}, map[string]time.Time{}
	}
	t.nodes[node] = fwNodeView{at: now, bans: rep.Bans, watching: rep.Watching}
	var fresh []types.FirewallBan
	for _, b := range rep.Bans {
		key := node + " " + b.Addr + " " + b.At.Format(time.RFC3339Nano)
		if _, ok := t.evented[key]; !ok {
			t.evented[key] = b.Until
			if now.Sub(b.At) < fwEventSoon {
				fresh = append(fresh, b)
			}
		}
	}
	if len(rep.Counts) > 0 {
		t.recent = append(t.recent, fwPoint{at: now, counts: rep.Counts})
		m := now.Unix() / 60
		slot := t.minutes[m]
		if slot == nil {
			slot = map[string]*types.FirewallCounts{}
			t.minutes[m] = slot
		}
		for app, c := range rep.Counts {
			if slot[app] == nil {
				slot[app] = &types.FirewallCounts{}
			}
			slot[app].Add(c)
		}
	}
	for addr, s := range rep.Limited {
		cur := t.slowed[addr]
		if cur == nil {
			cur = &types.FirewallSlowed{}
			t.slowed[addr] = cur
		}
		cur.App, cur.Last = s.App, s.Last
		cur.Limited += s.Limited
	}
	// Forget what's past.
	t.recent = slices.DeleteFunc(t.recent, func(p fwPoint) bool { return now.Sub(p.at) > fwRecent })
	for m := range t.minutes {
		if now.Unix()/60-m >= 24*60 {
			delete(t.minutes, m)
		}
	}
	for addr, s := range t.slowed {
		if now.Sub(s.Last) > time.Hour {
			delete(t.slowed, addr)
		}
	}
	for key, until := range t.evented {
		if now.After(until) {
			delete(t.evented, key)
		}
	}
	return fresh
}

// Firewall is what the firewall is doing: its settings, the bans the
// proxies have, and their counts.
func (c *Controller) Firewall(ctx context.Context) (*types.Firewall, error) {
	s, err := c.FirewallSettings(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	fw := c.fw.view(now)
	fw.Settings = s
	for _, n := range nodes {
		if (n.Edge() || n.Ingress) && n.Ready(now) && !n.Has(types.FeatureFirewall) {
			fw.Missing = append(fw.Missing, n.Name)
		}
	}
	if !s.Enabled {
		fw.Bans, fw.Watching = []types.FirewallBan{}, nil
	}
	return fw, nil
}

func (t *fwTracker) view(now time.Time) *types.Firewall {
	t.mu.Lock()
	defer t.mu.Unlock()
	fw := &types.Firewall{Bans: []types.FirewallBan{}}
	bans := map[string]*types.FirewallBan{}
	watch := map[string]types.FirewallSuspect{}
	for _, node := range sortedKeys(t.nodes) {
		v := t.nodes[node]
		if now.Sub(v.at) > fwStale {
			continue
		}
		fw.Nodes = append(fw.Nodes, node)
		for _, b := range v.bans {
			if !now.Before(b.Until) {
				continue
			}
			cur := bans[b.Addr]
			if cur == nil {
				b := b
				b.Nodes = nil
				cur = &b
				bans[b.Addr] = cur
				cur.Blocked = 0
			}
			cur.Nodes = append(cur.Nodes, node)
			cur.Blocked += b.Blocked
			if b.Until.After(cur.Until) {
				cur.Until = b.Until
			}
			if b.At.Before(cur.At) {
				cur.At, cur.App, cur.Host, cur.Paths = b.At, b.App, b.Host, b.Paths
			}
		}
		for _, w := range v.watching {
			if cur, ok := watch[w.Addr]; !ok || w.NotFound > cur.NotFound {
				w.Node = node
				watch[w.Addr] = w
			}
		}
	}
	for _, b := range bans {
		fw.Bans = append(fw.Bans, *b)
	}
	sort.Slice(fw.Bans, func(i, j int) bool { return fw.Bans[i].At.After(fw.Bans[j].At) })
	for addr, w := range watch {
		if bans[addr] == nil {
			fw.Watching = append(fw.Watching, w)
		}
	}
	sort.Slice(fw.Watching, func(i, j int) bool {
		a, b := fw.Watching[i], fw.Watching[j]
		return a.NotFound > b.NotFound || a.NotFound == b.NotFound && a.Addr < b.Addr
	})
	fw.Watching = fw.Watching[:min(len(fw.Watching), fwTop)]
	for addr, s := range t.slowed {
		if now.Sub(s.Last) <= time.Hour {
			fw.Slowed = append(fw.Slowed, types.FirewallSlowedAddr{Addr: addr, FirewallSlowed: *s})
		}
	}
	sort.Slice(fw.Slowed, func(i, j int) bool {
		a, b := fw.Slowed[i], fw.Slowed[j]
		return a.Limited > b.Limited || a.Limited == b.Limited && a.Addr < b.Addr
	})
	fw.Slowed = fw.Slowed[:min(len(fw.Slowed), fwTop)]

	sites := map[string]*types.FirewallSite{}
	site := func(app string) *types.FirewallSite {
		if sites[app] == nil {
			sites[app] = &types.FirewallSite{App: app}
		}
		return sites[app]
	}
	for _, p := range t.recent {
		if now.Sub(p.at) < time.Minute {
			for app, c := range p.counts {
				fw.Minute.Add(c)
				site(app).Minute.Add(c)
			}
		}
	}
	nowMin := now.Unix() / 60
	for m, slot := range t.minutes {
		age := nowMin - m
		for app, c := range slot {
			if age < 60 {
				fw.Hour.Add(*c)
				site(app).Hour.Add(*c)
			}
			if age < 24*60 {
				fw.Day.Add(*c)
				site(app).Day.Add(*c)
			}
		}
	}
	for _, s := range sites {
		fw.Sites = append(fw.Sites, *s)
	}
	sort.Slice(fw.Sites, func(i, j int) bool {
		a, b := fw.Sites[i], fw.Sites[j]
		if a.Day.Blocked != b.Day.Blocked {
			return a.Day.Blocked > b.Day.Blocked
		}
		return a.App < b.App
	})
	return fw
}
