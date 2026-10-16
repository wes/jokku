package cluster_test

import (
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/wes/jokku/internal/agent"
	"github.com/wes/jokku/internal/store"
)

func TestInternalDNSNames(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.waitReady(2)
	h.deploy("api", 2) // one instance on each node

	insts, _ := h.st.Instances(h.ctx, "api")
	var ips []string
	for _, in := range insts {
		if in.Desired == store.DesiredRunning {
			ips = append(ips, in.IP)
		}
	}
	sort.Strings(ips)
	meshIP := map[string]string{"control": "10.210.1.1", "w1": "10.210.2.1"}
	eventually(t, 5*time.Second, "every node serves the app's names", func() error {
		for name, n := range h.nodes {
			z := n.resolver.get()
			if z.Listen != meshIP[name] {
				return fmt.Errorf("%s listens on %q", name, z.Listen)
			}
			for _, rec := range []string{"web.api.internal", "api.internal"} {
				if !slices.Equal(z.Records[rec], ips) {
					return fmt.Errorf("%s: %s = %v, want %v", name, rec, z.Records[rec], ips)
				}
			}
		}
		return nil
	})

	// VMs ask their own node, and look names up in their app first.
	for name, n := range h.nodes {
		n.rt.mu.Lock()
		for _, s := range n.rt.running {
			if !slices.Equal(s.Guest.DNS, []string{meshIP[name]}) || !slices.Equal(s.Guest.Search, []string{"api.internal", "internal"}) {
				t.Errorf("%s: VM %s has DNS %v, search %v", name, s.Process, s.Guest.DNS, s.Guest.Search)
			}
		}
		n.rt.mu.Unlock()
	}

	// A stopped app has no names.
	if err := h.pipe.PS(h.ctx, "api", "stop", "test", func(string) {}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the stopped app's names are gone", func() error {
		for name, n := range h.nodes {
			if recs := n.resolver.get().Records; len(recs) != 0 {
				return fmt.Errorf("%s still serves %v", name, recs)
			}
		}
		return nil
	})
}

// When a node boots, its DNS service answers a moment after the agent
// starts. An instance waits for it rather than start with the host's
// resolvers, which don't know internal names: an app reaching its database
// as mysql-myapp-db.internal could never reach it. (One didn't, after every
// node restarted at once.) If it never answers, the instance starts after
// DNSWait anyway, with the internet but without internal names.
func TestInstancesWaitForTheNodesDNS(t *testing.T) {
	defer func(d time.Duration) { agent.DNSWait = d }(agent.DNSWait)
	agent.DNSWait = 3 * time.Second
	h := newHarness(t)
	w1 := h.join("w1")
	h.waitReady(2)
	undo := h.onlyOn("w1")
	h.deploy("api", 1)
	undo()

	// reboot restarts w1 with its VMs gone and its DNS service not
	// answering, and returns how its instance starts.
	reboot := func(dnsAfter time.Duration) (dns []string, after time.Duration) {
		h.kill("w1")
		var ids []string
		w1.rt.mu.Lock()
		for id := range w1.rt.running {
			ids = append(ids, id)
		}
		w1.rt.mu.Unlock()
		for _, id := range ids {
			w1.rt.crash(id)
		}
		w1.resolver.down.Store(true)
		start := time.Now()
		h.revive("w1")
		if dnsAfter > 0 {
			time.AfterFunc(dnsAfter, func() { w1.resolver.down.Store(false) })
		}
		eventually(t, 15*time.Second, "api started again on w1", func() error {
			w1.rt.mu.Lock()
			defer w1.rt.mu.Unlock()
			for _, s := range w1.rt.running {
				if s.App == "api" {
					dns, after = s.Guest.DNS, time.Since(start)
					return nil
				}
			}
			return errFmt("not yet")
		})
		w1.resolver.down.Store(false)
		return dns, after
	}

	dns, after := reboot(1500 * time.Millisecond)
	if !slices.Equal(dns, []string{"10.210.2.1"}) || after < 1500*time.Millisecond {
		t.Errorf("with the node's DNS answering after 1.5s, api started after %s with DNS %v", after.Round(10*time.Millisecond), dns)
	}
	dns, after = reboot(0)
	if !slices.Equal(dns, []string{hostResolver}) || after < agent.DNSWait {
		t.Errorf("with the node's DNS never answering, api started after %s with DNS %v", after.Round(10*time.Millisecond), dns)
	}
}
