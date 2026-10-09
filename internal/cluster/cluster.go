// Package cluster is the control plane: it decides what every node runs and
// learns what is actually running.
//
//	agent --GET  /v1/agent/state (long-poll)--> control   desired instances, mesh peers, proxy routes
//	agent --POST /v1/agent/status ------------> control   observed instance states, node metrics
//
// The control node's own agent uses the same two calls in-process, so one
// code path runs every node. Desired state is recomputed from the store on
// every poll and identified by an ETag, so no change can be "missed": at
// worst an agent sees it at its next poll, a couple of seconds later, and
// Changed makes that immediate.
package cluster

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/wes/jokku/internal/store"
)

type Controller struct {
	Store       *store.Store
	Log         *slog.Logger
	Self        string       // the control node's name
	ClusterCIDR netip.Prefix // e.g. 10.210.0.0/16
	Version     string
	// Address is where joining nodes reach this node's API, host:port.
	Address string
	// Pin identifies this node's TLS key; it is embedded in join tokens.
	Pin string
	// TickEvery is how often the controller loop runs (default 3s).
	TickEvery time.Duration
	// AgentAddr is where a node's agent API listens, host:port. The default
	// is its mesh address and AgentPort; tests override it.
	AgentAddr func(store.Node) string

	mu        sync.Mutex
	changed   chan struct{} // closed and replaced on every change
	lastReady map[string]bool
	attempts  map[string]time.Time // instance ID -> last replacement attempt
	pinned    map[string]string    // instance ID -> why it can't move (told once)

	volMu       sync.Mutex           // one volume decision at a time; guards volAttempts
	volAttempts map[string]time.Time // volume ID -> last attempt to move it off a draining node
	backups     sync.Map             // volume ID -> true while it is being backed up
}

func New(c *Controller) *Controller {
	c.changed = make(chan struct{})
	c.lastReady = map[string]bool{}
	c.attempts = map[string]time.Time{}
	c.pinned = map[string]string{}
	c.volAttempts = map[string]time.Time{}
	return c
}

// agentAddr is where n's agent API listens.
func (c *Controller) agentAddr(n store.Node) string {
	if c.AgentAddr != nil {
		return c.AgentAddr(n)
	}
	_, meshIP := NodeSubnet(c.ClusterCIDR, n.SubnetIndex)
	return net.JoinHostPort(meshIP.String(), strconv.Itoa(AgentPort))
}

// Changed wakes every waiting agent so it recomputes its state now.
func (c *Controller) Changed() {
	c.mu.Lock()
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
}

func (c *Controller) changes() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

// NodeSubnet returns node index's /24 inside the /16 cluster network and the
// node's own address (.1) in it: index 3 in 10.210.0.0/16 is 10.210.3.0/24
// and 10.210.3.1.
func NodeSubnet(cluster netip.Prefix, index int) (netip.Prefix, netip.Addr) {
	b := cluster.Masked().Addr().As4()
	b[2] = byte(index)
	subnet := netip.PrefixFrom(netip.AddrFrom4(b), 24)
	b[3] = 1
	return subnet, netip.AddrFrom4(b)
}

// RescheduleAfter is how long a node may be down before its instances are
// started elsewhere. Short blips (a reboot, an update) don't move anything.
// The timings here are variables so tests can shorten them.
var RescheduleAfter = 60 * time.Second

// Run is the controller loop: it notices nodes going down or coming back,
// moves instances off dead, removed and draining nodes, and cleans up.
func (c *Controller) Run(ctx context.Context) {
	if c.TickEvery == 0 {
		c.TickEvery = 3 * time.Second
	}
	t := time.NewTicker(c.TickEvery)
	defer t.Stop()
	for {
		if err := c.tick(ctx); err != nil && ctx.Err() == nil {
			c.Log.Error("controller", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
