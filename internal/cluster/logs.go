package cluster

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wes/jokku/internal/journal"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// agentClient talks to agents' APIs over the mesh. Requests to a dead node
// fail after a few seconds instead of hanging a whole "jokku logs".
var agentClient = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 10 * time.Second}}

// Logs streams an app's log lines from every node that is up. Without
// Follow, lines are merged by timestamp and the last o.Tail returned; with
// Follow, lines are passed on as they arrive.
func (c *Controller) Logs(ctx context.Context, app string, o types.LogOptions, line func(string)) error {
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	var live []store.Node
	for _, n := range nodes {
		if n.Ready(now) {
			live = append(live, n)
		}
	}
	q := url.Values{"app": {app}, "tail": {strconv.Itoa(o.Tail)}, "process": {o.Process}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var lines []string
	var errs []string
	if o.Follow {
		q.Set("follow", "true")
	}
	for _, n := range live {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := c.nodeLogs(ctx, n, "/v1/logs", q, func(l string) {
				mu.Lock()
				defer mu.Unlock()
				if o.Follow {
					line(l)
				} else {
					lines = append(lines, l)
				}
			})
			if err != nil && ctx.Err() == nil {
				mu.Lock()
				errs = append(errs, n.Name+": "+err.Error())
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if !o.Follow {
		sort.Strings(lines) // lines start with an RFC 3339 UTC timestamp
		if len(lines) > o.Tail {
			lines = lines[len(lines)-o.Tail:]
		}
		for _, l := range lines {
			line(l)
		}
	}
	if len(errs) > 0 && len(errs) == len(live) {
		return fmt.Errorf("could not read logs: %s", strings.Join(errs, "; "))
	}
	return nil
}

// InstanceLogs returns the last n lines an instance printed, wherever it
// runs, for deploy failure messages.
func (c *Controller) InstanceLogs(ctx context.Context, in store.Instance, n int) []string {
	node, err := c.Store.Node(ctx, in.Node)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var lines []string
	c.nodeLogs(ctx, *node, "/v1/instance-logs", url.Values{"id": {in.ID}, "tail": {strconv.Itoa(n)}}, func(l string) {
		lines = append(lines, l)
	})
	return lines
}

func (c *Controller) nodeLogs(ctx context.Context, n store.Node, path string, q url.Values, line func(string)) error {
	if n.Name == c.Self {
		return journal.Serve(ctx, path, q, line)
	}
	_, meshIP := NodeSubnet(c.ClusterCIDR, n.SubnetIndex)
	u := fmt.Sprintf("http://%s:%d%s?%s", meshIP, AgentPort, path, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.AgentToken)
	resp, err := agentClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent answered %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line(sc.Text())
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}
