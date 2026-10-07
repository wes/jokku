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
			"handle":   []any{handler(r)},
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
		},
	}
	if len(tlsHosts) > 0 {
		servers["https"] = map[string]any{
			"listen": []string{fmt.Sprintf(":%d", s.HTTPSPort)},
			"routes": httpsRoutes,
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
		"storage": map[string]any{"module": "file_system", "root": filepath.Join(s.DataDir, "proxy", "data")},
		"logging": map[string]any{"logs": map[string]any{"default": map[string]any{"level": "WARN"}}},
		"apps":    apps,
	}
	return json.MarshalIndent(cfg, "", "  ")
}

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
	return map[string]any{
		"handler":        "reverse_proxy",
		"upstreams":      upstreams,
		"load_balancing": map[string]any{"selection_policy": map[string]any{"policy": "round_robin"}},
		"health_checks":  map[string]any{"passive": map[string]any{"fail_duration": "10s"}},
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
