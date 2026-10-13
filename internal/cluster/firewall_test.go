package cluster_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/types"
)

// postFirewall reports what n's proxy's firewall did, the way it does.
func (h *harness) postFirewall(n *node, rep types.FirewallReport) error {
	b, _ := json.Marshal(rep)
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/v1/agent/firewall", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+n.token)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cluster.PinnedTLS(h.pin)}}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("firewall report: %s", resp.Status)
	}
	return nil
}

func (p *fakeProxy) firewall() *types.ProxyFirewall {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ps == nil {
		return nil
	}
	return p.ps.Firewall
}

func (h *harness) events(text string) int {
	events, _ := h.st.Events(h.ctx, "", 200)
	n := 0
	for _, e := range events {
		if strings.Contains(e.Message, text) {
			n++
		}
	}
	return n
}

func TestFirewallRulesReachTheProxies(t *testing.T) {
	h := newHarness(t)
	w1 := h.join("w1")
	h.deploy("shop", 1)
	// On from the start, with the defaults.
	eventually(t, 5*time.Second, "the firewall's rules reach w1", func() error {
		fw := w1.proxy.firewall()
		if fw == nil || !reflect.DeepEqual(*fw, types.ProxyFirewall{NotFound: 20, WindowSec: 600, BanSec: 3600, Rate: 50, Burst: 300}) {
			return fmt.Errorf("w1's firewall: %+v", fw)
		}
		return nil
	})

	off := false
	if _, err := h.client.SetFirewall(h.ctx, "shop", types.FirewallPatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "shop's routes skip the firewall", func() error {
		if r := w1.proxy.route("shop"); r == nil || !r.NoFirewall {
			return fmt.Errorf("shop's route: %+v", r)
		}
		return nil
	})

	notFound, window := 30, "5m"
	allow := []string{"203.0.113.0/24", "198.51.100.7/32", "2001:db8::/48"}
	s, err := h.client.SetFirewall(h.ctx, "", types.FirewallPatch{NotFound: &notFound, Window: &window, Allow: &allow})
	if err != nil {
		t.Fatal(err)
	}
	if s.NotFound != 30 || s.Window != "5m" || s.Ban != "1h" || !reflect.DeepEqual(s.Allow, []string{"203.0.113.0/24", "198.51.100.7", "2001:db8::/48"}) ||
		!reflect.DeepEqual(s.OffForApps, []string{"shop"}) {
		t.Errorf("settings: %+v", s)
	}
	eventually(t, 5*time.Second, "the new rules reach w1", func() error {
		if fw := w1.proxy.firewall(); fw == nil || fw.NotFound != 30 || fw.WindowSec != 300 || len(fw.Allow) != 3 {
			return fmt.Errorf("w1's firewall: %+v", fw)
		}
		return nil
	})

	// Out of bounds, or a rule for one app.
	short, bad := "10s", []string{"example.com"}
	for _, p := range []types.FirewallPatch{{Window: &short}, {Allow: &bad}, {NotFound: new(int)}} {
		if _, err := h.client.SetFirewall(h.ctx, "", p); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	if _, err := h.client.SetFirewall(h.ctx, "shop", types.FirewallPatch{NotFound: &notFound}); err == nil {
		t.Error("accepted a rule for one app")
	}

	if _, err := h.client.SetFirewall(h.ctx, "", types.FirewallPatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the firewall comes off", func() error {
		if fw := w1.proxy.firewall(); fw != nil {
			return fmt.Errorf("w1's firewall: %+v", fw)
		}
		return nil
	})
}

