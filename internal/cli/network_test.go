package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/types"
)

func netTest(finished time.Time, slow string) types.NetTest {
	nodes := []string{"jokku1", "jokku2", "jokku3"}
	t := types.NetTest{Started: finished.Add(-30 * time.Second), Finished: finished, Seconds: 3, Nodes: nodes}
	for _, from := range nodes {
		for _, to := range nodes {
			if from != to {
				l := types.NetLink{From: from, To: to, Mbps: 941, Ping: types.PingStats{Sent: 100, Received: 100, Avg: 0.5}}
				if from == slow || to == slow {
					l.Mbps = 94
				}
				t.Links = append(t.Links, l)
			}
		}
	}
	return t
}

func TestNetworkCLI(t *testing.T) {
	dir := t.TempDir()
	startAPI(t, func(c *cluster.Controller) { c.DataDir = dir })
	if out := mustJokku(t, "network:report"); !strings.Contains(out, "No network test yet: jokku network:test") {
		t.Errorf("before any test:\n%s", out)
	}
	_, errOut, code := jokku(t, "", "network:test")
	if code == 0 || !strings.Contains(errOut, "a network test needs two nodes to test between") {
		t.Errorf("a test with one node: exit %d, %s", code, errOut)
	}

	// A control node with two tests kept.
	dir = t.TempDir()
	now := time.Now()
	b, _ := json.Marshal([]types.NetTest{netTest(now.Add(-time.Minute), "jokku3"), netTest(now.Add(-3*time.Hour), "")})
	if err := os.WriteFile(filepath.Join(dir, "nettests.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	startAPI(t, func(c *cluster.Controller) { c.DataDir = dir })
	out := mustJokku(t, "network:report")
	for _, want := range []string{
		"=====> Network test 1m ago (30s; 3 nodes, 6 links over the mesh, 3s each)",
		"A link here usually carries 941 Mbit/s and answers in 0.50 ms.",
		"jokku3 sends at 10% of the usual speed; receives at 10% of the usual speed.",
		"Mbit/s from \\ to", "94 !!", "ms from \\ to",
		"VS. THE TEST 3H AGO", "-90% speed, +0.00 ms", "+0% speed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
