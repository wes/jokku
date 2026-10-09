package cluster_test

// These tests run a real control plane (store, controller, API over TLS) and
// real agents speaking the real protocol, with fake microVMs, so cluster
// behavior (placement, failover, draining, removal, outages) is exercised end
// to end in seconds.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/wes/jokku/internal/agent"
	"github.com/wes/jokku/internal/api"
	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/deploy"
	"github.com/wes/jokku/internal/dns"
	"github.com/wes/jokku/internal/gitrepo"
	"github.com/wes/jokku/internal/proxy"
	"github.com/wes/jokku/internal/session"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/vm"
)

func init() {
	store.NodeDownAfter = 1500 * time.Millisecond
	cluster.RescheduleAfter = 2 * time.Second
	cluster.RetryAfter = time.Second
	cluster.DrainGrace = 0
	// Well under agent.FenceAfter, as in production (25s against 2m): an
	// idle node hears from the control node at least once per poll.
	cluster.PollTimeout = 300 * time.Millisecond
	cluster.PollRecheck = 200 * time.Millisecond
	cluster.FailoverAfter = 4 * time.Second
	cluster.RetryFailoverAfter = time.Second
	agent.FenceAfter = time.Second
	agent.FenceCheckEvery = 200 * time.Millisecond
}

// cuttable is a node's way to the control node, which a test can cut, as a
// network partition would.
type cuttable struct {
	agent.ControlPlane
	cut *atomic.Bool
}

var errCut = errors.New("cut off from the control node")

func (c cuttable) State(ctx context.Context, etag string) (*types.NodeState, error) {
	if c.cut.Load() {
		time.Sleep(200 * time.Millisecond)
		return nil, errCut
	}
	return c.ControlPlane.State(ctx, etag)
}

func (c cuttable) Report(ctx context.Context, st *types.NodeStatus) error {
	if c.cut.Load() {
		return errCut
	}
	return c.ControlPlane.Report(ctx, st)
}

// fakeRuntime pretends to run VMs: a started VM is "running" until stopped
// or crashed.
type fakeRuntime struct {
	guestDir    string         // each fake VM's filesystem: <guestDir>/<id>
	restarts    map[string]int // app restarts the guest agent did in place (after an import)
	freezes     map[string]int // writes paused for a backup, by instance
	tokens      map[string]string
	node        string
	disks       *diskTracker
	mu          sync.Mutex
	running     map[string]vm.Spec
	starts      int
	order       []string    // app/process of each start, in order
	ignoreStops atomic.Bool // VMs ignore stop requests (a hung shutdown)
}

func newFakeRuntime(node string, disks *diskTracker) *fakeRuntime {
	return &fakeRuntime{node: node, disks: disks, running: map[string]vm.Spec{}, restarts: map[string]int{}, freezes: map[string]int{}, tokens: map[string]string{}}
}

// Session connects to a fake guest agent that speaks the real protocol: exec
// reports what it was asked and echoes stdin; export and import work on the
// VM's directory under guestDir.
func (f *fakeRuntime) Session(_ context.Context, id string) (io.ReadWriteCloser, string, error) {
	f.mu.Lock()
	_, running := f.running[id]
	token := f.tokens[id]
	f.mu.Unlock()
	if !running {
		return nil, "", fmt.Errorf("instance %s is not running", id)
	}
	host, guest := net.Pipe()
	go f.guest(id, token, guest)
	return host, token, nil
}

func (f *fakeRuntime) guest(id, token string, conn net.Conn) {
	c := session.New(conn)
	defer c.Close()
	req, err := c.ReadRequest()
	if err != nil {
		return
	}
	if req.Token != token {
		c.Exit(1, "unauthorized")
		return
	}
	dir := filepath.Join(f.guestDir, id, filepath.FromSlash(req.Path))
	switch req.Op {
	case session.OpFreeze:
		f.mu.Lock()
		f.freezes[id]++
		f.mu.Unlock()
		c.Write(session.FrameStdout, []byte("frozen\n"))
		for {
			if typ, _, err := c.Read(); err != nil || typ == session.FrameEOF {
				break
			}
		}
		c.Exit(0, "")
	case session.OpExec:
		if len(req.Argv) == 2 && req.Argv[0] == "exit" {
			code, _ := strconv.Atoi(req.Argv[1])
			c.Exit(code, "")
			return
		}
		fmt.Fprintf(c.Writer(session.FrameStdout), "ran %v root=%v tty=%v\n", req.Argv, req.Root, req.TTY)
		for {
			typ, p, err := c.Read()
			if err != nil || typ == session.FrameEOF {
				break
			}
			if typ == session.FrameStdin {
				c.Write(session.FrameStdout, p)
			}
		}
		c.Exit(0, "")
	case session.OpExport:
		os.MkdirAll(dir, 0o755)
		if err := session.Archive(dir, c.Writer(session.FrameStdout)); err != nil {
			c.Exit(1, err.Error())
			return
		}
		c.Exit(0, "")
	case session.OpImport:
		os.MkdirAll(dir, 0o755)
		pr, pw := io.Pipe()
		go func() {
			for {
				typ, p, err := c.Read()
				if err != nil {
					pw.CloseWithError(err)
					return
				}
				if typ == session.FrameEOF {
					pw.Close()
					return
				}
				pw.Write(p)
			}
		}()
		if err := session.Extract(pr, dir, session.ExtractOptions{Clear: req.Clear}); err != nil {
			c.Exit(1, err.Error())
			return
		}
		f.mu.Lock()
		f.restarts[id]++
		f.mu.Unlock()
		c.Exit(0, "")
	}
}

