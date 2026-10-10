package cluster

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// External apps are services Jokku routes to but doesn't run: Home
// Assistant or a NAS on the LAN, say. Their settings are properties of the
// app (plugin "external"): url, via (the node that forwards edge traffic
// to the target; the control node by default) and insecure.
const ExternalPlugin = "external"

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// External is an external app's target, ready to route.
type External struct {
	App      string
	TLS      bool // the target speaks HTTPS
	Target   netip.AddrPort
	Via      string // the node edges reach it through
	Insecure bool   // its certificate isn't checked
}

// ParseTarget checks an external app's URL: http or https, to an IPv4
// address and optional port. Edges reach the target through a node over the
// mesh, so it has to be an address, not a name only the LAN resolves.
func ParseTarget(raw string, cluster netip.Prefix) (tls bool, target netip.AddrPort, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false, netip.AddrPort{}, fmt.Errorf("invalid URL %q, expected e.g. http://192.168.1.50:8123", raw)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil || u.Fragment != "" {
		return false, netip.AddrPort{}, fmt.Errorf("the URL %q must be only a scheme, an address and a port, e.g. http://192.168.1.50:8123", raw)
	}
	addr, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		return false, netip.AddrPort{}, fmt.Errorf("%q is not an IP address: external apps are reached by address (edges can't resolve names on your network)", u.Hostname())
	}
	addr = addr.Unmap()
	switch {
	case !addr.Is4():
		return false, netip.AddrPort{}, errors.New("external apps need an IPv4 address")
	case addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast():
		return false, netip.AddrPort{}, fmt.Errorf("%s is not an address other machines can reach: use the machine's address on your network", addr)
	case cluster.IsValid() && cluster.Contains(addr):
		return false, netip.AddrPort{}, fmt.Errorf("%s is in the cluster network %s: route Jokku apps with domains:add instead", addr, cluster)
	case !addr.IsPrivate() && !cgnat.Contains(addr):
		// A public address is reachable from the edge itself, and one that
		// is a node's public address would send the mesh's own traffic
		// into the mesh.
		return false, netip.AddrPort{}, fmt.Errorf("%s is a public address; external apps are on your network (10.x, 172.16-31.x, 192.168.x or 100.64-127.x)", addr)
	}
	tls = u.Scheme == "https"
	port := 80
	if tls {
		port = 443
	}
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil || port < 1 || port > 65535 {
			return false, netip.AddrPort{}, fmt.Errorf("invalid port %q", p)
		}
	}
	return tls, netip.AddrPortFrom(addr, uint16(port)), nil
}

// externals reads the external apps' targets. One that is misconfigured
// (which the API prevents) is left out rather than failing every node's
// state. A target whose via node is gone or is an edge is reached through
// the control node.
func (c *Controller) externals(ctx context.Context, apps []types.App, nodes []store.Node) (map[string]External, error) {
	usable := map[string]bool{}
	for _, n := range nodes {
		usable[n.Name] = !n.Edge()
	}
	out := map[string]External{}
	for _, a := range apps {
		if a.Kind != types.KindExternal {
			continue
		}
		p, err := c.Store.Properties(ctx, a.Name, ExternalPlugin)
		if err != nil {
			return nil, err
		}
		tls, target, err := ParseTarget(p["url"], c.ClusterCIDR)
		if err != nil {
			continue
		}
		via := p["via"]
		if !usable[via] {
			via = c.Self
		}
		out[a.Name] = External{App: a.Name, TLS: tls, Target: target, Via: via, Insecure: p["insecure"] == "true"}
	}
	return out, nil
}
