package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wes/jokku/internal/types"
)

func TestPingLoopback(t *testing.T) {
	st, err := Ping(context.Background(), "127.0.0.1", 20)
	if err != nil && strings.Contains(err.Error(), "opening an ICMP socket") {
		t.Skip("this system allows no ICMP socket here:", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if st.Sent != 20 || st.Received != 20 || st.LossPct() != 0 {
		t.Errorf("sent %d, received %d", st.Sent, st.Received)
	}
	if st.Min <= 0 || st.Min > st.Avg || st.Avg > st.Max || st.Max > 100 || st.Jitter < 0 {
		t.Errorf("round trips: %+v", st)
	}
}

// TestProbe checks a probe pings the target's host and sends it throughput
// for as long as asked, which the target's sink counts.
func TestProbe(t *testing.T) {
	a := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Ping: func(ctx context.Context, host string, count int) (types.PingStats, error) {
			if host != "127.0.0.1" {
				t.Errorf("pinged %s, not the target's host", host)
			}
			return types.PingStats{Sent: count, Received: count, Min: 0.2, Avg: 0.3, Max: 0.5}, nil
		}})
	srv := httptest.NewServer(a.Handler("tok"))
	defer srv.Close()
	target := strings.TrimPrefix(srv.URL, "http://")

	probe := func(token string, p types.NetProbe) (*http.Response, types.NetProbeResult) {
		b, _ := json.Marshal(p)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/nettest/probe", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var res types.NetProbeResult
		json.NewDecoder(resp.Body).Decode(&res)
		return resp, res
	}
	if resp, _ := probe("wrong", types.NetProbe{Target: target, Seconds: 1}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a probe without the control node's token: %s", resp.Status)
	}
	start := time.Now()
	resp, res := probe("tok", types.NetProbe{Target: target, Seconds: 0.5, Pings: 10})
	if resp.StatusCode != http.StatusOK || res.Error != "" || res.PingError != "" {
		t.Fatalf("%s: %+v", resp.Status, res)
	}
	if took := time.Since(start); took < 500*time.Millisecond || took > 5*time.Second {
		t.Errorf("a half-second probe took %s", took)
	}
	if res.Mbps < 10 || res.Ping.Received != 10 {
		t.Errorf("result: %+v", res)
	}
}