// diskTracker follows which VM, on any node, has each volume attached, and
// records it if two ever do at once.
type diskTracker struct {
	mu         sync.Mutex
	attached   map[string]string // volume ID -> node/instance
	violations []string
}

func volumeID(disk string) string { return strings.TrimSuffix(filepath.Base(disk), ".ext4") }

func (d *diskTracker) attach(node string, s vm.Spec) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, v := range s.Volumes {
		id := volumeID(v.Disk)
		if other, ok := d.attached[id]; ok {
			d.violations = append(d.violations, fmt.Sprintf("volume %s attached to %s/%s while %s has it", id, node, s.ID, other))
		}
		d.attached[id] = node + "/" + s.ID
	}
}

func (d *diskTracker) detach(node string, s vm.Spec) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, v := range s.Volumes {
		if id := volumeID(v.Disk); d.attached[id] == node+"/"+s.ID {
			delete(d.attached, id)
		}
	}
}

func (d *diskTracker) problems() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.violations...)
}

func (f *fakeRuntime) Available() error                    { return nil }
func (f *fakeRuntime) EnsureNetwork(context.Context) error { return nil }
func (f *fakeRuntime) Remove(context.Context, string) error {
	return nil
}
func (f *fakeRuntime) Start(_ context.Context, s vm.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.running[s.ID]; ok {
		return fmt.Errorf("unit for %s already exists", s.ID) // as systemd-run says
	}
	for _, v := range s.Volumes {
		if _, err := os.Stat(v.Disk); err != nil {
			return fmt.Errorf("volume disk: %w", err) // as Firecracker would
		}
	}
	if s.Guest.AgentToken == "" {
		s.Guest.AgentToken = fmt.Sprintf("token-%s-%d", s.ID, f.starts)
	}
	f.tokens[s.ID] = s.Guest.AgentToken
	f.running[s.ID] = s
	f.starts++
	f.order = append(f.order, s.App+"/"+s.Process)
	f.disks.attach(f.node, s)
	return nil
}

// CreateVolume makes a sparse file; a real one would also get a filesystem.
func (f *fakeRuntime) CreateVolume(_ context.Context, path string, sizeMB int) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Truncate(int64(sizeMB) << 20)
}

func (f *fakeRuntime) GrowVolume(_ context.Context, path string, sizeMB int) error {
	return os.Truncate(path, int64(sizeMB)<<20)
}

func (f *fakeRuntime) Attached(context.Context) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for _, s := range f.running {
		for _, v := range s.Volumes {
			out[v.Disk] = true
		}
	}
	return out, nil
}

func (f *fakeRuntime) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}
func (f *fakeRuntime) Stop(_ context.Context, id string, _ time.Duration) error {
	if f.ignoreStops.Load() {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disks.detach(f.node, f.running[id])
	delete(f.running, id)
	return nil
}
func (f *fakeRuntime) Units(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for id := range f.running {
		out[id] = "active"
	}
	return out, nil
}
func (f *fakeRuntime) Usage(context.Context, []string) (map[string]vm.Usage, error) {
	return map[string]vm.Usage{}, nil
}
func (f *fakeRuntime) CheckTCP(addr string) bool {
	host, _, _ := net.SplitHostPort(addr)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.running {
		if s.IP == host {
			return true
		}
	}
	return false
}

// crash makes a VM exit as if the app died.
func (f *fakeRuntime) crash(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disks.detach(f.node, f.running[id])
	delete(f.running, id)
}

func (f *fakeRuntime) apps() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for _, s := range f.running {
		out[s.App]++
	}
	return out
}

