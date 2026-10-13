// Package proxy is the HTTP(S) router in front of apps: Caddy, embedded in the
// jokku binary and run as its own service ("jokku proxy"), so restarting or
// updating the jokku daemon never interrupts traffic.
//
// The daemon computes routes and loads them through Caddy's admin API on a
// unix socket. The last config is also saved to disk, so a restarted proxy
// serves the same routes immediately.
package proxy

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	"github.com/wes/jokku/internal/types"
)

const (
	// AdminSocket is Caddy's admin API, reachable only by root.
	AdminSocket = "/run/jokku-proxy/admin.sock"
	// Version changes when a release needs the proxy process restarted;
	// "jokku setup" compares it to the running one. 2: the jokku_auth and
	// jokku_strip_identity handlers, which configs from now on use. 3: the
	// jokku_wake handler and jokku upstreams, for apps that sleep. 4: the
	// layer4 app, for streams. 5: the jokku_firewall handler.
	Version = "5"
)

// Route sends requests for Hosts to Upstreams (IP:port of healthy web
// instances, or an external app's target). An empty Upstreams answers 502.
type Route struct {
	App       string
	Hosts     []string
	Upstreams []string
	TLS       bool
	// UpstreamTLS: the upstreams speak HTTPS, with any certificate if
	// Insecure.
	UpstreamTLS, Insecure bool
	// Auth puts a login in front of the route (with Settings.Auth).
	Auth *types.RouteAuth
	// Sleeps: the app sleeps when idle, and a request wakes it (after the
	// login, if it has one).
	Sleeps bool
	// NoCompress: the app opted out of compression, and NoFirewall of the
	// firewall.
	NoCompress, NoFirewall bool
	// Relayed, on an edge: the upstreams are nodes' proxies (RelayPort),
	// which pass requests on to the app's instances.
	Relayed bool
}

type Settings struct {
	Routes      []Route
	Email       string // ACME account email; optional
	DataDir     string // certificates and the saved config
	HTTPPort    int
	HTTPSPort   int
	AdminSocket string // default AdminSocket
	// Storage keeps certificates on the control node (shared by every
	// ingress node); nil keeps them on local disk.
	Storage *ClusterStorage
	// Auth is what routes with a login need; with a login domain, that
	// domain gets the login page every users login shares.
	Auth *types.ProxyAuth
	// Edges are the subnets of the edges that send requests through this
	// node (see relayRoute).
	Edges []string
	// Streams are the TCP routes (see streamServers).
	Streams []types.ProxyStream
	// Firewall is the firewall's rules (see FirewallHandler); nil for none.
	Firewall *types.ProxyFirewall
}

// Relaying
//
// An edge sends a request for an app through the proxy of a node that runs
// it, rather than straight to an instance, so the response is compressed
// before it crosses the mesh: from a home lab, that trip goes up a home
// connection, often the slowest link of all. The node's plain HTTP server
// answers requests from the edges' subnets (and only those) with relay
// routes, which skip the login (the edge did it) and the access log (the
// edge writes it), and report the instance that answered back to the edge
// in RelayHeader, for the edge's log.

// RelayPort is where an edge reaches a node's proxy, over the mesh.
const RelayPort = 80

// RelayHeader carries the instance that answered a relayed request back to
// the edge, which drops it.
const RelayHeader = "X-Jokku-Upstream"

// ConfigPath is where the last loaded config is saved.
func ConfigPath(dataDir string) string { return filepath.Join(dataDir, "proxy", "config.json") }

