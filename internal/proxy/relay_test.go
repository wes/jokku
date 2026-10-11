package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// seen is what a backend got with each request.
type seen struct {
	mu      sync.Mutex
	headers map[string]http.Header // by path
}

func (s *seen) get(path string) http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headers[path]
}

func cssBackend(t *testing.T, css string) (*httptest.Server, *seen) {
	t.Helper()
	s := &seen{headers: map[string]http.Header{}}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.headers[r.URL.Path] = r.Header.Clone()
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/css")
		io.WriteString(w, css)
	}))
	t.Cleanup(backend.Close)
	return backend, s
}

// client sends requests to the proxy on port from addr: 127.0.0.1 or ::1.
func client(addr string, port int) *http.Client {
	return &http.Client{Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DisableCompression: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
		}}}
}

func send(t *testing.T, c *http.Client, host, path string, header map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+host+path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		if resp, err = c.Do(req); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(zr)
	return string(out)
}

// TestANodeRelaysForItsEdges plays the edge from 127.0.0.1, which the node
// counts as an edge, and a visitor from ::1, which it doesn't.
func TestANodeRelaysForItsEdges(t *testing.T) {
	if l, err := net.Listen("tcp", "[::1]:0"); err != nil {
		t.Skip("no IPv6 loopback:", err)
	} else {
		l.Close()
	}
	css := strings.Repeat(".card { color: #c95a00; margin: 0 auto; }\n", 500)
	backend, got := cssBackend(t, css)
	upstream := strings.TrimPrefix(backend.URL, "http://")

	// The relayed requests leave no access log line: the edge writes it.
	logged := watchAccessLog(t)

	port, _ := startProxy(t, Settings{Edges: []string{"127.0.0.1/32"}, Routes: []Route{
		{App: "web", Hosts: []string{"web.test"}, Upstreams: []string{upstream}, TLS: true},
		{App: "raw", Hosts: []string{"raw.test"}, Upstreams: []string{upstream}, TLS: true, NoCompress: true},
	}})
	edge := client("127.0.0.1", port)
	fromEdge := map[string]string{
		"Accept-Encoding": "gzip", "X-Forwarded-For": "203.0.113.9", "X-Forwarded-Proto": "https",
		"X-Forwarded-Host": "web.test", "X-Jokku-User": "wes",
	}

	// An HTTPS host's request from the edge is answered, not redirected,
	// and compressed here.
	resp, body := send(t, edge, "web.test", "/app.css", fromEdge)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("relayed request: %s, Content-Encoding %q", resp.Status, resp.Header.Get("Content-Encoding"))
	}
	if gunzip(t, body) != css {
		t.Error("the relayed response doesn't decompress to what the app sent")
	}
	if got := resp.Header.Get(RelayHeader); got != upstream {
		t.Errorf("%s = %q, want the instance that answered, %s", RelayHeader, got, upstream)
	}
	// The app sees the visitor as the edge did, and the edge's login.
	h := got.get("/app.css")
	for k, want := range map[string]string{"X-Forwarded-For": "203.0.113.9", "X-Forwarded-Proto": "https", "X-Forwarded-Host": "web.test", "X-Jokku-User": "wes"} {
		if h.Get(k) != want {
			t.Errorf("the app got %s %q, want %q", k, h.Get(k), want)
		}
	}

	// An app that opted out isn't compressed, here or anywhere.
	if resp, body := send(t, edge, "raw.test", "/raw.css", fromEdge); resp.Header.Get("Content-Encoding") != "" || string(body) != css {
		t.Errorf("an app that opted out was compressed: %q", resp.Header.Get("Content-Encoding"))
	}

	// A host this node doesn't route yet is an error for the edge, not a
	// redirect it would hand on.
	if resp, _ := send(t, edge, "new.test", "/new", fromEdge); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("a host the node doesn't route: %s, want 502", resp.Status)
	}

	// Anyone else gets the public routes: the redirect to HTTPS, with no
	// relay, and nothing they claim about themselves believed.
	visitor := client("::1", port)
	resp, _ = send(t, visitor, "web.test", "/visitor", fromEdge)
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get(RelayHeader) != "" {
		t.Errorf("a visitor's request: %s, %s %q", resp.Status, RelayHeader, resp.Header.Get(RelayHeader))
	}

	// One log line: the visitor's.
	paths := map[string]int{}
	for _, req := range logged() {
		paths[req.Path]++
	}
	if paths["/visitor"] != 1 || paths["/app.css"] != 0 || paths["/raw.css"] != 0 || paths["/new"] != 0 {
		t.Errorf("logged %v: want the visitor's request only", paths)
	}
}

// TestAnEdgeSendsThroughANode plays the node with a backend that answers as
// a node's relay route does.
func TestAnEdgeSendsThroughANode(t *testing.T) {
	css := strings.Repeat(".card { color: #c95a00; margin: 0 auto; }\n", 500)
	var pre bytes.Buffer
	zw := gzip.NewWriter(&pre)
	io.WriteString(zw, css)
	zw.Close()
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set(RelayHeader, "10.210.2.5:5000")
		w.Write(pre.Bytes())
	}))
	defer node.Close()

	logged := watchAccessLog(t)
	port, _ := startProxy(t, Settings{Routes: []Route{
		{App: "web", Hosts: []string{"web.test"}, Upstreams: []string{strings.TrimPrefix(node.URL, "http://")}, Relayed: true},
	}})
	resp, body := send(t, client("127.0.0.1", port), "web.test", "/app.css", map[string]string{"Accept-Encoding": "gzip"})
	if resp.Header.Get(RelayHeader) != "" {
		t.Errorf("the visitor got %s %q", RelayHeader, resp.Header.Get(RelayHeader))
	}
	if resp.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(body, pre.Bytes()) {
		t.Error("what the node compressed was changed on the way")
	}

	// The edge logs the instance the node named.
	for _, req := range logged() {
		if req.Path == "/app.css" {
			if req.App != "web" || req.Upstream != "10.210.2.5:5000" {
				t.Errorf("logged as %+v, want app web from 10.210.2.5:5000", req)
			}
			return
		}
	}
	t.Error("the request wasn't logged")
}