type fakeMesh struct {
	mu    sync.Mutex
	peers []types.Peer
}

func (m *fakeMesh) Sync(_ context.Context, _ types.NodeIdentity, peers []types.Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peers = peers
	return nil
}

func (m *fakeMesh) names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, p := range m.peers {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

type fakeProxy struct {
	mu sync.Mutex
	ps *types.ProxyState
}

func (p *fakeProxy) Apply(_ context.Context, ps *types.ProxyState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ps = ps
	return nil
}

func (p *fakeProxy) upstreams(app string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ps == nil {
		return nil
	}
	var out []string
	for _, r := range p.ps.Routes {
		if r.App == app && len(r.Hosts) > 0 {
			out = append(out, r.Upstreams...)
		}
	}
	sort.Strings(out)
	return out
}

// fakeResolver records the zone a node's agent hands its DNS service.
type fakeResolver struct {
	mu   sync.Mutex
	zone dns.Zone
}

func (r *fakeResolver) Apply(_ context.Context, z dns.Zone) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.zone = z
	return nil
}

func (r *fakeResolver) Ready(string) bool { return true }

func (r *fakeResolver) get() dns.Zone {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.zone
}

type node struct {
	name       string
	dir        string
	rt         *fakeRuntime
	mesh       *fakeMesh
	proxy      *fakeProxy
	resolver   *fakeResolver
	token      string
	agentToken string
	api        *httptest.Server // the agent API, where other nodes copy volumes from
	cancel     context.CancelFunc
	stopped    chan struct{}
	cut        atomic.Bool // cut off from the control node
}