// Config renders the Caddy JSON config. TLS hosts get certificates
// automatically and their plain-HTTP requests are redirected; other hosts are
// served over plain HTTP. Unknown hosts get a 404.
func Config(s Settings) ([]byte, error) {
	routes := append([]Route(nil), s.Routes...)
	// Exact hosts before wildcards, then by app for a stable config.
	sort.SliceStable(routes, func(i, j int) bool {
		wi, wj := hasWildcard(routes[i].Hosts), hasWildcard(routes[j].Hosts)
		if wi != wj {
			return !wi
		}
		return routes[i].App < routes[j].App
	})

	var httpRoutes, httpsRoutes []any
	var tlsHosts []string
	portal := portalHandler(s)
	for _, r := range routes {
		if len(r.Hosts) == 0 {
			continue
		}
		handle := tags(r)
		if !r.NoFirewall {
			handle = append(handle, firewallHandler(r.App, s)...)
		}
		if !r.NoCompress {
			handle = append(handle, compress)
		}
		handle = append(handle, map[string]any{"handler": "jokku_strip_identity"})
		if r.Auth != nil && s.Auth != nil {
			handle = append(handle, map[string]any{"handler": "jokku_auth", "app": r.App, "auth": r.Auth, "global": s.Auth,
				"portal_url": portal.url})
		}
		if r.Sleeps {
			handle = append(handle, wake(r, s))
		}
		route := map[string]any{
			"match":    []any{map[string]any{"host": r.Hosts}},
			"handle":   append(handle, handler(r)),
			"terminal": true,
		}
		if r.TLS {
			httpsRoutes = append(httpsRoutes, route)
			tlsHosts = append(tlsHosts, r.Hosts...)
		} else {
			httpRoutes = append(httpRoutes, route)
		}
	}
	if portal.route != nil {
		if portal.tls {
			httpsRoutes = append([]any{portal.route}, httpsRoutes...)
			tlsHosts = append(tlsHosts, s.Auth.LoginDomain)
		} else {
			httpRoutes = append([]any{portal.route}, httpRoutes...)
		}
	}
	if len(s.Edges) > 0 {
		var relay []any
		for _, r := range routes {
			if len(r.Hosts) > 0 {
				relay = append(relay, relayRoute(r, s))
			}
		}
		// A host this node doesn't route (yet: the edge heard of it first)
		// gets an error, not the public routes' redirect to HTTPS, which the
		// edge would hand the visitor, who'd come straight back.
		relay = append(relay, map[string]any{
			"match": []any{map[string]any{"remote_ip": map[string]any{"ranges": s.Edges}}},
			"handle": []any{
				map[string]any{"handler": "vars", "log_skip": true},
				map[string]any{"handler": "static_response", "status_code": 502, "body": "This server doesn't route {http.request.host} yet.\n"},
			},
			"terminal": true,
		})
		httpRoutes = append(relay, httpRoutes...)
	}
	if len(tlsHosts) > 0 {
		httpRoutes = append(httpRoutes, map[string]any{
			"match": []any{map[string]any{"host": tlsHosts}},
			"handle": []any{map[string]any{
				"handler":     "static_response",
				"status_code": 308,
				"headers":     map[string]any{"Location": []string{"https://{http.request.host}{http.request.uri}"}},
			}},
			"terminal": true,
		})
	}
	// Scanners often ask for hosts no app serves, by address.
	notFound := map[string]any{"handle": append(firewallHandler("", s), map[string]any{
		"handler": "static_response", "status_code": 404,
		"body": "No app is configured for this domain.\n",
	})}
	httpRoutes = append(httpRoutes, notFound)
	httpsRoutes = append(httpsRoutes, notFound)

	httpServer := map[string]any{
		"listen": []string{fmt.Sprintf(":%d", s.HTTPPort)},
		"routes": httpRoutes,
		"errors": unreachable,
		"logs":   map[string]any{},
	}
	if len(s.Edges) > 0 {
		// The edges' X-Forwarded-* headers (the visitor's address, and that
		// they came over HTTPS) go on to the app as they are.
		httpServer["trusted_proxies"] = map[string]any{"source": "static", "ranges": s.Edges}
	}
	servers := map[string]any{"http": httpServer}
	if len(tlsHosts) > 0 {
		servers["https"] = map[string]any{
			"listen": []string{fmt.Sprintf(":%d", s.HTTPSPort)},
			"routes": httpsRoutes,
			"errors": unreachable,
			"logs":   map[string]any{},
			// The http server above already redirects, after ACME challenges
			// (which Caddy answers before any route).
			"automatic_https": map[string]any{"disable_redirects": true},
		}
	}
	apps := map[string]any{
		"http": map[string]any{
			"http_port":  s.HTTPPort,
			"https_port": s.HTTPSPort,
			"servers":    servers,
		},
	}
	var acme, internal []string
	if len(s.Streams) > 0 {
		var servers map[string]any
		servers, acme, internal = streamServers(s.Streams)
		apps["layer4"] = map[string]any{"servers": servers}
	}
	tlsApp := map[string]any{}
	var policies []any
	if len(internal) > 0 {
		// Names Let's Encrypt can't reach get certificates from the proxy's
		// own CA, which nothing is told to trust.
		policies = append(policies, map[string]any{"subjects": internal, "issuers": []any{map[string]any{"module": "internal"}}})
		apps["pki"] = map[string]any{"certificate_authorities": map[string]any{"local": map[string]any{"install_trust": false}}}
	}
	if s.Email != "" {
		policies = append(policies, map[string]any{"issuers": []any{map[string]any{"module": "acme", "email": s.Email}}})
	}
	if len(policies) > 0 {
		tlsApp["automation"] = map[string]any{"policies": policies}
	}
	// The http app gets its hosts' certificates itself; streams' are
	// listed. Their ACME challenges are answered on the HTTP port, like
	// the http app's.
	if names := append(acme, internal...); len(names) > 0 {
		tlsApp["certificates"] = map[string]any{"automate": names}
	}
	if len(tlsApp) > 0 {
		apps["tls"] = tlsApp
	}
	admin := s.AdminSocket
	if admin == "" {
		admin = AdminSocket
	}
	cfg := map[string]any{
		"admin": map[string]any{
			"listen": "unix/" + admin,
			"config": map[string]any{"persist": false},
		},
		"storage": storageConfig(s),
		"logging": map[string]any{"logs": map[string]any{
			"default": map[string]any{"level": "WARN", "exclude": []string{"http.log.access"}},
			// One JSON line per request on stderr, so the journal: the
			// router lines in "jokku logs" and the traffic view in top.
			"access": map[string]any{
				"include": []string{"http.log.access"},
				"writer":  map[string]any{"output": "stderr"},
				"encoder": map[string]any{
					"format": "filter",
					"wrap":   map[string]any{"format": "json"},
					"fields": map[string]any{
						"request>headers": map[string]any{"filter": "delete"},
						"resp_headers":    map[string]any{"filter": "delete"},
						"request>tls":     map[string]any{"filter": "delete"},
					},
				},
			},
		}},
		"apps": apps,
	}
	return json.MarshalIndent(cfg, "", "  ")
}

