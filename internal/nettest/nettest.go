// Package nettest reads network tests (types.NetTest) the same way for
// jokku top, network:report and the event a test records: what a healthy
// link does in this cluster, how each link and node compares, and which
// nodes stand out.
package nettest

import (
	"fmt"
	"slices"

	"github.com/wes/jokku/internal/types"
)

// Grade is how a link or node compares with the rest of the cluster.
type Grade int

const (
	Good Grade = iota
	Warn       // noticeably slower, a lost echo, or slow round trips
	Bad        // half the usual speed or less, losing echoes, or failed
)

// Thresholds, against what a healthy link does.
const (
	warnSpeed = 0.85 // of the usual throughput
	badSpeed  = 0.5
	badLoss   = 2.0 // percent; any loss at all is worth a look
)

// Report is a test read: the usual link, and each node.
type Report struct {
	// Mbps and RTT are what a healthy link does here: the median, over
	// nodes, of each node's best link. A slow node is on most links of a
	// small cluster, so the median link would be its; and two nodes on one
	// host can talk faster than any cable, so the best link would be theirs.
	Mbps float64
	RTT  float64 // ms
	// Nodes are in the test's order.
	Nodes []Node
}

// Node is how one node did: the median of the links it sent on, of those
// it received on, and of their round trips; the echoes lost on its links
// both ways; and how it compares. Links to nodes blamed for theirs are left
// out (see blame).
type Node struct {
	Name    string
	Out, In float64 // Mbit/s
	RTT     float64 // ms
	Sent    int     // echoes
	Lost    int
	Failed  int // links that couldn't be measured
	// Grade and Problems are set for a node blamed for its links looking
	// wrong (see blame).
	Grade    Grade
	Problems []string
}

// LossPct is the share of echoes lost on the node's links.
func (n Node) LossPct() float64 {
	if n.Sent == 0 {
		return 0
	}
	return float64(n.Lost) / float64(n.Sent) * 100
}

// Read reads a test.
func Read(t *types.NetTest) Report {
	r := Report{}
	bestMbps, bestRTT := map[string]float64{}, map[string]float64{}
	for _, l := range t.Links {
		for _, n := range []string{l.From, l.To} {
			bestMbps[n] = max(bestMbps[n], l.Mbps)
			if l.Ping.Received > 0 && (bestRTT[n] == 0 || l.Ping.Avg < bestRTT[n]) {
				bestRTT[n] = l.Ping.Avg
			}
		}
	}
	var ms, rs []float64
	for _, n := range t.Nodes {
		if bestMbps[n] > 0 {
			ms = append(ms, bestMbps[n])
		}
		if bestRTT[n] > 0 {
			rs = append(rs, bestRTT[n])
		}
	}
	r.Mbps, r.RTT = median(ms), median(rs)

	grades := r.blame(t)
	for _, name := range t.Nodes {
		n := node(t, name, grades)
		if g, ok := grades[name]; ok {
			n.Grade, n.Problems = g, r.problems(n)
		}
		r.Nodes = append(r.Nodes, n)
	}
	return r
}

// node is a node's figures over its links, but those to other blamed nodes:
// a node isn't slow for sharing a link with a slow one.
func node(t *types.NetTest, name string, blamed map[string]Grade) Node {
	n := Node{Name: name}
	var outs, ins, rtts []float64
	for _, l := range t.Links {
		peer := l.To
		if l.To == name {
			peer = l.From
		} else if l.From != name {
			continue
		}
		if _, ok := blamed[peer]; ok {
			continue
		}
		if l.Mbps == 0 {
			n.Failed++
		} else if l.From == name {
			outs = append(outs, l.Mbps)
		} else {
			ins = append(ins, l.Mbps)
		}
		if l.Ping.Received > 0 {
			rtts = append(rtts, l.Ping.Avg)
		}
		n.Sent += l.Ping.Sent
		n.Lost += l.Ping.Sent - l.Ping.Received
	}
	n.Out, n.In, n.RTT = median(outs), median(ins), median(rtts)
	return n
}