type harness struct {
	t       *testing.T
	ctx     context.Context
	dir     string
	st      *store.Store
	ctl     *cluster.Controller
	pipe    *deploy.Pipeline
	ts      *httptest.Server
	pin     string
	down    atomic.Bool // simulates the control node being unreachable
	nodes   map[string]*node
	log     *slog.Logger
	release map[string]int64 // app -> current release ID
	disks   *diskTracker
	builder *fakeBuilder
	client  *client.Client

	apiMu    sync.Mutex
	apiAddrs map[string]string // node -> its agent API's address
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "jokku.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cert, pin, err := cluster.LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctx: ctx, dir: dir, st: st, pin: pin, nodes: map[string]*node{}, log: log, release: map[string]int64{},
		disks: &diskTracker{attached: map[string]string{}}, apiAddrs: map[string]string{}}
	h.ctl = cluster.New(&cluster.Controller{
		Store: st, Log: log, Self: "control", ClusterCIDR: netip.MustParsePrefix("10.210.0.0/16"),
		Version: "test", Pin: pin, TickEvery: 200 * time.Millisecond, DataDir: dir,
		AgentAddr: func(n store.Node) string {
			h.apiMu.Lock()
			defer h.apiMu.Unlock()
			return h.apiAddrs[n.Name]
		},
	})
	h.builder = newFakeBuilder(dir)
	h.pipe = &deploy.Pipeline{Store: st, Builder: h.builder, Cluster: h.ctl, DataDir: dir, Log: log}
	srv := api.New(api.Config{
		Store: st, Git: &gitrepo.Manager{Dir: filepath.Join(dir, "git")}, Deployer: h.pipe, Log: log, DataDir: dir, Cluster: h.ctl,
	})
	public := srv.Public()
	h.ts = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.down.Load() {
			panic(http.ErrAbortHandler) // drop the connection, as an outage would
		}
		public.ServeHTTP(&outageWriter{ResponseWriter: w, down: &h.down}, r)
	}))
	h.ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	h.ts.StartTLS()
	t.Cleanup(h.ts.Close)
	h.ctl.Address = h.ts.Listener.Addr().String()

	if err := st.RegisterControlNode(ctx, store.Node{Name: "control", CPUs: 4, MemoryMB: 4096, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeWireGuard(ctx, "control", wgKey(t), "127.0.0.1:51820"); err != nil {
		t.Fatal(err)
	}
	go h.ctl.Run(ctx)
	if err := st.SetNodeAgentToken(ctx, "control", "control-agent-token"); err != nil {
		t.Fatal(err)
	}
	h.startNode("control", "", "control-agent-token", dir, &cluster.LocalPlane{C: h.ctl, DataDir: dir})

	// The admin API, as the CLI reaches it (unix socket paths must be short).
	sockDir, err := os.MkdirTemp("", "jk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	ln, err := net.Listen("unix", filepath.Join(sockDir, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	admin := &http.Server{Handler: srv}
	go admin.Serve(ln)
	t.Cleanup(func() { admin.Close() })
	h.client = client.New(client.Target{Socket: filepath.Join(sockDir, "api.sock")}, "tester")
	return h
}

// outageWriter drops responses that are in flight when the outage starts
// (a long-poll that began before it, for example).
type outageWriter struct {
	http.ResponseWriter
	down *atomic.Bool
}

func (w *outageWriter) WriteHeader(code int) {
	if w.down.Load() {
		panic(http.ErrAbortHandler)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *outageWriter) Write(b []byte) (int, error) {
	if w.down.Load() {
		panic(http.ErrAbortHandler)
	}
	return w.ResponseWriter.Write(b)
}

func (w *outageWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func wgKey(t *testing.T) string {
	k, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k.PublicKey().String()
}

func (h *harness) startNode(name, token, agentToken, dir string, plane agent.ControlPlane) *node {
	n := h.nodes[name]
	if n == nil {
		n = &node{name: name, dir: dir, rt: newFakeRuntime(name, h.disks), mesh: &fakeMesh{}, proxy: &fakeProxy{}, resolver: &fakeResolver{},
			token: token, agentToken: agentToken}
		n.rt.guestDir = filepath.Join(dir, "guests")
		h.nodes[name] = n
	}
	ctx, cancel := context.WithCancel(h.ctx)
	n.cancel, n.stopped = cancel, make(chan struct{})
	a := agent.New(agent.Config{
		Control: cuttable{plane, &n.cut}, Runtime: n.rt, Mesh: n.mesh, Proxy: n.proxy, Resolver: n.resolver, DataDir: dir, Version: "test", Log: h.log,
		GCArtifacts: name != "control", ReconcileEvery: 100 * time.Millisecond, ReportEvery: 200 * time.Millisecond, UpFor: time.Second,
		Metrics: func() types.NodeMetrics { return types.NodeMetrics{CPUs: 4, MemoryMB: 4096} },
	})
	handler := a.Handler(n.agentToken)
	if name == "control" {
		// The control node's agent goes down with it.
		inner := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h.down.Load() {
				panic(http.ErrAbortHandler)
			}
			inner.ServeHTTP(w, r)
		})
	}
	n.api = httptest.NewServer(handler)
	h.apiMu.Lock()
	h.apiAddrs[name] = n.api.Listener.Addr().String()
	h.apiMu.Unlock()
	stopped, api := n.stopped, n.api
	go func() {
		a.Run(ctx)
		close(stopped)
	}()
	// Registered after the node's temp dir, so it runs first: the agent
	// stops before its files are deleted.
	h.t.Cleanup(func() {
		cancel()
		<-stopped
		api.Close()
	})
	return n
}

func (h *harness) remote(token string) agent.ControlPlane {
	return agent.NewRemote(h.ts.URL, token, cluster.PinnedTLS(h.pin))
}

// join adds a worker the way "jokku setup --join" does.
func (h *harness) join(name string) *node {
	h.t.Helper()
	tok, err := h.ctl.CreateJoinToken(h.ctx, time.Hour, false)
	if err != nil {
		h.t.Fatal(err)
	}
	res, err := h.postJoin(types.JoinRequest{
		Protocol: types.ProtocolVersion, Version: "test", Token: tok.Token, Name: name,
		PublicKey: wgKey(h.t), Endpoint: "127.0.0.1:51820", CPUs: 4, MemoryMB: 4096,
	}, h.pin)
	if err != nil {
		h.t.Fatal(err)
	}
	dir := h.t.TempDir()
	return h.startNode(name, res.NodeToken, res.AgentToken, dir, h.remote(res.NodeToken))
}

func (h *harness) postJoin(req types.JoinRequest, pin string) (*types.JoinResponse, error) {
	b, _ := json.Marshal(req)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cluster.PinnedTLS(pin)}}
	resp, err := c.Post(h.ts.URL+"/v1/cluster/join", "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e types.Error
		json.NewDecoder(resp.Body).Decode(&e)
		return nil, fmt.Errorf("%s: %s", resp.Status, e.Error)
	}
	var res types.JoinResponse
	return &res, json.NewDecoder(resp.Body).Decode(&res)
}

// kill stops a node's agent, as if the machine died. Its fake VMs stay as
// they were.
func (h *harness) kill(name string) {
	n := h.nodes[name]
	n.cancel()
	<-n.stopped
	n.api.CloseClientConnections()
	n.api.Close()
}