func TestFirewallReportsBansAndUnbans(t *testing.T) {
	h := newHarness(t)
	w1, w2 := h.join("w1"), h.join("w2")
	h.deploy("shop", 1)
	now := time.Now().UTC()
	ban := types.FirewallBan{Addr: "203.0.113.7", App: "shop", Host: "shop.example.test", Paths: []string{"/.env", "/.git/config"},
		At: now.Add(-10 * time.Second), Until: now.Add(time.Hour - 10*time.Second), Blocked: 40}
	report := types.FirewallReport{Bans: []types.FirewallBan{ban},
		Counts:   map[string]types.FirewallCounts{"shop": {Requests: 60, Blocked: 40, NotFound: 20, Bans: 1}, "": {Blocked: 5}},
		Watching: []types.FirewallSuspect{{Addr: "198.51.100.3", App: "shop", NotFound: 7, Last: now}},
		Limited:  map[string]types.FirewallSlowed{"198.51.100.9": {App: "shop", Limited: 12, Last: now}}}
	if err := h.postFirewall(w1, report); err != nil {
		t.Fatal(err)
	}
	// w2 banned it too; reporting again doesn't make a second event.
	ban2 := ban
	ban2.At, ban2.Blocked = now, 2
	if err := h.postFirewall(w2, types.FirewallReport{Bans: []types.FirewallBan{ban2}}); err != nil {
		t.Fatal(err)
	}
	again := report
	again.Counts, again.Limited = nil, nil
	if err := h.postFirewall(w1, again); err != nil {
		t.Fatal(err)
	}
	fw, err := h.client.Firewall(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fw.Bans) != 1 {
		t.Fatalf("bans: %+v", fw.Bans)
	}
	b := fw.Bans[0]
	if b.Addr != "203.0.113.7" || b.Blocked != 42 || !reflect.DeepEqual(b.Nodes, []string{"w1", "w2"}) || !b.At.Equal(ban.At) || b.App != "shop" {
		t.Errorf("ban: %+v", b)
	}
	if fw.Hour.Blocked != 45 || fw.Hour.Requests != 60 || fw.Hour.Bans != 1 || fw.Minute.Blocked != 45 || fw.Day.Blocked != 45 {
		t.Errorf("counts: minute %+v hour %+v day %+v", fw.Minute, fw.Hour, fw.Day)
	}
	if len(fw.Sites) != 2 || fw.Sites[0].App != "shop" || fw.Sites[0].Hour.Blocked != 40 {
		t.Errorf("sites: %+v", fw.Sites)
	}
	if len(fw.Watching) != 1 || fw.Watching[0].Node != "w1" || len(fw.Slowed) != 1 || fw.Slowed[0].Limited != 12 {
		t.Errorf("watching %+v, slowed %+v", fw.Watching, fw.Slowed)
	}
	if n := h.events("banned 203.0.113.7 for 1h: 20 pages not found within 10m: /.env /.git/config"); n != 2 {
		t.Errorf("%d ban events, want one each from w1 and w2", n)
	}

	// A ban first heard of long after it began (the control node
	// restarted) is no news.
	old := types.FirewallBan{Addr: "203.0.113.8", At: now.Add(-30 * time.Minute), Until: now.Add(30 * time.Minute)}
	if err := h.postFirewall(w2, types.FirewallReport{Bans: []types.FirewallBan{ban2, old}}); err != nil {
		t.Fatal(err)
	}
	if h.events("banned 203.0.113.8") != 0 {
		t.Error("an event for a ban from before the control node started")
	}

	had, err := h.client.Unban(h.ctx, "203.0.113.7")
	if err != nil || !had {
		t.Fatalf("unban: %v %v", had, err)
	}
	if had, err := h.client.Unban(h.ctx, "2001:db8:1:2::77"); err != nil || had {
		t.Fatalf("unban of an address without a ban: %v %v", had, err)
	}
	if _, err := h.client.Unban(h.ctx, "203.0.113.0/24"); err == nil {
		t.Error("unbanned a range")
	}
	eventually(t, 5*time.Second, "the proxies are told", func() error {
		fw := w1.proxy.firewall()
		if fw == nil || fw.Unbanned["203.0.113.7"].IsZero() || fw.Unbanned["2001:db8:1:2::/64"].IsZero() {
			return fmt.Errorf("w1's firewall: %+v", fw)
		}
		return nil
	})
	fw, _ = h.client.Firewall(h.ctx)
	if len(fw.Bans) != 1 || fw.Bans[0].Addr != "203.0.113.8" || h.events("unbanned 203.0.113.7") != 1 {
		t.Errorf("after the unban: %+v", fw.Bans)
	}

	// Off, the bans the proxies remember don't count.
	off := false
	h.client.SetFirewall(h.ctx, "", types.FirewallPatch{Enabled: &off})
	h.postFirewall(w2, types.FirewallReport{Bans: []types.FirewallBan{old}})
	if fw, _ := h.client.Firewall(h.ctx); len(fw.Bans) != 0 || fw.Settings.Enabled {
		t.Errorf("off: %+v", fw)
	}
}

func TestRouterLineSaysWhatTheFirewallDid(t *testing.T) {
	line := cluster.RouterLine(types.Request{At: time.Unix(0, 0), Method: "GET", Path: "/.env", Host: "shop.test", Status: 403,
		Node: "edge1", Client: "203.0.113.7", Firewall: types.FirewallBanned})
	if !strings.HasSuffix(line, " via=edge1 client=203.0.113.7 firewall=banned") {
		t.Errorf("router line: %s", line)
	}
}