func storageConfig(s Settings) map[string]any {
	local := filepath.Join(s.DataDir, "proxy", "data")
	if s.Storage == nil {
		return map[string]any{"module": "file_system", "root": local}
	}
	return map[string]any{"module": "jokku", "url": s.Storage.URL, "token": s.Storage.Token, "pin": s.Storage.Pin,
		"cache": filepath.Join(s.DataDir, "proxy", "mirror"), "legacy": local}
}

// Applier loads routes from the control node into this node's proxy.
type Applier struct {
	DataDir     string
	AdminSocket string
	Storage     *ClusterStorage
	// HTTPPort and HTTPSPort are RelayPort and 443 when 0 (tests set them).
	HTTPPort, HTTPSPort int

	// The streams that last failed to load, why, and when: tried again
	// after streamRetry rather than on every pass.
	failed   string
	failErr  error
	failedAt time.Time
}

const streamRetry = time.Minute

func (a *Applier) Apply(ctx context.Context, ps *types.ProxyState) error {
	routes := make([]Route, len(ps.Routes))
	for i, r := range ps.Routes {
		routes[i] = Route{App: r.App, Hosts: r.Hosts, Upstreams: r.Upstreams, TLS: r.TLS,
			UpstreamTLS: r.UpstreamTLS, Insecure: r.Insecure, Auth: r.Auth, Sleeps: r.Sleeps,
			NoCompress: r.NoCompress, NoFirewall: r.NoFirewall, Relayed: r.Relayed}
	}
	s := Settings{
		Routes: routes, Email: ps.Email, DataDir: a.DataDir, HTTPPort: cmp.Or(a.HTTPPort, RelayPort), HTTPSPort: cmp.Or(a.HTTPSPort, 443),
		AdminSocket: a.AdminSocket, Storage: a.Storage, Auth: ps.Auth, Edges: ps.Edges, Streams: ps.Streams, Firewall: ps.Firewall,
	}
	err := a.apply(ctx, s, ps)
	if err == nil || s.Firewall == nil {
		return err
	}
	// A proxy a release behind (until "jokku setup" restarts it) has no
	// firewall to load: the sites mustn't go down with it.
	s.Firewall = nil
	if a.apply(ctx, s, ps) != nil {
		return err
	}
	return fmt.Errorf("loaded the routes without the firewall: %w", err)
}

