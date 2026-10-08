package dns

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// upstream is a resolver for the outside world that knows one name.
func upstream(t *testing.T) string {
	t.Helper()
	h := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if r.Question[0].Name == "example.com." {
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(93, 184, 215, 14)}}
		} else {
			m.Rcode = dns.RcodeNameError
		}
		w.WriteMsg(m)
	})
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	udp, tcp := &dns.Server{PacketConn: pc, Handler: h}, &dns.Server{Listener: ln, Handler: h}
	go udp.ActivateAndServe()
	go tcp.ActivateAndServe()
	t.Cleanup(func() { udp.Shutdown(); tcp.Shutdown() })
	return pc.LocalAddr().String()
}

func query(t *testing.T, addr, network, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	c := &dns.Client{Net: network, Timeout: 2 * time.Second}
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("%s %s over %s: %v", name, dns.TypeToString[qtype], network, err)
	}
	return resp
}

func addrs(m *dns.Msg) []string {
	var out []string
	for _, rr := range m.Answer {
		if a, ok := rr.(*dns.A); ok {
			out = append(out, a.A.String())
		}
	}
	sort.Strings(out)
	return out
}

// start serves srv on a free port, UDP and TCP alike.
func start(t *testing.T, srv *Server) string {
	t.Helper()
	for range 20 {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := pc.LocalAddr().String()
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			pc.Close() // that port is taken for TCP; try another
			continue
		}
		l := serveOn(addr, pc, ln, srv)
		t.Cleanup(l.close)
		return addr
	}
	t.Fatal("no free port")
	return ""
}

func TestServerAnswersInternalNamesAndForwardsTheRest(t *testing.T) {
	srv := NewServer([]string{upstream(t)})
	srv.SetRecords(map[string][]string{
		"web.shop.internal": {"10.210.1.5", "10.210.2.7"},
		"shop.internal":     {"10.210.1.5", "10.210.2.7"},
		"db.shop.internal":  {"10.210.2.9"},
		"bad.example":       {"10.0.0.1"}, // outside the zone: ignored
		"v6.shop.internal":  {"fd00::1"},  // not IPv4: ignored
		"x.shop.internal":   {"not an address"},
	})
	addr := start(t, srv)

	for _, network := range []string{"udp", "tcp"} {
		if got := addrs(query(t, addr, network, "web.shop.internal", dns.TypeA)); !slices.Equal(got, []string{"10.210.1.5", "10.210.2.7"}) {
			t.Errorf("%s: web.shop.internal = %v", network, got)
		}
		if got := addrs(query(t, addr, network, "DB.Shop.Internal", dns.TypeA)); !slices.Equal(got, []string{"10.210.2.9"}) {
			t.Errorf("%s: names are case-insensitive, got %v", network, got)
		}
		if got := addrs(query(t, addr, network, "example.com", dns.TypeA)); !slices.Equal(got, []string{"93.184.215.14"}) {
			t.Errorf("%s: forwarded example.com = %v", network, got)
		}
	}
	r := query(t, addr, "udp", "web.shop.internal", dns.TypeA)
	if !r.Authoritative || r.Answer[0].Header().Ttl != TTL {
		t.Errorf("internal answers should be authoritative with a %ds TTL: %v", TTL, r)
	}
	if r := query(t, addr, "udp", "nope.shop.internal", dns.TypeA); r.Rcode != dns.RcodeNameError || len(r.Ns) != 1 {
		t.Errorf("unknown name: rcode %s, ns %v", dns.RcodeToString[r.Rcode], r.Ns)
	}
	// Names that exist but have no such records answer empty, not NXDOMAIN,
	// so resolvers don't give up on the A lookup.
	for _, q := range []struct {
		name  string
		qtype uint16
	}{{"web.shop.internal", dns.TypeAAAA}, {"v6.shop.internal", dns.TypeA}, {"internal", dns.TypeA}} {
		if r := query(t, addr, "udp", q.name, q.qtype); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
			t.Errorf("%s %s: rcode %s, %d answers", q.name, dns.TypeToString[q.qtype], dns.RcodeToString[r.Rcode], len(r.Answer))
		}
	}
	if r := query(t, addr, "udp", "bad.example", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Error("a record outside the zone was served")
	}
	if r := query(t, addr, "udp", "internal", dns.TypeSOA); len(r.Answer) != 1 {
		t.Error("no SOA for the zone")
	}
}

