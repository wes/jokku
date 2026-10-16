package nettest

import (
	"strings"
	"testing"

	"github.com/wes/jokku/internal/types"
)

func ping(avg float64, sent, received int) types.PingStats {
	return types.PingStats{Sent: sent, Received: received, Min: avg / 2, Avg: avg, Max: avg * 2}
}

// cluster is a test between nodes where every link runs at 896 Mbit/s with
// 0.5 ms round trips, then each link is passed to change.
func cluster(nodes []string, change func(l *types.NetLink)) *types.NetTest {
	t := &types.NetTest{Nodes: nodes}
	for _, from := range nodes {
		for _, to := range nodes {
			if from != to {
				l := types.NetLink{From: from, To: to, Mbps: 896, Ping: ping(0.5, 100, 100)}
				change(&l)
				t.Links = append(t.Links, l)
			}
		}
	}
	return t
}

func involves(l *types.NetLink, n string) bool { return l.From == n || l.To == n }

func grades(r Report) map[string]Grade {
	out := map[string]Grade{}
	for _, n := range r.Nodes {
		out[n.Name] = n.Grade
	}
	return out
}

// n3's port negotiated 100 Mbit/s, and n4 drops an echo now and then.
func TestTheOddNodeStandsOut(t *testing.T) {
	test := cluster([]string{"n1", "n2", "n3", "n4"}, func(l *types.NetLink) {
		if involves(l, "n3") {
			l.Mbps = 94
		}
		if involves(l, "n4") {
			l.Ping = ping(0.5, 100, 99)
		}
	})
	r := Read(test)
	if r.Mbps != 896 || r.RTT != 0.5 {
		t.Fatalf("usual link: %v Mbit/s, %v ms", r.Mbps, r.RTT)
	}
	// n1 and n2 have links to both, but most of theirs are fine. Half of
	// n4's links are slow ones to n3, and the rest lose echoes.
	if g := grades(r); g["n3"] != Bad || g["n4"] != Warn || g["n1"] != Good || g["n2"] != Good {
		t.Errorf("grades: %v", g)
	}
	s := Summary(test)
	if !strings.Contains(s, "usually 896 Mbit/s and 0.50 ms") || !strings.Contains(s, "n3 sends at 10% of the usual speed") ||
		!strings.Contains(s, "n4 ") || strings.Contains(s, "n1") {
		t.Errorf("summary: %s", s)
	}
}

// With three nodes, a slow one is on four links of six: the median link is
// its, yet it's still the one that stands out.
func TestASlowNodeInAThreeNodeCluster(t *testing.T) {
	r := Read(cluster([]string{"n1", "n2", "n3"}, func(l *types.NetLink) {
		if involves(l, "n3") {
			l.Mbps = 300
		}
	}))
	if r.Mbps != 896 {
		t.Errorf("usual: %v Mbit/s", r.Mbps)
	}
	if g := grades(r); g["n3"] != Bad || g["n1"] != Good || g["n2"] != Good {
		t.Errorf("grades: %v", g)
	}
}

// Two VMs on one host talk faster than any cable: the others are not slow
// for it.
func TestNodesSharingAHost(t *testing.T) {
	r := Read(cluster([]string{"n1", "n2", "n3", "n4", "n5"}, func(l *types.NetLink) {
		if involves(l, "n1") && involves(l, "n2") {
			l.Mbps = 9400
		}
	}))
	if r.Mbps != 896 {
		t.Errorf("usual: %v Mbit/s", r.Mbps)
	}
	for name, g := range grades(r) {
		if g != Good {
			t.Errorf("%s: %v", name, g)
		}
	}
}

func TestLinkGrades(t *testing.T) {
	r := Report{Mbps: 900, RTT: 0.5}
	for _, c := range []struct {
		link types.NetLink
		want Grade
	}{
		{types.NetLink{Mbps: 900, Ping: ping(0.5, 100, 100)}, Good},
		{types.NetLink{Mbps: 700, Ping: ping(0.5, 100, 100)}, Warn},
		{types.NetLink{Mbps: 900, Ping: ping(0.5, 100, 99)}, Warn},
		{types.NetLink{Mbps: 900, Ping: ping(0.5, 100, 98)}, Warn},
		{types.NetLink{Mbps: 900, Ping: ping(1.2, 100, 100)}, Good}, // twice as slow, but not by a millisecond
		{types.NetLink{Mbps: 900, Ping: ping(2.0, 100, 100)}, Warn},
		{types.NetLink{Mbps: 400, Ping: ping(0.5, 100, 100)}, Bad},
		{types.NetLink{Mbps: 900, Ping: ping(0.5, 100, 97)}, Bad},
		{types.NetLink{Mbps: 900, Ping: types.PingStats{Sent: 100}}, Bad},
		{types.NetLink{Error: "connection refused"}, Bad},
	} {
		if got := r.Grade(c.link); got != c.want {
			t.Errorf("%+v: %v, want %v", c.link, got, c.want)
		}
	}
}

func TestSummaries(t *testing.T) {
	nodes := []string{"n1", "n2", "n3", "n4", "n5"}
	alike := cluster(nodes, func(*types.NetLink) {})
	if s := Summary(alike); s != "network tested: 20 links between 5 nodes, usually 896 Mbit/s and 0.50 ms; every node alike" {
		t.Errorf("summary: %s", s)
	}
	one := cluster(nodes, func(l *types.NetLink) {
		if l.From == "n2" && l.To == "n4" {
			l.Mbps = 600
		}
	})
	if s := Summary(one); !strings.HasSuffix(s, "; n2 → n4 stands out") {
		t.Errorf("summary: %s", s)
	}
}
