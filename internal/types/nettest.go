package types

import "time"

// FeatureNetTest is reported by agents that take part in network tests:
// they answer pings and take throughput from other nodes, and test a link
// when the control node asks.
const FeatureNetTest = "nettest"

// NetTest is one test of the network between the cluster's nodes (not its
// edges): every ordered pair in turn, over the mesh, so each link is
// measured alone and the way apps use it.
type NetTest struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Seconds  float64   `json:"seconds"` // how long each link was loaded
	Nodes    []string  `json:"nodes"`
	Links    []NetLink `json:"links"`
	// Skipped are the nodes left out, and why ("not ready", "runs v0.8.18").
	Skipped map[string]string `json:"skipped,omitempty"`
	// Stopped says why the test ended before testing every link.
	Stopped string `json:"stopped,omitempty"`
}

// NetLink is what From measured towards To.
type NetLink struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Mbps is the TCP throughput From sent at, as To received it.
	Mbps float64   `json:"mbps"`
	Ping PingStats `json:"ping"`
	// Error says why the link couldn't be measured (or only in part).
	Error string `json:"error,omitempty"`
}

// PingStats are round trips of ICMP echoes, in milliseconds.
type PingStats struct {
	Sent     int     `json:"sent"`
	Received int     `json:"received"`
	Min      float64 `json:"min_ms"`
	Avg      float64 `json:"avg_ms"`
	Max      float64 `json:"max_ms"`
	Jitter   float64 `json:"jitter_ms"` // standard deviation, ping's mdev
}

// LossPct is the share of echoes that got no answer, in percent.
func (p PingStats) LossPct() float64 {
	if p.Sent == 0 {
		return 0
	}
	return float64(p.Sent-p.Received) / float64(p.Sent) * 100
}

// NetTestProgress is a test that is running.
type NetTestProgress struct {
	Started time.Time `json:"started"`
	Done    int       `json:"done"`  // links measured
	Total   int       `json:"total"` // links to measure
	From    string    `json:"from"`  // the link being measured
	To      string    `json:"to"`
	// Test is what was measured so far.
	Test *NetTest `json:"test"`
}

// NetTests is the network test that is running, if any, and the latest
// finished ones, newest first.
type NetTests struct {
	Running *NetTestProgress `json:"running,omitempty"`
	Tests   []NetTest        `json:"tests"`
}

// NetTestRequest starts a test. Seconds is how long each link is loaded
// (default 3).
type NetTestRequest struct {
	Seconds float64 `json:"seconds,omitempty"`
}

// NetProbe asks a node's agent to measure its link to another node's
// agent, at Target (host:port on the mesh).
type NetProbe struct {
	Target  string  `json:"target"`
	Seconds float64 `json:"seconds"`
	Pings   int     `json:"pings"`
}

// NetProbeResult is what an agent measured for a NetProbe.
type NetProbeResult struct {
	Mbps      float64   `json:"mbps"`
	Ping      PingStats `json:"ping"`
	PingError string    `json:"ping_error,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// NetSinkResult is what a node took in from a throughput test.
type NetSinkResult struct {
	Bytes   int64   `json:"bytes"`
	Seconds float64 `json:"seconds"`
}
