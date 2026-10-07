package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestProxyRoutes runs the embedded Caddy with a generated config and sends
// real requests through it.
func TestProxyRoutes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from "+r.Host+" via "+r.Header.Get("X-Forwarded-Proto"))
	}))
	defer backend.Close()

	dir, err := os.MkdirTemp("", "jkp") // unix socket paths must stay short
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	httpPort := freePort(t)
	settings := Settings{
		DataDir:     dir,
		HTTPPort:    httpPort,
		HTTPSPort:   freePort(t),
		AdminSocket: filepath.Join(dir, "admin.sock"),
		Routes: []Route{
			{App: "web", Hosts: []string{"web.192.168.1.10.sslip.io"}, Upstreams: []string{strings.TrimPrefix(backend.URL, "http://")}},
			{App: "down", Hosts: []string{"down.test"}},
		},
	}
	cfg, err := Config(Settings{DataDir: dir, HTTPPort: httpPort, HTTPSPort: settings.HTTPSPort, AdminSocket: settings.AdminSocket})
	if err != nil {
		t.Fatal(err)
	}
	// Start empty, then load routes through the admin socket the way the
	// daemon does.
	if err := caddy.Load(cfg, true); err != nil {
		t.Fatal(err)
	}
	defer caddy.Stop()
	if cfg, err = Config(settings); err != nil {
		t.Fatal(err)
	}
	if err := Load(context.Background(), settings.AdminSocket, dir, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ConfigPath(dir)); err != nil {
		t.Fatal("config not saved for restarts:", err)
	}

	get := func(host string) (int, string) {
		req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(httpPort)+"/", nil)
		req.Host = host
		var resp *http.Response
		for i := 0; i < 50; i++ {
			if resp, err = http.DefaultClient.Do(req); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("web.192.168.1.10.sslip.io"); code != 200 || body != "hello from web.192.168.1.10.sslip.io via http" {
		t.Errorf("app route: %d %q", code, body)
	}
	if code, _ := get("down.test"); code != 502 {
		t.Errorf("app without instances: %d, want 502", code)
	}
	if code, _ := get("unknown.test"); code != 404 {
		t.Errorf("unknown host: %d, want 404", code)
	}
}

func TestConfigTLSRedirects(t *testing.T) {
	cfg, err := Config(Settings{DataDir: "/d", HTTPPort: 80, HTTPSPort: 443, Email: "ops@example.com", Routes: []Route{
		{App: "shop", Hosts: []string{"shop.example.com"}, Upstreams: []string{"10.210.1.2:5000"}, TLS: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []json.RawMessage `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
			TLS json.RawMessage `json:"tls"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	httpRoutes := c.Apps.HTTP.Servers["http"].Routes
	if len(httpRoutes) != 2 || !strings.Contains(string(httpRoutes[0]), "308") {
		t.Errorf("http server should redirect TLS hosts, then 404: %s", httpRoutes)
	}
	if routes := c.Apps.HTTP.Servers["https"].Routes; len(routes) != 2 || !strings.Contains(string(routes[0]), "reverse_proxy") {
		t.Errorf("https server routes: %s", routes)
	}
	if !strings.Contains(string(c.Apps.TLS), "ops@example.com") {
		t.Error("ACME email missing")
	}
}

func TestPublicHost(t *testing.T) {
	for host, want := range map[string]bool{
		"shop.example.com":            true,
		"*.example.com":               true,
		"app.203.0.113.10.sslip.io":   true,
		"app.192.168.86.113.sslip.io": false,
		"app.10.0.0.5.nip.io":         false,
		"app.100.64.1.2.sslip.io":     false,
		"192.168.1.10":                false,
		"localhost":                   false,
	} {
		if got := PublicHost(host); got != want {
			t.Errorf("PublicHost(%q) = %v, want %v", host, got, want)
		}
	}
}