// apply loads s, and its streams if they load.
func (a *Applier) apply(ctx context.Context, s Settings, ps *types.ProxyState) error {
	streams, _ := json.Marshal(ps.Streams)
	if len(ps.Streams) > 0 && string(streams) == a.failed && time.Since(a.failedAt) < streamRetry {
		s.Streams = nil
		if err := a.load(ctx, s); err != nil {
			return err
		}
		return a.failErr
	}
	err := a.load(ctx, s)
	if err == nil || len(ps.Streams) == 0 {
		return err
	}
	// A port this node can't listen on (something else has it) must not
	// hold up the web routes, nor must a proxy a release behind, which
	// has no layer4 app until "jokku setup" restarts it.
	s.Streams = nil
	if werr := a.load(ctx, s); werr != nil {
		return err
	}
	a.failed, a.failedAt = string(streams), time.Now()
	a.failErr = fmt.Errorf("loaded the web routes without the TCP ones (db:*:expose): %w", err)
	return a.failErr
}

func (a *Applier) load(ctx context.Context, s Settings) error {
	cfg, err := Config(s)
	if err != nil {
		return err
	}
	return Load(ctx, a.AdminSocket, a.DataDir, cfg)
}

// compress encodes the text an app sends uncompressed (HTML, CSS, JS, JSON,
// SVG and the like), with zstd, or gzip for clients without it. Caddy leaves
// alone what is already encoded, under 512 bytes, partial (a range),
// marked no-transform, or a websocket, and streams server-sent events.
var compress = map[string]any{
	"handler":   "encode",
	"encodings": map[string]any{"zstd": map[string]any{}, "gzip": map[string]any{}},
	"prefer":    []string{"zstd", "gzip"},
}

// tags label each request's access log line with its app and the instance
// that answered: through a relay, the one the node says.
func tags(r Route) []any {
	upstream := "{http.reverse_proxy.upstream.hostport}"
	if r.Relayed {
		upstream = "{http.reverse_proxy.header." + RelayHeader + "}"
	}
	return []any{
		map[string]any{"handler": "log_append", "key": LogApp, "value": r.App},
		map[string]any{"handler": "log_append", "key": LogUpstream, "value": upstream},
	}
}

// firewallHandler is the firewall in front of app's routes: none without
// Settings.Firewall.
func firewallHandler(app string, s Settings) []any {
	if s.Firewall == nil {
		return nil
	}
	h := map[string]any{"handler": "jokku_firewall", "app": app, "rules": s.Firewall}
	if s.Storage != nil {
		h["control"] = Control{URL: s.Storage.URL, Token: s.Storage.Token, Pin: s.Storage.Pin}
	}
	return []any{h}
}

// wake holds a request to a sleeping app until it is awake.
func wake(r Route, s Settings) map[string]any {
	w := map[string]any{"handler": "jokku_wake", "app": r.App}
	if s.Storage != nil {
		w["control"] = Control{URL: s.Storage.URL, Token: s.Storage.Token, Pin: s.Storage.Pin}
	}
	return w
}