// blame grades the nodes whose links look wrong. A node with a bad cable or
// port makes every link it's on look wrong, the others' links to it too; so
// the node with the largest share of wrong links is blamed first, as long
// as that's most of them, and its links are then left out when judging the
// rest. Two nodes that only have each other are never blamed: either end
// could be at fault.
func (r *Report) blame(t *types.NetTest) map[string]Grade {
	blamed := map[string]Grade{}
	for {
		best, bestShare, bestBad := "", 0.5, 0
		var bestGrade Grade
		for _, name := range t.Nodes {
			if _, ok := blamed[name]; ok {
				continue
			}
			links, wrong, bad := 0, 0, 0
			peers := map[string]bool{}
			for _, l := range t.Links {
				peer := l.To
				if l.To == name {
					peer = l.From
				} else if l.From != name {
					continue
				}
				if _, ok := blamed[peer]; ok {
					continue
				}
				links++
				peers[peer] = true
				switch r.Grade(l) {
				case Bad:
					wrong++
					bad++
				case Warn:
					wrong++
				}
			}
			if len(peers) < 2 {
				continue
			}
			share := float64(wrong) / float64(links)
			if share > bestShare || share == bestShare && best != "" && bad > bestBad {
				best, bestShare, bestBad = name, share, bad
				bestGrade = Warn
				if bad*2 > links {
					bestGrade = Bad
				}
			}
		}
		if best == "" {
			return blamed
		}
		blamed[best] = bestGrade
	}
}

// Grade grades a link against the usual one.
func (r Report) Grade(l types.NetLink) Grade {
	loss := l.Ping.LossPct()
	switch {
	case l.Mbps == 0 || l.Ping.Sent > 0 && l.Ping.Received == 0:
		return Bad
	case l.Mbps < badSpeed*r.Mbps || loss > badLoss:
		return Bad
	case l.Mbps < warnSpeed*r.Mbps || loss > 0 || slowRTT(l.Ping.Avg, r.RTT):
		return Warn
	}
	return Good
}

// slowRTT says whether a round trip stands out: twice the usual one, and
// at least a millisecond more (LAN round trips are a fraction of one, so
// twice is nothing on its own).
func slowRTT(rtt, usual float64) bool {
	return usual > 0 && rtt > 2*usual && rtt > usual+1
}

// problems says what is wrong with a node's links, worst first.
func (r Report) problems(n Node) []string {
	var bad, warn []string
	if n.Failed > 0 {
		bad = append(bad, fmt.Sprintf("%d link%s failed", n.Failed, plural(n.Failed)))
	}
	for _, dir := range []struct {
		what string
		mbps float64
	}{{"sends", n.Out}, {"receives", n.In}} {
		what := fmt.Sprintf("%s at %.0f%% of the usual speed", dir.what, dir.mbps/r.Mbps*100)
		switch {
		case dir.mbps == 0:
		case dir.mbps < badSpeed*r.Mbps:
			bad = append(bad, what)
		case dir.mbps < warnSpeed*r.Mbps:
			warn = append(warn, what)
		}
	}
	switch loss := n.LossPct(); {
	case loss > badLoss:
		bad = append(bad, fmt.Sprintf("%.1f%% of pings lost", loss))
	case loss > 0:
		warn = append(warn, fmt.Sprintf("%d of %d pings lost", n.Lost, n.Sent))
	}
	if slowRTT(n.RTT, r.RTT) {
		warn = append(warn, fmt.Sprintf("round trips %.1f× the usual", n.RTT/r.RTT))
	}
	return append(bad, warn...)
}

// Summary is a test in a line, as its event says it.
func Summary(t *types.NetTest) string {
	r := Read(t)
	s := fmt.Sprintf("network tested: %d links between %d nodes", len(t.Links), len(t.Nodes))
	if r.Mbps > 0 {
		s += fmt.Sprintf(", usually %.0f Mbit/s", r.Mbps)
		if r.RTT > 0 {
			s += fmt.Sprintf(" and %.2f ms", r.RTT)
		}
	}
	flagged := false
	for _, n := range r.Nodes {
		if n.Grade != Good {
			s += "; " + n.Name + " " + n.Problems[0]
			flagged = true
		}
	}
	if flagged {
		return s
	}
	var odd []string
	for _, l := range t.Links {
		if r.Grade(l) != Good {
			odd = append(odd, l.From+" → "+l.To)
		}
	}
	switch {
	case len(odd) == 0 && len(t.Links) > 0:
		s += "; every node alike"
	case len(odd) <= 2:
		for _, l := range odd {
			s += "; " + l + " stands out"
		}
	case len(odd) > 2:
		s += fmt.Sprintf("; %d links stand out", len(odd))
	}
	return s
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
