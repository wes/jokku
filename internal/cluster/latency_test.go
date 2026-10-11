package cluster_test

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"
)

func near(got, want float64) bool { return math.Abs(got-want)/want <= 0.05 }

// TestLatencyAddsUpEveryNode: the hour's and day's percentiles are the
// whole cluster's requests', from every node's proxy, not an average of
// nodes' percentiles.
func TestLatencyAddsUpEveryNode(t *testing.T) {
	h := newHarness(t)
	w1 := h.join("w1")
	edge, _ := h.addEdge("edge1")
	h.waitReady(3)
	now := time.Now()
	for range 90 {
		h.nodes["control"].latency.Add(now, "shop", 10)
	}
	for range 10 {
		w1.latency.Add(now, "shop", 1000)
	}
	edge.latency.Add(now.Add(-3*time.Hour), "blog", 50)
	edge.latency.Add(now, "", 2)

	l, err := h.client.Latency(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Missing) != 0 {
		t.Errorf("missing %v", l.Missing)
	}
	shop := l.Hour.Apps["shop"]
	if shop.Requests != 100 || !near(shop.P50, 10) || !near(shop.P95, 1000) {
		t.Errorf("shop over the hour: %+v, want 100 requests, p50 10ms, p95 1s", shop)
	}
	if l.Hour.Requests != 101 || l.Day.Requests != 102 {
		t.Errorf("%d requests in the hour, %d in the day; want 101 and 102", l.Hour.Requests, l.Day.Requests)
	}
	if _, ok := l.Hour.Apps["blog"]; ok || l.Day.Apps["blog"].Requests != 1 {
		t.Errorf("blog's request three hours ago: hour %+v, day %+v", l.Hour.Apps["blog"], l.Day.Apps["blog"])
	}
	if l.Hour.Apps[""].Requests != 1 {
		t.Error("requests no app served aren't counted")
	}

	// A node that's down is named, and its requests can't be counted.
	h.kill("w1")
	eventually(t, 5*time.Second, "w1 left out", func() error {
		l, err := h.client.Latency(h.ctx)
		if err != nil {
			return err
		}
		if !slices.Equal(l.Missing, []string{"w1"}) || l.Hour.Apps["shop"].Requests != 90 {
			return fmt.Errorf("missing %v, shop %+v", l.Missing, l.Hour.Apps["shop"])
		}
		return nil
	})
}
