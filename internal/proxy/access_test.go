package proxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/wes/jokku/internal/journal"
)

// TestAccessLog checks the proxy writes one tagged JSON line per request,
// which "jokku logs" and "jokku top" read back.
func TestAccessLog(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer backend.Close()
	upstream := strings.TrimPrefix(backend.URL, "http://")

	// The access log goes to stderr (the journal, in production).
	r, w, _ := os.Pipe()
	stderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = stderr }()

	dir, _ := os.MkdirTemp("", "jkp")
	defer os.RemoveAll(dir)
	port := freePort(t)
	cfg, err := Config(Settings{DataDir: dir, HTTPPort: port, HTTPSPort: freePort(t), AdminSocket: filepath.Join(dir, "a.sock"),
		Routes: []Route{{App: "shop", Hosts: []string{"shop.test"}, Upstreams: []string{upstream}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := caddy.Load(cfg, true); err != nil {
		t.Fatal(err)
	}
	defer caddy.Stop()

	get := func(host, path string) {
		req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(port)+path, nil)
		req.Host = host
		for i := 0; i < 50; i++ {
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("proxy did not answer")
	}
	get("shop.test", "/pricing?x=1")
	get("nobody.test", "/")

	found := map[string]bool{}
	sc := bufio.NewScanner(r)
	deadline := time.Now().Add(5 * time.Second)
	go func() { time.Sleep(5 * time.Second); w.Close() }()
	for time.Now().Before(deadline) && sc.Scan() {
		req, ok := journal.ParseAccess(sc.Text())
		if !ok {
			continue
		}
		switch {
		case req.App == "shop":
			if req.Path != "/pricing?x=1" || req.Status != 200 || req.Upstream != upstream || req.Method != "GET" || req.Bytes != 5 || req.Client == "" {
				t.Errorf("shop request logged as %+v", req)
			}
			if !strings.Contains(sc.Text(), `"jokku_upstream"`) || strings.Contains(sc.Text(), `"headers"`) {
				t.Errorf("unexpected log line shape: %s", sc.Text())
			}
			found["shop"] = true
		case req.Host == "nobody.test":
			if req.Status != 404 || req.App != "" {
				t.Errorf("unmatched request logged as %+v", req)
			}
			found["unmatched"] = true
		}
		if found["shop"] && found["unmatched"] {
			return
		}
	}
	t.Fatalf("access log lines missing: %v", found)
}
