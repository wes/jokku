package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/wes/jokku/internal/types"
)

// Fencing. A volume whose backups can restore it is restored onto another
// node when its node has been down a while (see cluster.FailoverAfter). If
// that node is only cut off from the control node, not dead, its copy of
// the app would keep writing to its own disk while the restored one runs:
// two copies of a database, say, each taking orders. So a node that has
// lost the control node for FenceAfter stops the instances using such
// volumes, well before the control node would restore them, unless it is
// clearly the control node that is down: no node this one reaches hears from
// it, and together they are a strict majority of the nodes. The control node
// in turn only restores while it sees at least half of the nodes, so the two
// sides of a split never both run an app.

// The timings are variables so tests can shorten them. FenceAfter must stay
// well under cluster.FailoverAfter, and well over cluster.PollTimeout: an
// idle node only hears from the control node when a long poll returns.
var (
	FenceAfter      = 2 * time.Minute
	FenceCheckEvery = 5 * time.Second
)

// fence is what this node knows about its contact with the control node.
type fence struct {
	mu        sync.Mutex
	heard     time.Time // last answer from the control node
	etag      string    // the state it confirmed then
	checked   bool      // peers were asked since contact was lost
	cutOff    bool      // and said this node is the one cut off
	announced bool
}

// heard records that this node has the control node's latest state, etag.
// Long polls return at least every cluster.PollTimeout, well under
// FenceAfter.
func (a *Agent) heard(etag string) {
	a.fence.mu.Lock()
	a.fence.heard, a.fence.etag = time.Now(), etag
	a.fence.mu.Unlock()
}

// serveContact tells other nodes how long since this one heard from the
// control node. It gives nothing else away, so it needs no token.
func (a *Agent) serveContact(w http.ResponseWriter, r *http.Request) {
	a.fence.mu.Lock()
	heard := a.fence.heard
	a.fence.mu.Unlock()
	c := types.Contact{HeardAgoSeconds: -1}
	if !heard.IsZero() {
		c.HeardAgoSeconds = time.Since(heard).Seconds()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c)
}

// mayRunFenced says whether instances using volumes that would be restored
// elsewhere may run, applying state etag: while the control node answers
// and confirmed that state (not an older one a pass is still applying), or
// when it seems to be down for every node. Until that is known (just after
// starting, say), not.
func (a *Agent) mayRunFenced(etag string) bool {
	a.fence.mu.Lock()
	defer a.fence.mu.Unlock()
	if time.Since(a.fence.heard) < FenceAfter {
		return etag == a.fence.etag
	}
	return a.fence.checked && !a.fence.cutOff
}

// fenceLoop asks the other nodes, while the control node doesn't answer,
// whether they still hear from it.
func (a *Agent) fenceLoop(ctx context.Context) {
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		a.fence.mu.Lock()
		lost := time.Since(a.fence.heard) >= FenceAfter
		if !lost {
			a.fence.checked, a.fence.cutOff = false, false
			if a.fence.announced {
				a.fence.announced = false
				a.Log.Info("the control node answers again")
			}
		}
		a.fence.mu.Unlock()
		if lost {
			cutOff := a.askPeers(ctx, client)
			a.fence.mu.Lock()
			changed := !a.fence.checked || a.fence.cutOff != cutOff
			a.fence.checked, a.fence.cutOff = true, cutOff
			if changed && cutOff {
				a.fence.announced = true
				a.Log.Warn("cut off from the control node while other nodes still reach it: stopping the instances whose volumes it would restore elsewhere")
			}
			a.fence.mu.Unlock()
			if changed {
				a.Kick()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(FenceCheckEvery):
		}
	}
}

// askPeers says whether this node must take itself to be cut off: another
// node still hears from the control node, or this node and the ones it
// reaches are no strict majority (so the control node may see the rest).
func (a *Agent) askPeers(ctx context.Context, client *http.Client) bool {
	st := a.Desired()
	if st == nil {
		return true
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	reached, heard := 0, 0
	for _, p := range st.Peers {
		if p.AgentAddr == "" {
			continue
		}
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/v1/contact", nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var c types.Contact
			if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&c) != nil {
				return
			}
			mu.Lock()
			reached++
			if c.HeardAgoSeconds >= 0 && c.HeardAgoSeconds < FenceAfter.Seconds() {
				heard++
			}
			mu.Unlock()
		}(p.AgentAddr)
	}
	wg.Wait()
	nodes := 1 // edges have no vote: they run nothing, and sit outside
	for _, p := range st.Peers {
		if p.Role != types.RoleEdge {
			nodes++
		}
	}
	return heard > 0 || (1+reached)*2 <= nodes
}

// fenced says whether an instance of state etag must stay stopped: it uses
// a volume that would be restored elsewhere, and this node may be cut off.
func (a *Agent) fenced(spec types.InstanceSpec, etag string) bool {
	if len(spec.Volumes) == 0 || a.mayRunFenced(etag) {
		return false
	}
	a.volMu.Lock()
	defer a.volMu.Unlock()
	for _, m := range spec.Volumes {
		if v := a.vols[m.ID]; v != nil && v.autoRestore {
			return true
		}
	}
	return false
}
