package cluster

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/wes/jokku/internal/database"
	"github.com/wes/jokku/internal/mesh"
	"github.com/wes/jokku/internal/proxy"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// ExposePlugin holds what reaches an exposed database from outside
// (db:<engine>:expose): its domain, port, the client addresses let in and
// whether it stays on your network. The ingress nodes' and edges' proxies
// carry its connections as streams (types.ProxyStream), straight to its
// instance over the mesh.
const ExposePlugin = "expose"

// Exposure is how a database is reached from outside.
type Exposure struct {
	App    string
	Engine database.Engine
	Host   string
	Port   int
	Allow  []string // CIDRs; empty lets in any address
	LAN    bool     // only the ingress nodes carry it, not the edges
}

// Properties are what the store keeps of x.
func (x Exposure) Properties() map[string]string {
	lan := ""
	if x.LAN {
		lan = "true"
	}
	return map[string]string{"domain": x.Host, "port": strconv.Itoa(x.Port), "allow": strings.Join(x.Allow, ","), "lan": lan}
}

// UnexposeProperties clear an exposure.
var UnexposeProperties = map[string]string{"domain": "", "port": "", "allow": "", "lan": ""}

// LANRanges are the addresses a database exposed on your network (--lan)
// lets in unless told otherwise: the private ranges, Tailscale's and the
// nodes' own.
var LANRanges = []string{"10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "::1/128", "fc00::/7"}

// Exposures reads the exposed databases, by app. One that is misconfigured
// (which the API prevents) is left out rather than failing every node's
// state.
func Exposures(ctx context.Context, st *store.Store, apps []types.App) (map[string]Exposure, error) {
	out := map[string]Exposure{}
	for _, a := range apps {
		e, ok := database.Lookup(a.Kind)
		if !ok {
			continue
		}
		p, err := st.Properties(ctx, a.Name, ExposePlugin)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(p["port"])
		if p["domain"] == "" || err != nil {
			continue
		}
		x := Exposure{App: a.Name, Engine: e, Host: p["domain"], Port: port, LAN: p["lan"] == "true"}
		if p["allow"] != "" {
			x.Allow = strings.Split(p["allow"], ",")
		}
		out[a.Name] = x
	}
	return out, nil
}

// ReservedPorts are the TCP ports Jokku's nodes use themselves: ssh (for
// pushes), DNS, the proxies' HTTP and HTTPS, and the control node's API.
var ReservedPorts = func() map[int]bool {
	ports := map[int]bool{22: true, 53: true, proxy.RelayPort: true, 443: true}
	for _, p := range strings.Split(mesh.ProtectedPorts, ",") {
		n, _ := strconv.Atoi(p)
		ports[n] = true
	}
	return ports
}()

// CheckExposure completes x (the engine's own port when it has none) and
// checks it against the other exposures: a port carries one protocol, a
// plain stream has its port to itself, and a name on a shared port reaches
// one database. allow is the client addresses let in, as given.
func CheckExposure(x *Exposure, allow []string, others map[string]Exposure) error {
	if x.Port == 0 {
		x.Port = x.Engine.Port
	}
	if x.Port < 1 || x.Port > 65535 {
		return fmt.Errorf("invalid port %d", x.Port)
	}
	if ReservedPorts[x.Port] {
		return fmt.Errorf("port %d is one Jokku's nodes use themselves; choose another with --port", x.Port)
	}
	x.Allow = nil
	for _, a := range allow {
		p, err := netip.ParsePrefix(a)
		if err != nil {
			ip, ierr := netip.ParseAddr(a)
			if ierr != nil {
				return fmt.Errorf("%q is not an address or a range of them, like 203.0.113.7 or 203.0.113.0/24", a)
			}
			p = netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen())
		}
		x.Allow = append(x.Allow, p.Masked().String())
	}
	sort.Strings(x.Allow)
	for _, app := range sortedKeys(others) {
		o := others[app]
		if app == x.App || o.Port != x.Port {
			continue
		}
		switch {
		case x.Engine.Stream == types.StreamPlain || o.Engine.Stream == types.StreamPlain:
			return fmt.Errorf("port %d already carries %s's connections, and a %s database needs a port to itself (it can't be told apart by name): choose another with --port",
				x.Port, o.App, x.Engine.Name)
		case o.Engine.Stream != x.Engine.Stream:
			return fmt.Errorf("port %d already carries %s's connections, which are %s, not %s: choose another with --port",
				x.Port, o.App, o.Engine.Name, x.Engine.Name)
		case strings.EqualFold(o.Host, x.Host):
			return fmt.Errorf("%s:%d already reaches %s", x.Host, x.Port, o.App)
		}
	}
	return nil
}

// streams are the TCP routes a node's proxy carries: one per exposed
// database, to its running instance, except that an edge leaves out the
// ones kept on your network. A public domain gets a certificate from Let's
// Encrypt, which clients can verify (sslmode=verify-full); any other name,
// and any database on your network (whose name Let's Encrypt usually can't
// reach), gets the proxies' own.
func streams(exp map[string]Exposure, apps []types.App, insts []store.Instance, ready map[string]bool, edge bool) []types.ProxyStream {
	stopped := map[string]bool{}
	for _, a := range apps {
		stopped[a.Name] = a.Stopped
	}
	out := []types.ProxyStream{}
	for _, app := range sortedKeys(exp) {
		x := exp[app]
		if x.LAN && edge {
			continue
		}
		s := types.ProxyStream{App: app, Protocol: x.Engine.Stream, Host: x.Host, Port: x.Port, Upstreams: []string{}, Allow: x.Allow,
			ACME: !x.LAN && x.Engine.Stream != types.StreamPlain && proxy.PublicHost(x.Host)}
		if !stopped[app] {
			for _, in := range insts {
				if in.App == app && in.ProcessType == x.Engine.Process() && in.Desired == store.DesiredRunning &&
					in.State == store.StateHealthy && ready[in.Node] && in.IP != "" {
					s.Upstreams = append(s.Upstreams, fmt.Sprintf("%s:%d", in.IP, x.Engine.Port))
				}
			}
		}
		out = append(out, s)
	}
	return out
}