func (h *harness) revive(name string) {
	n := h.nodes[name]
	h.startNode(name, n.token, n.agentToken, n.dir, h.remote(n.token))
}

// deploy creates an app with a fake built release and rolls it out at the
// given scale.
func (h *harness) deploy(app string, web int) {
	h.t.Helper()
	h.deployWith(app, web, nil)
}

// deployWith is deploy with a chance to set the app up (volumes, say) before
// the rollout.
func (h *harness) deployWith(app string, web int, setup func()) {
	h.t.Helper()
	ctx := h.ctx
	if _, err := h.st.CreateApp(ctx, app); err != nil {
		h.t.Fatal(err)
	}
	if setup != nil {
		setup()
	}
	h.st.SetProperty(ctx, app, "checks", "wait-to-retire", "0")
	h.st.UpdateDomains(ctx, app, types.DomainsPatch{Add: []string{app + ".example.test"}})
	h.st.Scale(ctx, app, map[string]int{"web": web})
	artifact := filepath.Join(h.dir, "artifacts", app+".ext4")
	os.MkdirAll(filepath.Dir(artifact), 0o755)
	content := []byte("rootfs of " + app)
	os.WriteFile(artifact, content, 0o644)
	sum := sha256.Sum256(content)
	rel := &store.Release{
		App: app, Artifact: artifact, ArtifactSHA: hex.EncodeToString(sum[:]), ArtifactLen: int64(len(content)),
		Processes: map[string][]string{"web": {"/bin/app"}}, Image: store.ImageConfig{Port: 5000},
		ConfigVars: map[string]string{},
	}
	if err := h.st.CreateRelease(ctx, rel); err != nil {
		h.t.Fatal(err)
	}
	h.st.SetCurrentRelease(ctx, app, rel.ID)
	h.release[app] = rel.ID
	var out []string
	if err := h.pipe.PS(ctx, app, "restart", "test", func(l string) { out = append(out, l) }); err != nil {
		h.t.Fatalf("rollout of %s failed: %v\n%s", app, err, strings.Join(out, "\n"))
	}
}

// eventually retries check until it passes or the timeout expires.
func eventually(t *testing.T, timeout time.Duration, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s: %v", what, err)
}

// placement counts each node's healthy, wanted instances of app.
func (h *harness) placement(app string) map[string]int {
	insts, _ := h.st.Instances(h.ctx, app)
	out := map[string]int{}
	for _, in := range insts {
		if in.Desired == store.DesiredRunning && in.State == store.StateHealthy {
			out[in.Node]++
		}
	}
	return out
}

func TestJoinSpreadsAndRoutes(t *testing.T) {
	h := newHarness(t)
	w1, w2 := h.join("w1"), h.join("w2")
	eventually(t, 5*time.Second, "workers report in", func() error {
		st, _ := h.ctl.Status(h.ctx)
		if st.Totals.NodesReady != 3 {
			return fmt.Errorf("%d of 3 nodes ready", st.Totals.NodesReady)
		}
		return nil
	})
	h.deploy("web", 3)

	if got := h.placement("web"); len(got) != 3 || got["control"] != 1 || got["w1"] != 1 || got["w2"] != 1 {
		t.Fatalf("instances should spread one per node, got %v", got)
	}
	for _, n := range []*node{h.nodes["control"], w1, w2} {
		if n.rt.apps()["web"] != 1 {
			t.Errorf("%s runs %d web VMs, want 1", n.name, n.rt.apps()["web"])
		}
	}
	// Workers downloaded and verified the root filesystem.
	if b, err := os.ReadFile(filepath.Join(w1.dir, "artifacts", "web.ext4")); err != nil || string(b) != "rootfs of web" {
		t.Errorf("w1 artifact: %q, %v", b, err)
	}
	// Every ingress node routes to all three instances, on whichever node.
	eventually(t, 3*time.Second, "proxies route to every instance", func() error {
		for _, n := range h.nodes {
			if got := n.proxy.upstreams("web"); len(got) != 3 {
				return fmt.Errorf("%s routes to %v", n.name, got)
			}
		}
		return nil
	})
	// The mesh is complete.
	eventually(t, 3*time.Second, "mesh peers", func() error {
		want := map[string]string{"control": "w1,w2", "w1": "control,w2", "w2": "control,w1"}
		for name, n := range h.nodes {
			if got := strings.Join(n.mesh.names(), ","); got != want[name] {
				return fmt.Errorf("%s peers %s, want %s", name, got, want[name])
			}
		}
		return nil
	})
}

