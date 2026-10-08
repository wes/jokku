// Package dns gives microVMs names for each other. <process>.<app>.internal
// resolves to the addresses of an app's instances of that process type, and
// <app>.internal to its web instances, wherever in the cluster they run.
// VMs get "search <app>.internal internal", so "db" finds a process of their
// own app and "mydb" another app.
//
// Every node runs the server as its own service ("jokku dns", started by
// systemd as jokku-dns) on its VM bridge address, so restarting or updating
// the daemon never interrupts lookups. The agent writes the cluster's names
// to a file the service watches. Every other name is forwarded to the host's
// resolvers.
package dns

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const (
	// Domain is the zone the server answers for.
	Domain = "internal."
	// TTL is short: instances come and go with deploys and moves.
	TTL = 5
	// Version changes when a release needs the DNS service restarted;
	// "jokku setup" compares it to the running one.
	Version = "1"
	// Port is where VMs reach the server, on their node's bridge address.
	Port = 53
)

// Zone is what the agent hands the service.
type Zone struct {
	Listen  string              `json:"listen"`  // the node's address on its VM bridge (port 53), or address:port
	Records map[string][]string `json:"records"` // name, without the trailing dot -> IPv4 addresses
}

// ZonePath is the file the agent writes and the service watches.
func ZonePath(dataDir string) string { return filepath.Join(dataDir, "dns", "zone.json") }

// Server answers for Domain and forwards everything else.
type Server struct {
	mu        sync.RWMutex
	records   map[string][]netip.Addr // lowercase FQDN -> addresses
	names     map[string]bool         // every name that exists, including parents like app.internal.
	upstreams []string                // host:port
	timeout   time.Duration
}

func NewServer(upstreams []string) *Server {
	s := &Server{timeout: 2 * time.Second}
	s.SetRecords(nil)
	s.SetUpstreams(upstreams)
	return s
}

// SetRecords replaces the names the server answers for. Names outside
// Domain and addresses that aren't IPv4 are ignored.
func (s *Server) SetRecords(m map[string][]string) {
	records := map[string][]netip.Addr{}
	names := map[string]bool{Domain: true}
	for name, ips := range m {
		fqdn := dns.Fqdn(strings.ToLower(name))
		if !strings.HasSuffix(fqdn, "."+Domain) {
			continue
		}
		for _, ip := range ips {
			if a, err := netip.ParseAddr(ip); err == nil && a.Is4() {
				records[fqdn] = append(records[fqdn], a)
			}
		}
		for n := fqdn; n != Domain; n = n[strings.Index(n, ".")+1:] {
			names[n] = true
		}
	}
	s.mu.Lock()
	s.records, s.names = records, names
	s.mu.Unlock()
}

func (s *Server) SetUpstreams(upstreams []string) {
	s.mu.Lock()
	s.upstreams = upstreams
	s.mu.Unlock()
}

func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) != 1 || r.Opcode != dns.OpcodeQuery {
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeFormatError)
		w.WriteMsg(m)
		return
	}
	name := strings.ToLower(r.Question[0].Name)
	if name == Domain || strings.HasSuffix(name, "."+Domain) {
		w.WriteMsg(s.answer(r, name))
		return
	}
	w.WriteMsg(s.forward(r, w.LocalAddr().Network()))
}

// answer replies authoritatively for a name in Domain.
func (s *Server) answer(r *dns.Msg, name string) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative, m.RecursionAvailable = true, true
	q := r.Question[0]
	s.mu.RLock()
	addrs, exists := s.records[name], s.names[name]
	s.mu.RUnlock()
	switch {
	case name == Domain && q.Qtype == dns.TypeSOA:
		m.Answer = []dns.RR{soa()}
	case !exists:
		m.Rcode = dns.RcodeNameError
		m.Ns = []dns.RR{soa()}
	case len(addrs) > 0 && (q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY):
		// A different order each time spreads clients that use the first.
		order := rand.Perm(len(addrs))
		for _, i := range order {
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: TTL},
				A:   addrs[i].AsSlice(),
			})
		}
	default:
		m.Ns = []dns.RR{soa()} // the name exists, but has no records of this type
	}
	return m
}

