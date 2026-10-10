package cluster

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/wes/jokku/internal/props"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// An app can sleep when idle (ps:set <app> sleep-after 15m): after that
// long without a request, its instances stop, keeping their places, their
// addresses and their volumes, and the next request starts them again and
// waits for them.
//
// The proxies tell the control node which of these apps they served
// requests for every few seconds, counting requests still open (a
// websocket), and ask it to wake an app a request found asleep. When an
// app was last used is kept in memory only: after a restart of the control
// node, every app gets a whole sleep-after before it sleeps.
type sleeper struct {
	mu      sync.Mutex
	started time.Time
	used    map[string]time.Time // app -> last request, wake or deploy
	busy    map[string]int       // app -> deploys under way, which keep it awake
}

func (s *sleeper) touch(app string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used == nil {
		s.used = map[string]time.Time{}
	}
	if now.After(s.used[app]) {
		s.used[app] = now
	}
}

// idle is how long app has gone unused (with s.mu held).
func (s *sleeper) idle(app string, now time.Time) time.Duration {
	if s.busy[app] > 0 {
		return 0
	}
	last := s.started
	if t := s.used[app]; t.After(last) {
		last = t
	}
	return now.Sub(last)
}

// idleFor reports whether app has gone unused for after.
func (s *sleeper) idleFor(app string, after time.Duration, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idle(app, now) >= after
}

// forget drops what is known about apps that no longer exist.
func (s *sleeper) forget(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for app := range s.used {
		if !keep[app] {
			delete(s.used, app)
		}
	}
}

// Activity records what a node's proxy reports: apps it served, and apps a
// request is waiting to wake.
func (c *Controller) Activity(ctx context.Context, node string, act types.ProxyActivity) error {
	now := time.Now()
	for _, app := range act.Active {
		c.sleep.touch(app, now)
	}
	for _, app := range act.Wake {
		if _, err := c.Wake(ctx, app, "for a request through "+node); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// Wake starts a sleeping app's instances again, reporting whether it was
// asleep. Why finishes the event's "woke up ...".
func (c *Controller) Wake(ctx context.Context, app, why string) (bool, error) {
	c.sleep.touch(app, time.Now())
	woke, err := c.Store.SetAppAsleep(ctx, app, false)
	if err != nil || !woke {
		return false, err
	}
	c.Store.AddEvent(ctx, "deploy", app, "", "woke up %s", why)
	c.Changed()
	return true, nil
}

// KeepAwake wakes an app if it's asleep, and keeps it awake until done is
// called: a deploy or restart needs its instances running.
func (c *Controller) KeepAwake(ctx context.Context, app, why string) (done func(), err error) {
	c.sleep.mu.Lock()
	if c.sleep.busy == nil {
		c.sleep.busy = map[string]int{}
	}
	c.sleep.busy[app]++
	c.sleep.mu.Unlock()
	done = sync.OnceFunc(func() {
		c.sleep.mu.Lock()
		if c.sleep.busy[app]--; c.sleep.busy[app] <= 0 {
			delete(c.sleep.busy, app)
		}
		c.sleep.mu.Unlock()
		c.sleep.touch(app, time.Now())
	})
	if _, err := c.Wake(ctx, app, why); err != nil {
		done()
		return nil, err
	}
	return done, nil
}

// tickSleep puts apps to sleep that have gone unused for their
// sleep-after, and wakes sleeping apps that no longer should sleep (their
// setting was cleared, or a request could no longer reach them to wake
// them).
func (c *Controller) tickSleep(ctx context.Context, now time.Time) error {
	apps, err := c.Store.Apps(ctx)
	if err != nil {
		return err
	}
	p, _ := props.Lookup("ps")
	global, err := c.Store.Properties(ctx, "", "ps")
	if err != nil {
		return err
	}
	known := map[string]bool{}
	changed := false
	for _, a := range apps {
		known[a.Name] = true
		asleep := a.AsleepSince != nil
		if a.Kind != "" || (asleep && a.Stopped) { // a stopped app wakes with ps:start
			continue
		}
		appProps, err := c.Store.Properties(ctx, a.Name, "ps")
		if err != nil {
			return err
		}
		after := props.SleepAfter(props.Compute(p, appProps, global)["sleep-after"])
		could := after > 0 && !a.Stopped && a.CurrentRelease != 0
		if !asleep && (!could || !c.sleep.idleFor(a.Name, after, now)) {
			continue // the common case, decided without looking further
		}
		can := false
		if could {
			if can, err = c.wakeable(ctx, a.Name); err != nil {
				return err
			}
		}
		switch {
		case asleep && !can:
			if _, err := c.Wake(ctx, a.Name, "now that it no longer sleeps when idle"); err != nil {
				return err
			}
		case !asleep && can:
			slept, err := c.sleepIfIdle(ctx, a.Name, after, now)
			if err != nil {
				return err
			}
			changed = changed || slept
		}
	}
	c.sleep.forget(known)
	if changed {
		c.Changed()
	}
	return nil
}

// sleepIfIdle puts app to sleep if it has gone unused for after. The
// sleeper's lock is held throughout, so a deploy starting meanwhile
// (KeepAwake) either keeps it awake or wakes it again.
func (c *Controller) sleepIfIdle(ctx context.Context, app string, after time.Duration, now time.Time) (bool, error) {
	c.sleep.mu.Lock()
	defer c.sleep.mu.Unlock()
	if c.sleep.idle(app, now) < after {
		return false, nil
	}
	slept, err := c.Store.SetAppAsleep(ctx, app, true)
	if slept {
		c.Store.AddEvent(ctx, "deploy", app, "", "went to sleep after %s without requests; the next one wakes it", shortDuration(after))
	}
	return slept, err
}

// shortDuration writes 15m and 1h30m, not 15m0s and 1h30m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// wakeable reports whether a request can wake app: the proxy routes its
// domains to its web process. Without that, nothing would ever wake it.
func (c *Controller) wakeable(ctx context.Context, app string) (bool, error) {
	cur, err := c.Store.CurrentRelease(ctx, app)
	if err != nil || cur == nil {
		return false, err
	}
	insts, err := c.Store.Instances(ctx, app)
	if err != nil {
		return false, err
	}
	web := false
	for _, in := range insts {
		web = web || (in.ReleaseID == cur.ID && in.ProcessType == cur.WebProcess() && in.Desired == store.DesiredRunning)
	}
	if !web {
		return false, nil
	}
	px, _ := props.Lookup("proxy")
	global, err := c.Store.Properties(ctx, "", "proxy")
	if err != nil {
		return false, err
	}
	appProxy, err := c.Store.Properties(ctx, app, "proxy")
	if err != nil {
		return false, err
	}
	if props.Compute(px, appProxy, global)["enabled"] != "true" {
		return false, nil
	}
	domains, err := c.Store.Domains(ctx, app)
	return len(domains) > 0, err
}