func TestDeadNodeIsRescheduled(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.join("w2")
	eventually(t, 5*time.Second, "workers ready", func() error {
		if st, _ := h.ctl.Status(h.ctx); st.Totals.NodesReady != 3 {
			return fmt.Errorf("not ready")
		}
		return nil
	})
	h.deploy("api", 3)
	h.kill("w1")

	// Routes drop w1's instance as soon as w1 is considered down...
	eventually(t, 5*time.Second, "routes exclude the dead node", func() error {
		got := h.nodes["control"].proxy.upstreams("api")
		if len(got) != 2 {
			return fmt.Errorf("routes %v", got)
		}
		return nil
	})
	// ...and after RescheduleAfter its instance runs elsewhere.
	eventually(t, 10*time.Second, "replacement healthy elsewhere", func() error {
		got := h.placement("api")
		if got["w1"] != 0 || got["control"]+got["w2"] != 3 {
			return fmt.Errorf("placement %v", got)
		}
		return nil
	})
	eventually(t, 3*time.Second, "routes back to three", func() error {
		if got := h.nodes["w2"].proxy.upstreams("api"); len(got) != 3 {
			return fmt.Errorf("routes %v", got)
		}
		return nil
	})
	// w1 comes back: it stops the VM it is no longer told to run.
	if h.nodes["w1"].rt.apps()["api"] != 1 {
		t.Fatal("w1's VM should still be running while it was unreachable")
	}
	h.revive("w1")
	eventually(t, 5*time.Second, "returning node stops its stale VM", func() error {
		if n := h.nodes["w1"].rt.apps()["api"]; n != 0 {
			return fmt.Errorf("w1 still runs %d", n)
		}
		return nil
	})
}