func soa() dns.RR {
	return &dns.SOA{
		Hdr: dns.RR_Header{Name: Domain, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: TTL},
		Ns:  "ns." + Domain, Mbox: "hostmaster." + Domain,
		Serial: 1, Refresh: TTL, Retry: TTL, Expire: TTL, Minttl: TTL,
	}
}

// forward asks the host's resolvers, in order, over the protocol the VM used.
func (s *Server) forward(r *dns.Msg, network string) *dns.Msg {
	s.mu.RLock()
	upstreams, timeout := s.upstreams, s.timeout
	s.mu.RUnlock()
	c := &dns.Client{Net: "udp", Timeout: timeout}
	if network == "tcp" {
		c.Net = "tcp"
	}
	for _, up := range upstreams {
		resp, _, err := c.Exchange(r, up)
		if err == nil && resp != nil {
			return resp
		}
	}
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeServerFailure)
	return m
}

// HostResolvers are the resolvers this host uses (systemd-resolved's stub
// included: the service runs on the host), as host:port.
func HostResolvers() []string {
	var out []string
	if f, err := os.Open("/etc/resolv.conf"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) == 2 && fields[0] == "nameserver" {
				if a, err := netip.ParseAddr(fields[1]); err == nil {
					out = append(out, netip.AddrPortFrom(a.Unmap(), 53).String())
				}
			}
		}
		f.Close()
	}
	if len(out) == 0 {
		out = []string{"1.1.1.1:53", "8.8.8.8:53"}
	}
	return out
}

// Ping reports whether a DNS server answers at addr (host:port).
func Ping(addr string, timeout time.Duration) error {
	m := new(dns.Msg)
	m.SetQuestion(Domain, dns.TypeSOA)
	c := &dns.Client{Net: "udp", Timeout: timeout}
	_, _, err := c.Exchange(m, addr)
	return err
}