// relayRoute answers the requests an edge sends through this node for r's
// hosts: compressed here, and with the edge's forwarded headers passed on
// unchanged, so the app sees the visitor as the edge saw them.
func relayRoute(r Route, s Settings) map[string]any {
	handle := []any{map[string]any{"handler": "vars", "log_skip": true}}
	if !r.NoCompress {
		handle = append(handle, compress)
	}
	if r.Sleeps {
		handle = append(handle, wake(r, s))
	}
	h := handler(r)
	if h["handler"] == "reverse_proxy" {
		h["headers"] = map[string]any{
			"request":  map[string]any{"set": map[string][]string{"X-Forwarded-For": {"{http.request.header.X-Forwarded-For}"}}},
			"response": map[string]any{"set": map[string][]string{RelayHeader: {"{http.reverse_proxy.upstream.hostport}"}}},
		}
	}
	return map[string]any{
		"match":    []any{map[string]any{"remote_ip": map[string]any{"ranges": s.Edges}, "host": r.Hosts}},
		"handle":   append(handle, h),
		"terminal": true,
	}
}

// Access log fields Jokku adds.
const (
	LogApp      = "jokku_app"
	LogUpstream = "jokku_upstream"
	LogFirewall = "jokku_firewall" // what the firewall did: types.FirewallBanned or FirewallLimited
)

func handler(r Route) map[string]any {
	if len(r.Upstreams) == 0 && !r.Sleeps {
		return map[string]any{
			"handler": "static_response", "status_code": 502,
			"body": "No running instances of " + r.App + ". Check: jokku ps:report " + r.App + "\n",
		}
	}
	transport := map[string]any{"protocol": "http", "dial_timeout": "2s"}
	if r.UpstreamTLS {
		tls := map[string]any{}
		if r.Insecure {
			tls["insecure_skip_verify"] = true
		}
		transport["tls"] = tls
	}
	// An instance on a node that just died stops answering before the
	// control node notices (up to 30s). Failing the dial fast and retrying
	// on another instance turns that into latency instead of errors, and
	// passive health checks then skip it until routes catch up.
	rp := map[string]any{
		"handler": "reverse_proxy",
		"load_balancing": map[string]any{
			"selection_policy": map[string]any{"policy": "round_robin"},
			"try_duration":     "5s",
			"try_interval":     "250ms",
		},
		"health_checks": map[string]any{"passive": map[string]any{"fail_duration": "30s", "max_fails": 1}},
		"transport":     transport,
	}
	if r.Sleeps {
		// A sleeping app's upstreams come from the newest config, for
		// requests that waited in an older one for it to wake.
		ups := append([]string{}, r.Upstreams...)
		rp["dynamic_upstreams"] = map[string]any{"source": "jokku", "app": r.App, "upstreams": ups}
	} else {
		upstreams := make([]any, len(r.Upstreams))
		for i, u := range r.Upstreams {
			upstreams[i] = map[string]any{"dial": u}
		}
		rp["upstreams"] = upstreams
	}
	if len(r.Upstreams) <= 1 {
		// With nowhere else to go, marking the one upstream down would only
		// turn a blip into 30 seconds of errors.
		delete(rp, "health_checks")
	}
	if r.Relayed {
		// The node's word for which instance answered is for the log here,
		// not the visitor.
		rp["headers"] = map[string]any{"response": map[string]any{"delete": []string{RelayHeader}}}
	}
	return rp
}

// portal is the login domain's route, and its address for the routes whose
// login sends users there.
type portalRoute struct {
	route map[string]any
	url   string
	tls   bool
}

func portalHandler(s Settings) portalRoute {
	if s.Auth == nil || s.Auth.LoginDomain == "" {
		return portalRoute{}
	}
	d := s.Auth.LoginDomain
	hosts := map[string]PortalHost{}
	for _, r := range s.Routes {
		if r.Auth == nil || r.Auth.Mode != types.AuthUsers {
			continue
		}
		for _, h := range r.Hosts {
			users := r.Auth.Users
			if users == nil {
				users = []string{}
			}
			hosts[strings.ToLower(h)] = PortalHost{Users: users, TLS: r.TLS}
		}
	}
	p := portalRoute{tls: PublicHost(d), url: "http://" + d}
	if p.tls {
		p.url = "https://" + d
	}
	handle := append(tags(Route{}), firewallHandler("", s)...)
	p.route = map[string]any{
		"match":    []any{map[string]any{"host": []string{d}}},
		"handle":   append(handle, map[string]any{"handler": "jokku_auth", "portal": true, "global": s.Auth, "hosts": hosts}),
		"terminal": true,
	}
	return p
}