func TestDrainMovesInstancesWithoutDowntime(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	eventually(t, 5*time.Second, "worker ready", func() error {
		if st, _ := h.ctl.Status(h.ctx); st.Totals.NodesReady != 2 {
			return fmt.Errorf("not ready")
		}
		return nil
	})
	h.deploy("shop", 2) // one on each node
	if err := h.st.SetNodeFlag(h.ctx, "w1", "draining", true); err != nil {
		t.Fatal(err)
	}
	h.ctl.Changed()

	// While draining, there is always at least one healthy upstream.
	stop := make(chan struct{})
	var gaps atomic.Int32
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if len(h.nodes["control"].proxy.upstreams("shop")) == 0 {
				gaps.Add(1)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	eventually(t, 10*time.Second, "w1 drained", func() error {
		if got := h.placement("shop"); got["control"] != 2 || got["w1"] != 0 {
			return fmt.Errorf("placement %v", got)
		}
		if n := h.nodes["w1"].rt.apps()["shop"]; n != 0 {
			return fmt.Errorf("w1 still runs %d", n)
		}
		return nil
	})
	close(stop)
	if gaps.Load() > 0 {
		t.Errorf("the app had no upstreams %d times during the drain", gaps.Load())
	}
	// A drained node can be removed without --force.
	insts, _ := h.st.Instances(h.ctx, "")
	for _, in := range insts {
		if in.Node == "w1" && in.Desired == store.DesiredRunning {
			t.Fatalf("w1 still has a wanted instance: %+v", in)
		}
	}
}

func TestForceRemovedNodeStopsItsVMs(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	eventually(t, 5*time.Second, "worker ready", func() error {
		if st, _ := h.ctl.Status(h.ctx); st.Totals.NodesReady != 2 {
			return fmt.Errorf("not ready")
		}
		return nil
	})
	h.deploy("blog", 2)
	if h.nodes["w1"].rt.apps()["blog"] != 1 {
		t.Fatal("expected one instance on w1")
	}
	if err := h.st.DeleteNode(h.ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	h.ctl.Changed()
	eventually(t, 5*time.Second, "removed node stops its VMs", func() error {
		if n := h.nodes["w1"].rt.apps()["blog"]; n != 0 {
			return fmt.Errorf("w1 still runs %d", n)
		}
		return nil
	})
	eventually(t, 10*time.Second, "instance replaced on the control node", func() error {
		if got := h.placement("blog"); got["control"] != 2 {
			return fmt.Errorf("placement %v", got)
		}
		return nil
	})
}

func TestWorkersKeepRunningWhileControlIsDown(t *testing.T) {
	h := newHarness(t)
	w1 := h.join("w1")
	eventually(t, 5*time.Second, "worker ready", func() error {
		if st, _ := h.ctl.Status(h.ctx); st.Totals.NodesReady != 2 {
			return fmt.Errorf("not ready")
		}
		return nil
	})
	h.deploy("site", 2)
	eventually(t, 3*time.Second, "w1 routes to both instances", func() error {
		if got := w1.proxy.upstreams("site"); len(got) != 2 {
			return fmt.Errorf("routes %v", got)
		}
		return nil
	})
	routesBefore := w1.proxy.upstreams("site")

	h.down.Store(true)
	time.Sleep(3 * time.Second) // well past NodeDownAfter
	if w1.rt.apps()["site"] != 1 {
		t.Fatal("w1 stopped its instance while the control node was unreachable")
	}
	if got := w1.proxy.upstreams("site"); strings.Join(got, ",") != strings.Join(routesBefore, ",") {
		t.Fatalf("w1 changed its routes without the control node: %v", got)
	}
	// A restarted agent recovers its last state from disk.
	h.kill("w1")
	h.revive("w1")
	time.Sleep(time.Second)
	if w1.rt.apps()["site"] != 1 {
		t.Fatal("w1 lost its instance after restarting while the control node was down")
	}

	h.down.Store(false)
	eventually(t, 5*time.Second, "w1 reports again", func() error {
		if st, _ := h.ctl.Status(h.ctx); st.Totals.NodesReady != 2 {
			return fmt.Errorf("not ready")
		}
		return nil
	})
}

func TestRestartedAgentAdoptsRunningVMs(t *testing.T) {
	h := newHarness(t)
	w1 := h.join("w1")
	eventually(t, 5*time.Second, "worker ready", func() error {
		if st, _ := h.ctl.Status(h.ctx); st.Totals.NodesReady != 2 {
			return fmt.Errorf("not ready")
		}
		return nil
	})
	h.deploy("app", 2)
	before := w1.rt.startCount()
	h.kill("w1") // e.g. jokku on w1 is updated
	h.revive("w1")
	time.Sleep(2 * time.Second)
	if n := w1.rt.apps()["app"]; n != 1 {
		t.Fatalf("w1 runs %d instances after its agent restarted, want 1", n)
	}
	if got := w1.rt.startCount(); got != before {
		t.Fatalf("the restarted agent started %d VMs again instead of adopting them", got-before)
	}
	eventually(t, 3*time.Second, "adopted instance healthy", func() error {
		if got := h.placement("app"); got["w1"] != 1 {
			return fmt.Errorf("placement %v", got)
		}
		return nil
	})
}

func TestCrashedInstancesAreRestarted(t *testing.T) {
	h := newHarness(t)
	h.deploy("worker", 1)
	ctl := h.nodes["control"]
	var id string
	for k := range ctl.rt.running {
		id = k
	}
	ctl.rt.crash(id)
	eventually(t, 5*time.Second, "crash restarted", func() error {
		in, err := h.st.Instance(h.ctx, id)
		if err != nil {
			return err
		}
		if in.Restarts != 1 || in.State != store.StateHealthy {
			return fmt.Errorf("state %s, restarts %d", in.State, in.Restarts)
		}
		return nil
	})
	events, _ := h.st.Events(h.ctx, "worker", 20)
	found := false
	for _, e := range events {
		found = found || strings.Contains(e.Message, "crashed")
	}
	if !found {
		t.Error("no crash event recorded")
	}
}

func TestJoinRejectsBadTokens(t *testing.T) {
	h := newHarness(t)
	tok, _ := h.ctl.CreateJoinToken(h.ctx, time.Hour, false)
	req := types.JoinRequest{Protocol: types.ProtocolVersion, Version: "test", Token: tok.Token, Name: "n1",
		PublicKey: wgKey(t), Endpoint: "127.0.0.1:51820", CPUs: 1, MemoryMB: 1024}
	if _, err := h.postJoin(req, h.pin); err != nil {
		t.Fatal(err)
	}
	req.Name, req.PublicKey = "n2", wgKey(t)
	if _, err := h.postJoin(req, h.pin); err == nil {
		t.Error("a single-use token worked twice")
	}
	tok2, _ := h.ctl.CreateJoinToken(h.ctx, time.Hour, false)
	req.Token, req.Version = tok2.Token, "other"
	if _, err := h.postJoin(req, h.pin); err == nil || !strings.Contains(err.Error(), "runs jokku other") {
		t.Errorf("version mismatch: %v", err)
	}
	req.Version = "test"
	if _, err := h.postJoin(req, "wrong-pin"); err == nil || !strings.Contains(err.Error(), "TLS key") {
		t.Errorf("pin mismatch: %v", err)
	}
	reusable, _ := h.ctl.CreateJoinToken(h.ctx, time.Hour, true)
	for i, name := range []string{"r1", "r2"} {
		req.Token, req.Name, req.PublicKey = reusable.Token, name, wgKey(t)
		if _, err := h.postJoin(req, h.pin); err != nil {
			t.Errorf("reusable token, join %d: %v", i, err)
		}
	}
}

func TestSharedCertificateStore(t *testing.T) {
	h := newHarness(t)
	tok, _ := h.ctl.CreateJoinToken(h.ctx, time.Hour, false)
	res, err := h.postJoin(types.JoinRequest{Protocol: types.ProtocolVersion, Version: "test", Token: tok.Token, Name: "edge",
		PublicKey: wgKey(t), Endpoint: "127.0.0.1:51820", CPUs: 1, MemoryMB: 1024}, h.pin)
	if err != nil {
		t.Fatal(err)
	}
	newStorage := func() *proxy.ClusterStorage {
		s := &proxy.ClusterStorage{URL: h.ts.URL, Token: res.NodeToken, Pin: h.pin, Cache: t.TempDir()}
		if err := s.Provision(caddy.Context{}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := newStorage(), newStorage()
	ctx := h.ctx

	// What one node stores, another reads.
	if err := a.Store(ctx, "certificates/acme/example.com/example.com.crt", []byte("CERT")); err != nil {
		t.Fatal(err)
	}
	if v, err := b.Load(ctx, "certificates/acme/example.com/example.com.crt"); err != nil || string(v) != "CERT" {
		t.Fatalf("load from another node: %q, %v", v, err)
	}
	if _, err := b.Load(ctx, "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing key: %v", err)
	}
	if keys, err := b.List(ctx, "certificates", false); err != nil || strings.Join(keys, ",") != "certificates/acme" {
		t.Fatalf("list: %v, %v", keys, err)
	}
	if info, err := b.Stat(ctx, "certificates/acme"); err != nil || info.IsTerminal {
		t.Fatalf("stat dir: %+v, %v", info, err)
	}

	// Locks are cluster-wide.
	if err := a.Lock(ctx, "issue_cert_example.com"); err != nil {
		t.Fatal(err)
	}
	lockCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	if err := b.Lock(lockCtx, "issue_cert_example.com"); err == nil {
		t.Fatal("two nodes held the same lock")
	}
	cancel()
	a.Unlock(ctx, "issue_cert_example.com")
	if err := b.Lock(ctx, "issue_cert_example.com"); err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}

	// With the control node unreachable, reads come from the local mirror.
	h.down.Store(true)
	if v, err := b.Load(ctx, "certificates/acme/example.com/example.com.crt"); err != nil || string(v) != "CERT" {
		t.Fatalf("load during outage: %q, %v", v, err)
	}
	h.down.Store(false)

	// A certificate only one server had (from before it joined) is shared
	// the first time it is read.
	legacy := t.TempDir()
	os.MkdirAll(filepath.Join(legacy, "old"), 0o755)
	os.WriteFile(filepath.Join(legacy, "old", "cert"), []byte("OLD"), 0o600)
	c := &proxy.ClusterStorage{URL: h.ts.URL, Token: res.NodeToken, Pin: h.pin, Cache: t.TempDir(), Legacy: legacy}
	c.Provision(caddy.Context{})
	if v, err := c.Load(ctx, "old/cert"); err != nil || string(v) != "OLD" {
		t.Fatalf("legacy cert: %q, %v", v, err)
	}
	if v, err := a.Load(ctx, "old/cert"); err != nil || string(v) != "OLD" {
		t.Fatalf("promoted cert from another node: %q, %v", v, err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "old", "cert")); err == nil {
		t.Error("promoted cert should leave the legacy storage")
	}
	if err := a.Delete(ctx, "certificates"); err != nil {
		t.Fatal(err)
	}
	// b still has a mirrored copy, but the control node is the truth.
	if b.Exists(ctx, "certificates/acme/example.com/example.com.crt") {
		t.Error("a key deleted on one node still exists on another")
	}
	if _, err := b.Load(ctx, "certificates/acme/example.com/example.com.crt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("deleted key loaded from a stale mirror: %v", err)
	}
	if v, _, err := h.st.CertGet(ctx, "certificates/acme/example.com/example.com.crt"); err == nil {
		t.Errorf("a stale mirror resurrected a deleted key: %q", v)
	}
}