func TestServerWithoutUpstreams(t *testing.T) {
	dead, _ := net.ListenPacket("udp", "127.0.0.1:0")
	deadAddr := dead.LocalAddr().String()
	dead.Close()
	srv := NewServer([]string{deadAddr})
	srv.timeout = 200 * time.Millisecond
	addr := start(t, srv)
	if r := query(t, addr, "udp", "example.com", dns.TypeA); r.Rcode != dns.RcodeServerFailure {
		t.Errorf("unreachable upstream: rcode %s", dns.RcodeToString[r.Rcode])
	}
}

func TestRecords(t *testing.T) {
	got := Records([]Instance{
		{App: "shop", Process: "web", IP: "10.210.1.2", Healthy: true, Web: true},
		{App: "shop", Process: "web", IP: "10.210.1.3", Web: true}, // a deploy's new instance, not passing checks yet
		{App: "shop", Process: "worker", IP: "10.210.1.4"},
		{App: "mydb", Process: "postgres", IP: "10.210.2.2"},
		{App: "jobs", Process: "worker", IP: "10.210.2.3"},
		{App: "jobs", Process: "Cron_Job", IP: "10.210.2.4"},
		{App: "blog", Process: "app", IP: "10.210.3.2", Healthy: true, Web: true}, // compose: the routed service isn't "web"
		{App: "blog", Process: "db", IP: "10.210.3.3", Healthy: true},
	})
	want := map[string][]string{
		"web.shop.internal":      {"10.210.1.2"}, // healthy ones win while there are any
		"shop.internal":          {"10.210.1.2"}, // the routed process
		"worker.shop.internal":   {"10.210.1.4"}, // none healthy yet: the wanted ones
		"postgres.mydb.internal": {"10.210.2.2"},
		"mydb.internal":          {"10.210.2.2"}, // its only process
		"worker.jobs.internal":   {"10.210.2.3"},
		"cron_job.jobs.internal": {"10.210.2.4"},
		// no jobs.internal: two processes, none routed
		"app.blog.internal": {"10.210.3.2"},
		"blog.internal":     {"10.210.3.2"},
		"db.blog.internal":  {"10.210.3.3"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for name, ips := range want {
		if !slices.Equal(got[name], ips) {
			t.Errorf("%s = %v, want %v", name, got[name], ips)
		}
	}
}

func TestRunServesTheZoneFileAndFollowsIt(t *testing.T) {
	dir := t.TempDir()
	free, _ := net.ListenPacket("udp", "127.0.0.1:0")
	addr := free.LocalAddr().String()
	free.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- Run(ctx, dir, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	t.Cleanup(func() { cancel(); <-done })

	a := &Applier{DataDir: dir}
	if err := a.Apply(ctx, Zone{Listen: addr, Records: map[string][]string{"web.shop.internal": {"10.210.1.2"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadZone(filepath.Join(dir, "dns", "zone.json")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the service answers from the zone", func() bool {
		r, err := exchange(addr, "web.shop.internal")
		return err == nil && slices.Equal(addrs(r), []string{"10.210.1.2"})
	})
	if !a.Ready(addr) {
		t.Error("Applier.Ready: the running service was not seen")
	}
	a.Apply(ctx, Zone{Listen: addr, Records: map[string][]string{"web.shop.internal": {"10.210.1.9"}}})
	waitFor(t, "the service picks up a new zone", func() bool {
		r, err := exchange(addr, "web.shop.internal")
		return err == nil && slices.Equal(addrs(r), []string{"10.210.1.9"})
	})
}

func exchange(addr, name string) (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	r, _, err := (&dns.Client{Timeout: 300 * time.Millisecond}).Exchange(m, addr)
	return r, err
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}