// unreachable is the page for a request whose app didn't answer: the
// instances (or the nodes behind an edge) can't be reached right now.
var unreachable = map[string]any{"routes": []any{map[string]any{
	"match": []any{map[string]any{"expression": "{http.error.status_code} >= 502 && {http.error.status_code} <= 504"}},
	"handle": []any{map[string]any{
		"handler":     "static_response",
		"status_code": "{http.error.status_code}",
		"headers":     map[string]any{"Content-Type": []string{"text/html; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
		"body":        unreachablePage,
	}},
}}}

func hasWildcard(hosts []string) bool {
	for _, h := range hosts {
		if strings.HasPrefix(h, "*.") {
			return true
		}
	}
	return false
}

// PublicHost reports whether Let's Encrypt can reach a host: not an IP
// address, not localhost, and not an sslip.io / nip.io name for a private
// address (such as myapp.192.168.1.10.sslip.io).
func PublicHost(host string) bool {
	host = strings.TrimPrefix(host, "*.")
	if _, err := netip.ParseAddr(host); err == nil || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	for _, suffix := range []string{".sslip.io", ".nip.io"} {
		if name, ok := strings.CutSuffix(host, suffix); ok {
			labels := strings.Split(name, ".")
			if len(labels) >= 4 {
				ip, err := netip.ParseAddr(strings.Join(labels[len(labels)-4:], "."))
				if err == nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)) {
					return false
				}
			}
		}
	}
	return true
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Run is the "jokku proxy" service: Caddy with the saved config (or none yet),
// until ctx is done.
func Run(ctx context.Context, dataDir string) error {
	if err := os.MkdirAll(filepath.Dir(AdminSocket), 0o700); err != nil {
		return err
	}
	cfg, err := os.ReadFile(ConfigPath(dataDir))
	if err != nil {
		if cfg, err = Config(Settings{DataDir: dataDir, HTTPPort: 80, HTTPSPort: 443}); err != nil {
			return err
		}
	}
	if err := start(cfg); err != nil {
		return err
	}
	<-ctx.Done()
	return caddy.Stop()
}

// start loads the saved config. One whose streams can't load (a port
// another service took since, at boot say) still serves the web: the
// agent sends the streams again within a few minutes.
func start(cfg []byte) error {
	err := caddy.Load(cfg, true)
	if err == nil {
		return nil
	}
	if web, ok := webOnly(cfg); ok && caddy.Load(web, true) == nil {
		fmt.Fprintf(os.Stderr, "serving the web routes without the TCP ones, which didn't load: %v\n", err)
		return nil
	}
	return fmt.Errorf("starting caddy: %w", err)
}

// webOnly is cfg without its streams (the layer4 app); ok is false when it
// has none.
func webOnly(cfg []byte) (_ []byte, ok bool) {
	var c map[string]any
	if json.Unmarshal(cfg, &c) != nil {
		return nil, false
	}
	apps, _ := c["apps"].(map[string]any)
	if _, ok := apps["layer4"]; !ok {
		return nil, false
	}
	delete(apps, "layer4")
	b, err := json.Marshal(c)
	return b, err == nil
}

// Load sends a config to the proxy listening on adminSocket and saves it for
// restarts.
func Load(ctx context.Context, adminSocket, dataDir string, cfg []byte) error {
	c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", adminSocket)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/load", bytes.NewReader(cfg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("the proxy is not reachable (systemctl status jokku-proxy): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("the proxy rejected its config: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	path := ConfigPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, cfg, 0o600)
}
