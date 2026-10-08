package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// Requests streams the HTTP requests every node's proxy handles, or only
// one app's. Each is labelled with the node that received it and the
// instance (and its node) that answered. Without follow, the last tail
// requests are returned in time order.
func (c *Controller) Requests(ctx context.Context, app string, tail int, follow bool, fn func(types.Request)) error {
	nodes, err := c.liveNodes(ctx)
	if err != nil {
		return err
	}
	res := &upstreams{c: c}
	if err := res.refresh(ctx); err != nil {
		return err
	}
	if follow {
		go res.keepFresh(ctx)
	}
	q := url.Values{"app": {app}, "tail": {strconv.Itoa(tail)}}
	if follow {
		q.Set("follow", "true")
	}
	var mu sync.Mutex
	var all []types.Request
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.nodeLogs(ctx, n, "/v1/requests", q, func(line string) {
				var r types.Request
				if json.Unmarshal([]byte(line), &r) != nil {
					return
				}
				r.Node = n.Name
				res.label(&r)
				mu.Lock()
				defer mu.Unlock()
				if follow {
					fn(r)
				} else {
					all = append(all, r)
				}
			})
		}()
	}
	wg.Wait()
	if !follow {
		sort.SliceStable(all, func(i, j int) bool { return all[i].At.Before(all[j].At) })
		if len(all) > tail {
			all = all[len(all)-tail:]
		}
		for _, r := range all {
			fn(r)
		}
	}
	return nil
}

func (c *Controller) liveNodes(ctx context.Context) ([]store.Node, error) {
	nodes, err := c.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var live []store.Node
	for _, n := range nodes {
		if n.Ready(now) {
			live = append(live, n)
		}
	}
	return live, nil
}

// upstreams maps an instance's IP:port to its name and node.
type upstreams struct {
	c  *Controller
	mu sync.Mutex
	m  map[string][2]string
}

func (u *upstreams) refresh(ctx context.Context) error {
	insts, err := u.c.Store.Instances(ctx, "")
	if err != nil {
		return err
	}
	m := map[string][2]string{}
	for _, in := range insts {
		m[fmt.Sprintf("%s:%d", in.IP, in.Port)] = [2]string{in.Name(), in.Node}
	}
	u.mu.Lock()
	u.m = m
	u.mu.Unlock()
	return nil
}

func (u *upstreams) keepFresh(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.refresh(ctx)
		}
	}
}

func (u *upstreams) label(r *types.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if v, ok := u.m[r.Upstream]; ok {
		r.Instance, r.InstanceNode = v[0], v[1]
	}
}

// RouterLine renders a request the way "jokku logs" shows it, alongside the
// app's own output.
func RouterLine(r types.Request) string {
	line := fmt.Sprintf("%s app[router]: method=%s path=%q host=%s status=%d duration=%s bytes=%d",
		r.At.UTC().Format("2006-01-02T15:04:05.000000Z"), r.Method, r.Path, r.Host, r.Status, durationMS(r.DurationMS), r.Bytes)
	if r.Instance != "" {
		line += " instance=" + r.Instance + "@" + r.InstanceNode
	}
	if r.Node != "" {
		line += " via=" + r.Node
	}
	if r.Client != "" {
		line += " client=" + r.Client
	}
	return line
}

func durationMS(ms float64) string {
	if ms < 10 {
		return strconv.FormatFloat(ms, 'f', 1, 64) + "ms"
	}
	return strconv.FormatFloat(ms, 'f', 0, 64) + "ms"
}