// Run is the jokku-dns service: it serves the zone the agent writes, picking
// up changes within a second, until ctx is done.
func Run(ctx context.Context, dataDir string, log *slog.Logger) error {
	path := ZonePath(dataDir)
	srv := NewServer(HostResolvers())
	var (
		current   *listener
		listen    string
		stamp     string
		upstreams = time.Now()
		failed    string
	)
	defer func() {
		if current != nil {
			current.close()
		}
	}()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if info, err := os.Stat(path); err == nil {
			if s := fmt.Sprint(info.ModTime().UnixNano(), info.Size()); s != stamp {
				zone, err := ReadZone(path)
				if err != nil {
					log.Error("reading the zone", "err", err)
				} else {
					stamp = s
					srv.SetRecords(zone.Records)
					listen = zone.Listen
				}
			}
		}
		if time.Since(upstreams) > time.Minute {
			srv.SetUpstreams(HostResolvers())
			upstreams = time.Now()
		}
		if current != nil && (current.addr != listenAddr(listen) || current.dead()) {
			current.close()
			current = nil
		}
		if current == nil && listen != "" {
			l, err := serve(listenAddr(listen), srv)
			switch {
			case err != nil && err.Error() != failed:
				// The bridge address appears once the daemon has set up the
				// network; until then this fails, and is retried.
				failed = err.Error()
				log.Warn("cannot listen yet", "addr", listen, "err", err)
			case err == nil:
				failed = ""
				current = l
				log.Info("serving DNS for microVMs", "addr", l.addr, "upstreams", strings.Join(HostResolvers(), ","))
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// listenAddr adds the DNS port to a bare address.
func listenAddr(listen string) string {
	if _, _, err := net.SplitHostPort(listen); err == nil {
		return listen
	}
	return net.JoinHostPort(listen, fmt.Sprint(Port))
}

// ReadZone loads the agent's zone file.
func ReadZone(path string) (*Zone, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var z Zone
	return &z, json.Unmarshal(b, &z)
}

// WriteZone saves a zone atomically, so the service never reads half a file.
func WriteZone(path string, z Zone) error {
	b, err := json.Marshal(z)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// listener is the server on one address, over UDP and TCP.
type listener struct {
	addr     string
	udp, tcp *dns.Server
	errc     chan error
}

func serve(addr string, h dns.Handler) (*listener, error) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		pc.Close()
		return nil, err
	}
	return serveOn(addr, pc, ln, h), nil
}

func serveOn(addr string, pc net.PacketConn, ln net.Listener, h dns.Handler) *listener {
	l := &listener{addr: addr, udp: &dns.Server{PacketConn: pc, Handler: h}, tcp: &dns.Server{Listener: ln, Handler: h}, errc: make(chan error, 2)}
	go func() { l.errc <- l.udp.ActivateAndServe() }()
	go func() { l.errc <- l.tcp.ActivateAndServe() }()
	return l
}

func (l *listener) dead() bool {
	select {
	case <-l.errc:
		return true
	default:
		return false
	}
}

func (l *listener) close() {
	l.udp.Shutdown()
	l.tcp.Shutdown()
}

// Records computes the cluster's names from what runs where: for each app,
// <process>.<app>.internal and <app>.internal (the process that gets its
// HTTP traffic, or its only one). An instance counts while it is wanted and its node is up; healthy
// ones are preferred, so a name follows a deploy as the new instances pass
// their checks.
func Records(instances []Instance) map[string][]string {
	type key struct{ app, process string }
	wanted, healthy := map[key][]string{}, map[key][]string{}
	processes := map[string]map[string]bool{}
	web := map[string]string{}
	for _, in := range instances {
		k := key{strings.ToLower(in.App), strings.ToLower(in.Process)}
		if in.Web {
			web[k.app] = k.process
		}
		wanted[k] = append(wanted[k], in.IP)
		if in.Healthy {
			healthy[k] = append(healthy[k], in.IP)
		}
		if processes[k.app] == nil {
			processes[k.app] = map[string]bool{}
		}
		processes[k.app][k.process] = true
	}
	out := map[string][]string{}
	for k, ips := range wanted {
		if len(healthy[k]) > 0 {
			ips = healthy[k]
		}
		ips = append([]string(nil), ips...)
		sort.Strings(ips)
		out[k.process+"."+k.app+".internal"] = ips
	}
	for app, procs := range processes {
		main := ""
		switch {
		case web[app] != "":
			main = web[app]
		case len(procs) == 1:
			for p := range procs {
				main = p
			}
		}
		if main != "" {
			out[app+".internal"] = out[main+"."+app+".internal"]
		}
	}
	return out
}

// Instance is what Records needs to know about one instance.
type Instance struct {
	App, Process, IP string
	Healthy          bool
	Web              bool // its process gets the app's HTTP traffic
}

// Applier is the agent's side: it writes the zone for the service and tells
// whether the service answers, so VMs are only pointed at it when it does.
type Applier struct {
	DataDir string

	mu      sync.Mutex
	last    string
	checked time.Time
	ok      bool
}

// Apply writes the zone if it changed.
func (a *Applier) Apply(_ context.Context, z Zone) error {
	b, err := json.Marshal(z)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if string(b) == a.last {
		return nil
	}
	if err := WriteZone(ZonePath(a.DataDir), z); err != nil {
		return err
	}
	a.last = string(b)
	return nil
}

// Ready reports whether the service answers on addr (the node's bridge
// address). The answer is cached for a few seconds.
func (a *Applier) Ready(addr string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.checked) < 10*time.Second {
		return a.ok
	}
	a.ok = Ping(listenAddr(addr), 500*time.Millisecond) == nil
	a.checked = time.Now()
	return a.ok
}
