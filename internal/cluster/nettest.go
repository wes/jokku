package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/wes/jokku/internal/nettest"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// Network tests (see agent/nettest.go). The control node measures every
// ordered pair of nodes in turn, so each link carries only its own test,
// over the mesh, the way apps reach each other. Edges are left out: their
// links cross the internet, where a test would only measure (and spend)
// the uplink. The latest tests are kept in the data directory, so one made
// before a change to the network (a new switch, say) can be compared with
// one made after it.

var (
	// NetTestSeconds is how long each link is loaded by default.
	NetTestSeconds = 3.0
	// NetTestPings is how many echoes each link gets, 10ms apart.
	NetTestPings = 100
)

const (
	netTestMaxSeconds = 20
	netTestKeep       = 10
)

// NetTestRefused says why a network test can't start.
type NetTestRefused struct{ msg string }

func (e *NetTestRefused) Error() string { return e.msg }

type netTester struct {
	mu      sync.Mutex
	running *types.NetTestProgress
	tests   []types.NetTest // newest first; nil until loaded
}

var netProbeClient = &http.Client{} // each probe's context bounds it

// NetTests is the running test, if any, and the latest finished ones.
func (c *Controller) NetTests(ctx context.Context) (*types.NetTests, error) {
	c.nettest.mu.Lock()
	defer c.nettest.mu.Unlock()
	if err := c.loadNetTests(); err != nil {
		return nil, err
	}
	out := &types.NetTests{Tests: slices.Clone(c.nettest.tests)}
	if r := c.nettest.running; r != nil {
		cp := *r
		test := *r.Test
		test.Links = slices.Clone(r.Test.Links)
		cp.Test = &test
		out.Running = &cp
	}
	return out, nil
}

// StartNetTest starts testing the network between the nodes, and returns
// at once; NetTests follows it.
func (c *Controller) StartNetTest(ctx context.Context, req types.NetTestRequest) (*types.NetTestProgress, error) {
	seconds := req.Seconds
	if seconds == 0 {
		seconds = NetTestSeconds
	}
	if seconds < 0 || seconds > netTestMaxSeconds {
		return nil, badSetting("seconds must be more than 0 and at most %d", netTestMaxSeconds)
	}
	all, err := c.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var nodes []store.Node
	skipped := map[string]string{}
	for _, n := range all {
		switch {
		case n.Removed() || n.Edge():
		case !n.Ready(now):
			skipped[n.Name] = "not ready"
		case !n.Has(types.FeatureNetTest):
			skipped[n.Name] = "runs " + n.Version + ", from before network tests"
		case n.AgentToken == "":
			skipped[n.Name] = "has no agent API"
		default:
			nodes = append(nodes, n)
		}
	}
	if len(nodes) < 2 {
		msg := "a network test needs two nodes to test between"
		for _, name := range sortedKeys(skipped) {
			msg += fmt.Sprintf("; %s %s", name, skipped[name])
		}
		return nil, &NetTestRefused{msg}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	test := &types.NetTest{Started: now, Seconds: seconds}
	if len(skipped) > 0 {
		test.Skipped = skipped
	}
	for _, n := range nodes {
		test.Nodes = append(test.Nodes, n.Name)
	}
	c.nettest.mu.Lock()
	defer c.nettest.mu.Unlock()
	if r := c.nettest.running; r != nil {
		return nil, &NetTestRefused{fmt.Sprintf("a network test is running: %d of %d links done", r.Done, r.Total)}
	}
	c.nettest.running = &types.NetTestProgress{Started: now, Total: len(nodes) * (len(nodes) - 1), Test: test}
	progress := *c.nettest.running
	go c.runNetTest(context.WithoutCancel(ctx), nodes, test)
	return &progress, nil
}

func (c *Controller) runNetTest(ctx context.Context, nodes []store.Node, test *types.NetTest) {
	for _, from := range nodes {
		for _, to := range nodes {
			if from.Name == to.Name {
				continue
			}
			c.nettest.mu.Lock()
			c.nettest.running.From, c.nettest.running.To = from.Name, to.Name
			c.nettest.mu.Unlock()
			link := c.probeLink(ctx, from, to, test.Seconds)
			c.nettest.mu.Lock()
			test.Links = append(test.Links, link)
			c.nettest.running.Done++
			c.nettest.mu.Unlock()
		}
	}
	test.Finished = time.Now()

	c.nettest.mu.Lock()
	c.nettest.running = nil
	err := c.loadNetTests()
	if err == nil {
		c.nettest.tests = append([]types.NetTest{*test}, c.nettest.tests...)
		if len(c.nettest.tests) > netTestKeep {
			c.nettest.tests = c.nettest.tests[:netTestKeep]
		}
		err = c.saveNetTests()
	}
	c.nettest.mu.Unlock()
	if err != nil {
		c.Log.Warn("keeping the network test", "err", err)
	}
	c.Store.AddEvent(ctx, "node", "", "", "%s", nettest.Summary(test))
}

// probeLink asks from's agent to measure its link to to's.
func (c *Controller) probeLink(ctx context.Context, from, to store.Node, seconds float64) types.NetLink {
	link := types.NetLink{From: from.Name, To: to.Name}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds*float64(time.Second))+time.Duration(NetTestPings)*10*time.Millisecond+45*time.Second)
	defer cancel()
	body, _ := json.Marshal(types.NetProbe{Target: c.agentAddr(to), Seconds: seconds, Pings: NetTestPings})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.agentAddr(from)+"/v1/nettest/probe", bytes.NewReader(body))
	if err != nil {
		link.Error = err.Error()
		return link
	}
	req.Header.Set("Authorization", "Bearer "+from.AgentToken)
	resp, err := netProbeClient.Do(req)
	if err != nil {
		link.Error = "reaching " + from.Name + ": " + err.Error()
		return link
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		link.Error = from.Name + " answered " + resp.Status
		return link
	}
	var res types.NetProbeResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		link.Error = err.Error()
		return link
	}
	link.Mbps, link.Ping = res.Mbps, res.Ping
	switch {
	case res.Error != "" && res.PingError != "":
		link.Error = res.Error + "; ping: " + res.PingError
	case res.Error != "":
		link.Error = res.Error
	case res.PingError != "":
		link.Error = "ping: " + res.PingError
	}
	return link
}

func (c *Controller) netTestsPath() string { return filepath.Join(c.DataDir, "nettests.json") }

// loadNetTests reads the kept tests the first time (under nettest.mu).
func (c *Controller) loadNetTests() error {
	if c.nettest.tests != nil || c.DataDir == "" {
		return nil
	}
	b, err := os.ReadFile(c.netTestsPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		c.nettest.tests = []types.NetTest{}
		return nil
	case err != nil:
		return err
	}
	var tests []types.NetTest
	if err := json.Unmarshal(b, &tests); err != nil {
		return fmt.Errorf("%s: %w", c.netTestsPath(), err)
	}
	c.nettest.tests = tests
	return nil
}

func (c *Controller) saveNetTests() error {
	if c.DataDir == "" {
		return nil
	}
	b, err := json.Marshal(c.nettest.tests)
	if err != nil {
		return err
	}
	tmp := c.netTestsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.netTestsPath())
}
