package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/wes/jokku/internal/types"
)

// Network tests. The control node asks each node in turn to measure its
// link to another (serveProbe): pings, then as much TCP as it can send for
// a few seconds to the other's sink (serveSink), which counts what arrives.
// Both run on the agent API, on the mesh, which only nodes reach.

// netSinkMax is the longest a sink takes in, whatever the sender does.
const netSinkMax = 60 * time.Second

// pingEvery is how often a probe pings, as "ping -i 0.01" does.
const pingEvery = 10 * time.Millisecond

// netClient sends throughput. No timeout: the probe's context ends it.
var netClient = &http.Client{Transport: &http.Transport{DisableCompression: true, ResponseHeaderTimeout: netSinkMax + 10*time.Second}}

// serveSink reads a throughput test's body to the end and says how much
// arrived and how fast: from its first byte to its last.
func (a *Agent) serveSink(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), netSinkMax)
	defer cancel()
	go func() {
		<-ctx.Done()
		r.Body.Close()
	}()
	buf := make([]byte, 1<<20)
	var total int64
	var first time.Time
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			if first.IsZero() {
				first = time.Now()
			}
			total += int64(n)
		}
		if err != nil {
			break
		}
	}
	res := types.NetSinkResult{Bytes: total}
	if !first.IsZero() {
		res.Seconds = time.Since(first).Seconds()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// serveProbe measures this node's link to another node's agent, for the
// control node.
func (a *Agent) serveProbe(w http.ResponseWriter, r *http.Request) {
	var p types.NetProbe
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	host, _, err := net.SplitHostPort(p.Target)
	if err != nil || p.Seconds <= 0 || p.Seconds > netSinkMax.Seconds()/2 || p.Pings < 0 || p.Pings > 1000 {
		http.Error(w, "bad probe", http.StatusBadRequest)
		return
	}
	var res types.NetProbeResult
	ping := a.Ping
	if ping == nil {
		ping = Ping
	}
	if p.Pings > 0 {
		st, err := ping(r.Context(), host, p.Pings)
		res.Ping = st
		if err != nil {
			res.PingError = err.Error()
		}
	}
	mbps, err := sendThroughput(r.Context(), p.Target, time.Duration(p.Seconds*float64(time.Second)))
	res.Mbps = mbps
	if err != nil {
		res.Error = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// sendThroughput sends to target's sink for d and returns the rate it took
// it in at, in Mbit/s.
func sendThroughput(ctx context.Context, target string, d time.Duration) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, d+30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+target+"/v1/nettest/sink", &timedZeros{until: time.Now().Add(d)})
	if err != nil {
		return 0, err
	}
	resp, err := netClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s answered %s", target, resp.Status)
	}
	var res types.NetSinkResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return 0, err
	}
	if res.Seconds <= 0 {
		return 0, errors.New("nothing arrived")
	}
	return float64(res.Bytes) * 8 / res.Seconds / 1e6, nil
}

// timedZeros reads as zeros until a deadline, then as the end.
type timedZeros struct{ until time.Time }

var zeros = make([]byte, 1<<20)

func (z *timedZeros) Read(p []byte) (int, error) {
	if !time.Now().Before(z.until) {
		return 0, io.EOF
	}
	return copy(p, zeros), nil
}

// Ping sends count ICMP echoes to host, pingEvery apart, and waits up to a
// second for the last answers. It uses an unprivileged ICMP socket where
// the system allows one, and a raw one (root) where it doesn't.
func Ping(ctx context.Context, host string, count int) (types.PingStats, error) {
	st := types.PingStats{}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		addrs, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err != nil || len(addrs) == 0 {
			return st, fmt.Errorf("resolving %s: %v", host, err)
		}
		ip = addrs[0].To4()
	}
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	var dst net.Addr = &net.UDPAddr{IP: ip}
	if err != nil {
		if conn, err = icmp.ListenPacket("ip4:icmp", "0.0.0.0"); err != nil {
			return st, fmt.Errorf("opening an ICMP socket: %w", err)
		}
		dst = &net.IPAddr{IP: ip}
	}
	defer conn.Close()
	id := os.Getpid() & 0xffff
	var mu sync.Mutex
	sentAt, answered := make([]time.Time, count), make([]bool, count)
	var lastSent time.Time
	var rtts []float64

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1500)
		for {
			conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, _, err := conn.ReadFrom(buf)
			now := time.Now()
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					mu.Lock()
					last := st.Sent == count && (len(rtts) == count || now.Sub(lastSent) > time.Second)
					mu.Unlock()
					if last || ctx.Err() != nil {
						return
					}
					continue
				}
				return
			}
			msg, err := icmp.ParseMessage(1, buf[:n])
			if err != nil || msg.Type != ipv4.ICMPTypeEchoReply {
				continue
			}
			echo, ok := msg.Body.(*icmp.Echo)
			// The kernel picks the ID of an unprivileged socket's echoes,
			// and only hands it its own replies; a raw socket sees every
			// reply, so its own carry this process's ID.
			if !ok || echo.Seq < 0 || echo.Seq >= count {
				continue
			}
			if _, raw := dst.(*net.IPAddr); raw && echo.ID != id {
				continue
			}
			mu.Lock()
			if !sentAt[echo.Seq].IsZero() && !answered[echo.Seq] {
				answered[echo.Seq] = true
				rtts = append(rtts, float64(now.Sub(sentAt[echo.Seq]))/float64(time.Millisecond))
			}
			mu.Unlock()
		}
	}()

	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for seq := 0; seq < count && ctx.Err() == nil; seq++ {
		b, _ := (&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("jokku network test")}}).Marshal(nil)
		mu.Lock()
		sentAt[seq] = time.Now()
		lastSent = sentAt[seq]
		st.Sent++
		mu.Unlock()
		if _, err := conn.WriteTo(b, dst); err != nil {
			mu.Lock()
			st.Sent--
			mu.Unlock()
			conn.Close()
			<-done
			return st, fmt.Errorf("pinging %s: %w", host, err)
		}
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
	<-done
	mu.Lock()
	defer mu.Unlock()
	st.Received = len(rtts)
	if len(rtts) > 0 {
		st.Min, st.Max = math.Inf(1), 0
		var sum, sq float64
		for _, r := range rtts {
			st.Min, st.Max = min(st.Min, r), max(st.Max, r)
			sum += r
			sq += r * r
		}
		n := float64(len(rtts))
		st.Avg = sum / n
		st.Jitter = math.Sqrt(max(sq/n-st.Avg*st.Avg, 0))
	}
	return st, ctx.Err()
}
