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
	// "jokku setup" compares it to the running one.
	Version = "1"
)

// Route sends requests for Hosts to Upstreams (IP:port of healthy web
// instances). An empty Upstreams answers 502.
type Route struct {
	App       string
	Hosts     []string
	Upstreams []string
	TLS       bool
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
}

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
	for _, r := range routes {
		if len(r.Hosts) == 0 {
			continue
		}
		route := map[string]any{
			"match":    []any{map[string]any{"host": r.Hosts}},
			"handle":   append(tags(r.App), handler(r)),
			"terminal": true,
		}
		if r.TLS {
			httpsRoutes = append(httpsRoutes, route)
			tlsHosts = append(tlsHosts, r.Hosts...)
		} else {
			httpRoutes = append(httpRoutes, route)
		}
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
	notFound := map[string]any{"handle": []any{map[string]any{
		"handler": "static_response", "status_code": 404,
		"body": "No app is configured for this domain.\n",
	}}}
	httpRoutes = append(httpRoutes, notFound)
	httpsRoutes = append(httpsRoutes, notFound)

	servers := map[string]any{
		"http": map[string]any{
			"listen": []string{fmt.Sprintf(":%d", s.HTTPPort)},
			"routes": httpRoutes,
			"logs":   map[string]any{},
		},
	}
	if len(tlsHosts) > 0 {
		servers["https"] = map[string]any{
			"listen": []string{fmt.Sprintf(":%d", s.HTTPSPort)},
			"routes": httpsRoutes,
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
	if s.Email != "" {
		apps["tls"] = map[string]any{"automation": map[string]any{"policies": []any{
			map[string]any{"issuers": []any{map[string]any{"module": "acme", "email": s.Email}}},
		}}}
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
}

func (a *Applier) Apply(ctx context.Context, ps *types.ProxyState) error {
	routes := make([]Route, len(ps.Routes))
	for i, r := range ps.Routes {
		routes[i] = Route{App: r.App, Hosts: r.Hosts, Upstreams: r.Upstreams, TLS: r.TLS}
	}
	cfg, err := Config(Settings{
		Routes: routes, Email: ps.Email, DataDir: a.DataDir, HTTPPort: 80, HTTPSPort: 443,
		AdminSocket: a.AdminSocket, Storage: a.Storage,
	})
	if err != nil {
		return err
	}
	return Load(ctx, a.AdminSocket, a.DataDir, cfg)
}

// tags label each request's access log line with its app and the instance
// that answered.
func tags(app string) []any {
	return []any{
		map[string]any{"handler": "log_append", "key": LogApp, "value": app},
		map[string]any{"handler": "log_append", "key": LogUpstream, "value": "{http.reverse_proxy.upstream.hostport}"},
	}
}

// Access log fields Jokku adds.
const (
	LogApp      = "jokku_app"
	LogUpstream = "jokku_upstream"
)

func handler(r Route) map[string]any {
	if len(r.Upstreams) == 0 {
		return map[string]any{
			"handler": "static_response", "status_code": 502,
			"body": "No running instances of " + r.App + ". Check: jokku ps:report " + r.App + "\n",
		}
	}
	upstreams := make([]any, len(r.Upstreams))
	for i, u := range r.Upstreams {
		upstreams[i] = map[string]any{"dial": u}
	}
	// An instance on a node that just died stops answering before the
	// control node notices (up to 30s). Failing the dial fast and retrying
	// on another instance turns that into latency instead of errors, and
	// passive health checks then skip it until routes catch up.
	return map[string]any{
		"handler":   "reverse_proxy",
		"upstreams": upstreams,
		"load_balancing": map[string]any{
			"selection_policy": map[string]any{"policy": "round_robin"},
			"try_duration":     "5s",
			"try_interval":     "250ms",
		},
		"health_checks": map[string]any{"passive": map[string]any{"fail_duration": "30s", "max_fails": 1}},
		"transport":     map[string]any{"protocol": "http", "dial_timeout": "2s"},
	}
}

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
	if err := caddy.Load(cfg, true); err != nil {
		return fmt.Errorf("starting caddy: %w", err)
	}
	<-ctx.Done()
	return caddy.Stop()
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
