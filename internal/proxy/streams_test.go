package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/wes/jokku/internal/types"
)

// echoBackend answers each line with its name and the line, like a
// database that says which one it is.
func echoBackend(t *testing.T, name string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					io.WriteString(c, name+":"+line)
				}
			}()
		}
	}()
	return l.Addr().String()
}

// ask sends a line on c and returns the answer.
func ask(c net.Conn) (string, error) {
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c, "hello\n"); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line), err
}

// sslRequest is what a classic Postgres client sends first, asking for TLS.
func sslRequest() []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, 8)
	binary.BigEndian.PutUint32(b[4:], 80877103)
	return b
}

// TestProxyStreams runs the embedded Caddy with streams and connects the
// ways database clients do.
func TestProxyStreams(t *testing.T) {
	shop, other, cache, orders := echoBackend(t, "shop"), echoBackend(t, "other"), echoBackend(t, "cache"), echoBackend(t, "orders")
	dir, err := os.MkdirTemp("", "jkp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	pg, redis, mysql, closed := freePort(t), freePort(t), freePort(t), freePort(t)
	cfg, err := Config(Settings{
		DataDir: dir, HTTPPort: freePort(t), HTTPSPort: freePort(t), AdminSocket: filepath.Join(dir, "admin.sock"),
		Streams: []types.ProxyStream{
			{App: "postgres-shop", Protocol: types.StreamPostgres, Host: "shop.db.test", Port: pg, Upstreams: []string{shop}},
			{App: "postgres-other", Protocol: types.StreamPostgres, Host: "other.db.test", Port: pg, Upstreams: []string{other}},
			{App: "postgres-office", Protocol: types.StreamPostgres, Host: "office.db.test", Port: pg, Upstreams: []string{other},
				Allow: []string{"10.0.0.0/8"}},
			{App: "postgres-stopped", Protocol: types.StreamPostgres, Host: "stopped.db.test", Port: pg},
			{App: "redis-cache", Protocol: types.StreamTLS, Host: "cache.db.test", Port: redis, Upstreams: []string{cache}},
			{App: "mysql-orders", Protocol: types.StreamPlain, Host: "orders.db.test", Port: mysql, Upstreams: []string{orders},
				Allow: []string{"127.0.0.0/8"}},
			{App: "mysql-closed", Protocol: types.StreamPlain, Host: "closed.db.test", Port: closed, Upstreams: []string{orders},
				Allow: []string{"192.0.2.0/24"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := caddy.Load(cfg, true); err != nil {
		t.Fatalf("%v\n%s", err, cfg)
	}
	defer caddy.Stop()
	addr := func(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

	// classic connects the way libpq does by default: an SSLRequest, then
	// TLS once the server says S.
	classic := func(host string) (string, error) {
		c, err := net.Dial("tcp", addr(pg))
		if err != nil {
			return "", err
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write(sslRequest())
		yes := make([]byte, 1)
		if _, err := io.ReadFull(c, yes); err != nil || yes[0] != 'S' {
			return "", errors.New("no S for the SSLRequest")
		}
		tc := tls.Client(c, &tls.Config{ServerName: host, InsecureSkipVerify: true, NextProtos: []string{"postgresql"}})
		if err := tc.Handshake(); err != nil {
			return "", err
		}
		return ask(tc)
	}
	direct := func(port int, host string, alpn ...string) (string, error) {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr(port),
			&tls.Config{ServerName: host, InsecureSkipVerify: true, NextProtos: alpn})
		if err != nil {
			return "", err
		}
		defer c.Close()
		if len(alpn) > 0 && c.ConnectionState().NegotiatedProtocol != alpn[0] {
			return "", errors.New("ALPN " + c.ConnectionState().NegotiatedProtocol)
		}
		return ask(c)
	}
	plain := func(port int) (string, error) {
		c, err := net.DialTimeout("tcp", addr(port), 3*time.Second)
		if err != nil {
			return "", err
		}
		defer c.Close()
		return ask(c)
	}

	// The certificates come from the proxy's own CA, in the background, a
	// name at a time, so a name that should answer gets a while to.
	answer := func(dial func() (string, error)) (got string, err error) {
		for i := 0; i < 100; i++ {
			if got, err = dial(); err == nil {
				return got, nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return got, err
	}
	if got, err := answer(func() (string, error) { return classic("shop.db.test") }); got != "shop:hello" {
		t.Fatalf("classic Postgres TLS to shop: %q %v", got, err)
	}
	for _, c := range []struct {
		what string
		dial func() (string, error)
		want string // "" for a connection that is refused or closed
	}{
		{"classic Postgres TLS to other", func() (string, error) { return classic("other.db.test") }, "other:hello"},
		{"Postgres 17 direct TLS", func() (string, error) { return direct(pg, "other.db.test", "postgresql") }, "other:hello"},
		{"an unknown name", func() (string, error) { return classic("nope.db.test") }, ""},
		{"an address not let in", func() (string, error) { return classic("office.db.test") }, ""},
		{"a database that isn't running", func() (string, error) { return classic("stopped.db.test") }, ""},
		{"Postgres without TLS", func() (string, error) { return plain(pg) }, ""},
		{"rediss://", func() (string, error) { return direct(redis, "cache.db.test") }, "cache:hello"},
		{"a plain port", func() (string, error) { return plain(mysql) }, "orders:hello"},
		{"a plain port, from an address not let in", func() (string, error) { return plain(closed) }, ""},
	} {
		dial := c.dial
		if c.want != "" {
			dial = func() (string, error) { return answer(c.dial) }
		}
		got, err := dial()
		if c.want != "" && (err != nil || got != c.want) {
			t.Errorf("%s: %q %v, want %q", c.what, got, err, c.want)
		}
		if c.want == "" && err == nil {
			t.Errorf("%s: answered %q", c.what, got)
		}
	}
}

// TestStreamsDontHoldUpWebRoutes: a TCP port the proxy can't listen on
// leaves it serving the web routes, and the error says so.
func TestStreamsDontHoldUpWebRoutes(t *testing.T) {
	dir, err := os.MkdirTemp("", "jkp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	a := &Applier{DataDir: dir, AdminSocket: filepath.Join(dir, "admin.sock"), HTTPPort: freePort(t), HTTPSPort: freePort(t)}
	empty, err := Config(Settings{DataDir: dir, HTTPPort: a.HTTPPort, HTTPSPort: a.HTTPSPort, AdminSocket: a.AdminSocket})
	if err != nil {
		t.Fatal(err)
	}
	if err := caddy.Load(empty, true); err != nil {
		t.Fatal(err)
	}
	defer caddy.Stop()
	taken, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	ps := &types.ProxyState{
		Routes: []types.ProxyRoute{{App: "web", Hosts: []string{"web.test"}}},
		Streams: []types.ProxyStream{{App: "mysql-orders", Protocol: types.StreamPlain, Host: "orders.test",
			Port: taken.Addr().(*net.TCPAddr).Port, Upstreams: []string{"127.0.0.1:1"}}},
	}
	err = a.Apply(context.Background(), ps)
	if err == nil || !strings.Contains(err.Error(), "without the TCP ones") {
		t.Fatalf("apply: %v", err)
	}
	saved, _ := os.ReadFile(ConfigPath(dir))
	if !strings.Contains(string(saved), "web.test") || strings.Contains(string(saved), "layer4") {
		t.Errorf("the proxy runs:\n%s", saved)
	}
	// Again within a minute: the web routes load and the TCP ones aren't
	// tried.
	ps.Routes[0].Hosts = []string{"www.test"}
	if err := a.Apply(context.Background(), ps); err == nil {
		t.Error("the second apply says nothing of the TCP routes")
	}
	if saved, _ := os.ReadFile(ConfigPath(dir)); !strings.Contains(string(saved), "www.test") {
		t.Errorf("the web routes didn't change:\n%s", saved)
	}
}

// TestStartWithoutStreams: a saved config whose stream port was taken
// since still starts, serving the web.
func TestStartWithoutStreams(t *testing.T) {
	dir, err := os.MkdirTemp("", "jkp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	taken, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	httpPort := freePort(t)
	cfg, err := Config(Settings{
		DataDir: dir, HTTPPort: httpPort, HTTPSPort: freePort(t), AdminSocket: filepath.Join(dir, "admin.sock"),
		Routes: []Route{{App: "down", Hosts: []string{"down.test"}}},
		Streams: []types.ProxyStream{{App: "mysql-orders", Protocol: types.StreamPlain, Host: "orders.test",
			Port: taken.Addr().(*net.TCPAddr).Port, Upstreams: []string{"127.0.0.1:1"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := start(cfg); err != nil {
		t.Fatal(err)
	}
	defer caddy.Stop()
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(httpPort)+"/", nil)
	req.Host = "down.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("the web route answered %d, want 502", resp.StatusCode)
	}
	if _, ok := webOnly([]byte(`{"apps":{"http":{}}}`)); ok {
		t.Error("a config without streams has some to drop")
	}
}
