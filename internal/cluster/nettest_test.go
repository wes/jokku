package cluster_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/types"
)

// A network test measures every ordered pair of nodes in turn, through their
// agents (real throughput, between the harness's agent servers), keeps the
// result where a later test can be compared with it, and records an event.
// One test runs at a time.
func TestNetTest(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.join("w2")
	h.waitReady(3)
	eventually(t, 5*time.Second, "every node able to take part", func() error {
		nodes, err := h.st.Nodes(h.ctx)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if !n.Has(types.FeatureNetTest) {
				return errFmt(n.Name + " hasn't said it can")
			}
		}
		return nil
	})

	p, err := h.client.StartNetTest(h.ctx, types.NetTestRequest{Seconds: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 6 {
		t.Errorf("%d links to test between 3 nodes, want 6", p.Total)
	}
	if _, err := h.client.StartNetTest(h.ctx, types.NetTestRequest{Seconds: 0.2}); err == nil || !strings.Contains(err.Error(), "is running") {
		t.Errorf("a second test while one runs: %v", err)
	}
	var tests *types.NetTests
	eventually(t, 30*time.Second, "the test finished", func() error {
		tests, err = h.client.NetTests(h.ctx)
		if err != nil {
			return err
		}
		if tests.Running != nil {
			return errFmt("running: " + tests.Running.From + " → " + tests.Running.To)
		}
		return nil
	})
	if len(tests.Tests) != 1 {
		t.Fatalf("%d tests kept", len(tests.Tests))
	}
	test := tests.Tests[0]
	if strings.Join(test.Nodes, ",") != "control,w1,w2" || len(test.Links) != 6 {
		t.Fatalf("nodes %v, %d links", test.Nodes, len(test.Links))
	}
	seen := map[string]bool{}
	for _, l := range test.Links {
		seen[l.From+">"+l.To] = true
		if l.Error != "" || l.Mbps < 10 || l.Ping.Received != 100 {
			t.Errorf("%s → %s: %+v", l.From, l.To, l)
		}
	}
	if len(seen) != 6 || seen["w1>w1"] {
		t.Errorf("links: %v", seen)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "nettests.json")); err != nil {
		t.Errorf("the test wasn't kept: %v", err)
	}
	if !h.hasEvent("", "network tested: 6 links between 3 nodes") {
		t.Error("no event about the test")
	}
	if _, err := h.client.StartNetTest(h.ctx, types.NetTestRequest{Seconds: 99}); err == nil {
		t.Error("a 99-second test per link was accepted")
	}
}
