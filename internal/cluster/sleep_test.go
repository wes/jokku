package cluster_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/props"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

func init() {
	props.MinSleepAfter = time.Second
}

// postActivity reports activity the way n's proxy does.
func (h *harness) postActivity(n *node, act types.ProxyActivity) error {
	b, _ := json.Marshal(act)
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/v1/agent/activity", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+n.token)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cluster.PinnedTLS(h.pin)}}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("activity: %s", resp.Status)
	}
	return nil
}

// route is the route n's proxy has for app's domains.
func (p *fakeProxy) route(app string) *types.ProxyRoute {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ps == nil {
		return nil
	}
	for _, r := range p.ps.Routes {
		if r.App == app && len(r.Hosts) > 0 {
			return &r
		}
	}
	return nil
}

func (h *harness) asleep(app string) bool {
	a, err := h.st.App(h.ctx, app)
	return err == nil && a.AsleepSince != nil
}

// instanceIDs lists app's wanted instances, with their nodes.
func (h *harness) instanceIDs(app string) []string {
	insts, _ := h.st.Instances(h.ctx, app)
	var out []string
	for _, in := range insts {
		if in.Desired == store.DesiredRunning {
			out = append(out, in.ID+"@"+in.Node)
		}
	}
	sort.Strings(out)
	return out
}

func (h *harness) hasEvent(app, text string) bool {
	events, _ := h.st.Events(h.ctx, app, 50)
	for _, e := range events {
		if strings.Contains(e.Message, text) {
			return true
		}
	}
	return false
}

