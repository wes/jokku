package cluster_test

import (
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

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
