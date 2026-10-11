package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/wes/jokku/internal/latency"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// Latency is how long requests took over the last hour and day: every live
// node's histograms added up (each node's agent keeps its proxy's), so the
// percentiles are the cluster's, not an average of nodes'.
func (c *Controller) Latency(ctx context.Context) (*types.Latency, error) {
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	var reports []latency.Report
	out := &types.Latency{}
	var wg sync.WaitGroup
	now := time.Now()
	for _, n := range nodes {
		if n.Removed() {
			continue
		}
		if !n.Ready(now) {
			out.Missing = append(out.Missing, n.Name)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, err := c.nodeLatency(ctx, n)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				out.Missing = append(out.Missing, n.Name)
				return
			}
			reports = append(reports, rep)
		}()
	}
	wg.Wait()
	sort.Strings(out.Missing)
	hour, day := map[string]latency.Histogram{}, map[string]latency.Histogram{}
	for _, rep := range reports {
		mergeApps(hour, rep.Hour)
		mergeApps(day, rep.Day)
	}
	out.Hour, out.Day = latencyWindow(hour), latencyWindow(day)
	return out, nil
}

func (c *Controller) nodeLatency(ctx context.Context, n store.Node) (latency.Report, error) {
	var rep latency.Report
	if n.Name == c.Self {
		if c.Durations == nil {
			return rep, fmt.Errorf("not recorded here")
		}
		return c.Durations.Report(time.Now()), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.agentAddr(n)+"/v1/latency", nil)
	if err != nil {
		return rep, err
	}
	req.Header.Set("Authorization", "Bearer "+n.AgentToken)
	resp, err := agentClient.Do(req)
	if err != nil {
		return rep, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return rep, fmt.Errorf("agent answered %s", resp.Status)
	}
	return rep, json.NewDecoder(resp.Body).Decode(&rep)
}

func mergeApps(into, from map[string]latency.Histogram) {
	for app, h := range from {
		if into[app] == nil {
			into[app] = latency.Histogram{}
		}
		into[app].Merge(h)
	}
}

func latencyWindow(apps map[string]latency.Histogram) types.LatencyWindow {
	all := latency.Histogram{}
	w := types.LatencyWindow{Apps: map[string]types.LatencyStats{}}
	for app, h := range apps {
		all.Merge(h)
		w.Apps[app] = latencyStats(h)
	}
	w.LatencyStats = latencyStats(all)
	return w
}

func latencyStats(h latency.Histogram) types.LatencyStats {
	return types.LatencyStats{Requests: h.Count(), P50: h.Percentile(50), P95: h.Percentile(95)}
}