func TestIdleAppSleepsAndARequestWakesIt(t *testing.T) {
	h := newHarness(t)
	w1 := h.join("w1")
	eventually(t, 5*time.Second, "w1 reports in", func() error {
		st, _ := h.ctl.Status(h.ctx)
		if st.Totals.NodesReady != 2 {
			return fmt.Errorf("%d of 2 nodes ready", st.Totals.NodesReady)
		}
		return nil
	})
	h.deploy("web", 2)
	h.deploy("busy", 1)
	h.deploy("always", 1) // never sleeps
	before := h.instanceIDs("web")
	for _, app := range []string{"web", "busy"} {
		if err := h.client.SetProperty(h.ctx, app, "ps", "sleep-after", "2s"); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.client.SetProperty(h.ctx, "web", "ps", "sleep-after", "5"); err == nil {
		t.Error("a sleep-after without a unit was accepted")
	}

	// The proxies know which apps sleep, so their requests are counted.
	eventually(t, 3*time.Second, "routes of apps that sleep", func() error {
		for _, n := range h.nodes {
			if r := n.proxy.route("web"); r == nil || !r.Sleeps {
				return fmt.Errorf("%s: web route %+v", n.name, r)
			}
			if r := n.proxy.route("always"); r == nil || r.Sleeps {
				return fmt.Errorf("%s: always route %+v", n.name, r)
			}
			if recs := n.resolver.get().Records["web.web.internal"]; len(recs) != 2 {
				return fmt.Errorf("%s: web.web.internal resolves to %v", n.name, recs)
			}
		}
		return nil
	})

	// busy gets requests; web gets none and goes to sleep.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
				h.postActivity(w1, types.ProxyActivity{Active: []string{"busy"}})
			}
		}
	}()
	eventually(t, 8*time.Second, "web sleeps", func() error {
		switch {
		case !h.asleep("web"):
			return fmt.Errorf("web is awake")
		case h.nodes["control"].rt.apps()["web"]+w1.rt.apps()["web"] != 0:
			return fmt.Errorf("web VMs still run")
		}
		for _, n := range h.nodes {
			if r := n.proxy.route("web"); r == nil || !r.Sleeps || len(r.Upstreams) != 0 {
				return fmt.Errorf("%s: web route %+v", n.name, r)
			}
			if recs := n.resolver.get().Records["web.web.internal"]; len(recs) != 0 {
				return fmt.Errorf("%s: web.internal still resolves to %v", n.name, recs)
			}
		}
		return nil
	})
	if h.asleep("busy") || h.placement("busy")["control"]+h.placement("busy")["w1"] != 1 {
		t.Error("busy, which gets requests, went to sleep")
	}
	// Asleep isn't unhealthy: web's two instances are counted apart.
	eventually(t, 3*time.Second, "the totals leave web out", func() error {
		st, _ := h.ctl.Status(h.ctx)
		if t := st.Totals; t.InstancesAsleep != 2 || t.Instances != 2 || t.InstancesHealthy != 2 {
			return fmt.Errorf("totals %+v", t)
		}
		return nil
	})
	if h.asleep("always") {
		t.Error("an app without sleep-after went to sleep")
	}
	if !h.hasEvent("web", "went to sleep after 2s without requests") {
		t.Error("no event for web going to sleep")
	}
	if got := h.instanceIDs("web"); strings.Join(got, " ") != strings.Join(before, " ") {
		t.Errorf("sleeping replaced web's instances: %v, was %v", got, before)
	}

	// A request through w1 wakes it, where it was.
	if err := h.postActivity(w1, types.ProxyActivity{Wake: []string{"web"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "web wakes", func() error {
		switch {
		case h.asleep("web"):
			return fmt.Errorf("web is asleep")
		case len(h.placement("web")) != 2:
			return fmt.Errorf("web runs on %v", h.placement("web"))
		}
		for _, n := range h.nodes {
			if r := n.proxy.route("web"); r == nil || len(r.Upstreams) != 2 {
				return fmt.Errorf("%s: web route %+v", n.name, r)
			}
		}
		return nil
	})
	if got := h.instanceIDs("web"); strings.Join(got, " ") != strings.Join(before, " ") {
		t.Errorf("waking replaced web's instances: %v, was %v", got, before)
	}
	if !h.hasEvent("web", "woke up for a request through w1") {
		t.Error("no event for web waking")
	}

	// It sleeps again, and clearing sleep-after wakes it for good.
	eventually(t, 8*time.Second, "web sleeps again", func() error {
		if !h.asleep("web") {
			return fmt.Errorf("web is awake")
		}
		return nil
	})
	if err := h.client.SetProperty(h.ctx, "web", "ps", "sleep-after", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "web wakes once it no longer sleeps", func() error {
		if h.asleep("web") || len(h.placement("web")) != 2 {
			return fmt.Errorf("asleep %v, running on %v", h.asleep("web"), h.placement("web"))
		}
		if r := h.nodes["control"].proxy.route("web"); r == nil || r.Sleeps {
			return fmt.Errorf("web route %+v", r)
		}
		return nil
	})
}

func TestDeploysAndStartWakeASleepingApp(t *testing.T) {
	h := newHarness(t)
	h.deploy("web", 1)
	if err := h.client.SetProperty(h.ctx, "web", "ps", "sleep-after", "1s"); err != nil {
		t.Fatal(err)
	}
	sleep := func() {
		t.Helper()
		eventually(t, 6*time.Second, "web sleeps", func() error {
			if !h.asleep("web") || h.nodes["control"].rt.apps()["web"] != 0 {
				return fmt.Errorf("asleep %v, %d VMs", h.asleep("web"), h.nodes["control"].rt.apps()["web"])
			}
			return nil
		})
	}
	sleep()

	// A restart rolls out to running instances, and the app stays awake
	// through it however long it takes.
	var out []string
	if err := h.pipe.PS(h.ctx, "web", "restart", "test", func(l string) { out = append(out, l) }); err != nil {
		t.Fatalf("restart of a sleeping app: %v\n%s", err, strings.Join(out, "\n"))
	}
	if h.asleep("web") || h.placement("web")["control"] != 1 {
		t.Fatalf("after a restart: asleep %v, running %v", h.asleep("web"), h.placement("web"))
	}
	if !h.hasEvent("web", "woke up for a restart") {
		t.Error("no event for waking for the restart")
	}
	sleep()

	// ps:start wakes it and waits for it.
	out = nil
	if err := h.pipe.PS(h.ctx, "web", "start", "test", func(l string) { out = append(out, l) }); err != nil {
		t.Fatalf("ps:start of a sleeping app: %v\n%s", err, strings.Join(out, "\n"))
	}
	if h.asleep("web") || h.placement("web")["control"] != 1 {
		t.Fatalf("after ps:start: asleep %v, running %v", h.asleep("web"), h.placement("web"))
	}
	if got := strings.Join(out, "\n"); !strings.Contains(got, "web is awake") {
		t.Errorf("ps:start output:\n%s", got)
	}

	// A stopped app neither sleeps nor wakes for a request.
	if err := h.pipe.PS(h.ctx, "web", "stop", "test", func(string) {}); err != nil {
		t.Fatal(err)
	}
	if err := h.postActivity(&node{token: ""}, types.ProxyActivity{Wake: []string{"web"}}); err == nil {
		t.Error("activity without a node token was accepted")
	}
	if _, err := h.ctl.Wake(h.ctx, "web", "for a test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if h.asleep("web") || h.nodes["control"].rt.apps()["web"] != 0 {
		t.Errorf("stopped app: asleep %v, %d VMs", h.asleep("web"), h.nodes["control"].rt.apps()["web"])
	}
	if r := h.nodes["control"].proxy.route("web"); r == nil || r.Sleeps {
		t.Errorf("a stopped app's route wakes it: %+v", r)
	}
}
