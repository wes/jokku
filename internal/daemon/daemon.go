// Package daemon runs a Jokku node. The control node runs the store, the API,
// the scheduler and an agent; a worker (one with a node.json from joining a
// cluster) runs only an agent that follows the control node.
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/wes/jokku/internal/agent"
	"github.com/wes/jokku/internal/api"
	"github.com/wes/jokku/internal/build"
	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/deploy"
	"github.com/wes/jokku/internal/gitrepo"
	"github.com/wes/jokku/internal/mesh"
	"github.com/wes/jokku/internal/proxy"
	"github.com/wes/jokku/internal/sshkeys"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/version"
	"github.com/wes/jokku/internal/vm"
)

// Defaults for a server installed by install.sh / "jokku setup".
const (
	DefaultDataDir     = "/var/lib/jokku"
	DefaultUser        = "jokku"
	DefaultClusterCIDR = "10.210.0.0/16"
)

type Config struct {
	DataDir string // state and builds, e.g. /var/lib/jokku
	Socket  string // API socket, e.g. /run/jokku/jokku.sock
	GitDir  string // bare repos; defaults to <DataDir>/git
	// GitUser is the account sshd runs git and CLI commands as. It owns the
	// repos and authorized_keys and may use the socket. Empty means the
	// current user (local development).
	GitUser string
	// AuthorizedKeys defaults to ~GitUser/.ssh/authorized_keys. "-" disables
	// writing it.
	AuthorizedKeys string
	ClusterCIDR    string
	NodeName       string
	// Advertise is the address other nodes reach this one at; default the
	// address used to reach the internet.
	Advertise string
	// APIListen is the TLS listener for joins and agents; "-" disables it.
	APIListen string
}

func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if cfg.GitDir == "" {
		cfg.GitDir = filepath.Join(cfg.DataDir, "git")
	}
	if cfg.APIListen == "" {
		cfg.APIListen = fmt.Sprintf(":%d", cluster.APIPort)
	}
	// Paths are handed to other processes (sshd, git hooks), so they must not
	// depend on the daemon's working directory.
	for _, p := range []*string{&cfg.DataDir, &cfg.GitDir, &cfg.Socket, &cfg.AuthorizedKeys} {
		if *p == "" || *p == "-" {
			continue
		}
		abs, err := filepath.Abs(*p)
		if err != nil {
			return err
		}
		*p = abs
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	if nf, err := LoadNodeFile(cfg.DataDir); err == nil && nf.Role == store.RoleWorker {
		return runWorker(ctx, cfg, nf, log)
	}
	return runControl(ctx, cfg, log)
}

func runControl(ctx context.Context, cfg Config, log *slog.Logger) error {
	cidr, err := netip.ParsePrefix(cfg.ClusterCIDR)
	if err != nil || !cidr.Addr().Is4() || cidr.Bits() != 16 {
		return fmt.Errorf("cluster CIDR must be an IPv4 /16, got %q", cfg.ClusterCIDR)
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	owner, home, err := lookupGitUser(cfg.GitUser)
	if err != nil {
		return err
	}
	if cfg.AuthorizedKeys == "" && home != "" {
		cfg.AuthorizedKeys = filepath.Join(home, ".ssh", "authorized_keys")
	}
	if cfg.AuthorizedKeys == "-" {
		cfg.AuthorizedKeys = ""
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "jokku.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	nodeName := cfg.NodeName
	if nodeName == "" {
		nodeName, _ = os.Hostname()
	}
	advertise := cfg.Advertise
	if advertise == "" {
		advertise = outboundIP()
	}
	err = st.RegisterControlNode(ctx, store.Node{
		Name: nodeName, Address: advertise, Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(), MemoryMB: totalMemoryMB(), Version: version.Version,
	})
	if err != nil {
		return fmt.Errorf("registering node: %w", err)
	}
	keyFile := filepath.Join(cfg.DataDir, "wireguard.key")
	if pub, err := mesh.PublicKey(keyFile); err != nil {
		log.Warn("creating the WireGuard key", "err", err)
	} else if err := st.SetNodeWireGuard(ctx, nodeName, pub, net.JoinHostPort(advertise, strconv.Itoa(mesh.Port))); err != nil {
		return err
	}
	// The control node's own proxy reaches the shared certificate store
	// like any other node's: with a token, over TLS on loopback.
	selfToken := randomToken()
	if err := st.SetNodeToken(ctx, nodeName, cluster.HashToken(selfToken)); err != nil {
		return err
	}
	cert, pin, err := cluster.LoadIdentity(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("loading the TLS identity: %w", err)
	}

	ctl := cluster.New(&cluster.Controller{
		Store: st, Log: log, Self: nodeName, ClusterCIDR: cidr, Version: version.Version,
		Address: net.JoinHostPort(advertise, strconv.Itoa(cluster.APIPort)), Pin: pin,
	})
	go backfillChecksums(ctx, st, filepath.Join(cfg.DataDir, "artifacts"), log)

	subnet, gateway := cluster.NodeSubnet(cidr, 1)
	ag := agent.New(agent.Config{
		Control: &cluster.LocalPlane{C: ctl, DataDir: cfg.DataDir},
		Runtime: &vm.Host{DataDir: cfg.DataDir, Bridge: "jokku0", Gateway: netip.PrefixFrom(gateway, subnet.Bits()), Cluster: cidr},
		Mesh:    &mesh.Mesh{KeyFile: keyFile},
		Proxy: &proxy.Applier{DataDir: cfg.DataDir, AdminSocket: proxy.AdminSocket, Storage: &proxy.ClusterStorage{
			URL: fmt.Sprintf("https://127.0.0.1:%d", cluster.APIPort), Token: selfToken, Pin: pin,
		}},
		DataDir: cfg.DataDir, DNS: vm.HostDNS(), Version: version.Version, Log: log,
	})
	pipeline := &deploy.Pipeline{
		Store: st, Builder: &build.Builder{DataDir: cfg.DataDir, Exe: exe}, Cluster: ctl, DataDir: cfg.DataDir, Log: log,
	}

	srv := api.New(api.Config{
		Store:          st,
		Git:            &gitrepo.Manager{Dir: cfg.GitDir, Exe: exe, Owner: owner},
		Deployer:       pipeline,
		Log:            log,
		DataDir:        cfg.DataDir,
		Exe:            exe,
		AuthorizedKeys: cfg.AuthorizedKeys,
		Owner:          owner,
		Cluster:        ctl,
	})
	if err := srv.SyncAuthorizedKeys(ctx); err != nil {
		return fmt.Errorf("writing %s: %w", cfg.AuthorizedKeys, err)
	}

	ln, err := listenSocket(cfg.Socket, owner)
	if err != nil {
		return err
	}
	servers := []*http.Server{{Handler: srv, ReadHeaderTimeout: 10 * time.Second}}
	listeners := []net.Listener{ln}
	if cfg.APIListen != "-" {
		tcp, err := net.Listen("tcp", cfg.APIListen)
		if err != nil {
			return fmt.Errorf("listening on %s for nodes: %w", cfg.APIListen, err)
		}
		tlsLn := tls.NewListener(tcp, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
		servers = append(servers, &http.Server{Handler: srv.Public(), ReadHeaderTimeout: 10 * time.Second})
		listeners = append(listeners, tlsLn)
	}

	go ctl.Run(ctx)
	go ag.Run(ctx)
	log.Info("jokku daemon started", "role", "control", "version", version.Version, "node", nodeName,
		"socket", cfg.Socket, "api", cfg.APIListen, "data_dir", cfg.DataDir)
	return serve(ctx, servers, listeners)
}

func runWorker(ctx context.Context, cfg Config, nf *NodeFile, log *slog.Logger) error {
	subnet, err := netip.ParsePrefix(nf.Node.Subnet)
	if err != nil {
		return fmt.Errorf("node.json: %w", err)
	}
	meshIP, err := netip.ParseAddr(nf.Node.MeshIP)
	if err != nil {
		return fmt.Errorf("node.json: %w", err)
	}
	cidr, err := netip.ParsePrefix(nf.Node.ClusterCIDR)
	if err != nil {
		return fmt.Errorf("node.json: %w", err)
	}
	controlURL := "https://" + nf.Control
	ag := agent.New(agent.Config{
		Control: agent.NewRemote(controlURL, nf.NodeToken, cluster.PinnedTLS(nf.Pin)),
		Runtime: &vm.Host{DataDir: cfg.DataDir, Bridge: "jokku0", Gateway: netip.PrefixFrom(meshIP, subnet.Bits()), Cluster: cidr},
		Mesh:    &mesh.Mesh{KeyFile: filepath.Join(cfg.DataDir, "wireguard.key")},
		Proxy: &proxy.Applier{DataDir: cfg.DataDir, AdminSocket: proxy.AdminSocket, Storage: &proxy.ClusterStorage{
			URL: controlURL, Token: nf.NodeToken, Pin: nf.Pin,
		}},
		DataDir: cfg.DataDir, DNS: vm.HostDNS(), Version: version.Version, Log: log, GCArtifacts: true,
	})
	go agent.Serve(ctx, net.JoinHostPort(nf.Node.MeshIP, strconv.Itoa(cluster.AgentPort)), nf.AgentToken, log)

	// The local socket answers "jokku version" (and setup's health check);
	// everything else belongs on the control node.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"version":%q,"commit":%q}`, version.Version, version.Commit)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(nf.Control)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, `{"error":"%s is a worker node; run jokku commands on the control node (%s)"}`, nf.Node.Name, host)
	})
	owner, _, _ := lookupGitUser("")
	ln, err := listenSocket(cfg.Socket, owner)
	if err != nil {
		return err
	}
	go ag.Run(ctx)
	log.Info("jokku daemon started", "role", "worker", "version", version.Version, "node", nf.Node.Name, "control", nf.Control)
	return serve(ctx, []*http.Server{{Handler: mux, ReadHeaderTimeout: 10 * time.Second}}, []net.Listener{ln})
}

// serve runs each server on its listener until ctx is done or one fails.
func serve(ctx context.Context, servers []*http.Server, listeners []net.Listener) error {
	errc := make(chan error, len(servers))
	for i, s := range servers {
		go func() { errc <- s.Serve(listeners[i]) }()
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(shutdownCtx)
	}
	return nil
}

// backfillChecksums records the SHA-256 of root filesystems built before
// clusters existed, so other nodes can verify their downloads.
func backfillChecksums(ctx context.Context, st *store.Store, dir string, log *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".ext4" || ctx.Err() != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		sum, size, err := build.FileSHA256(path)
		if err != nil {
			log.Warn("checksumming artifact", "artifact", e.Name(), "err", err)
			continue
		}
		st.SetArtifactSum(ctx, path, sum, size)
	}
}

func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// lookupGitUser resolves the git user's uid, gid and home. When the daemon is
// not root it cannot chown, so ownership is left alone.
func lookupGitUser(name string) (*sshkeys.Owner, string, error) {
	if name == "" {
		return nil, "", nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, "", fmt.Errorf("user %q not found (the installer creates it; use --git-user to pick another): %w", name, err)
	}
	if os.Geteuid() != 0 {
		return nil, u.HomeDir, nil
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return &sshkeys.Owner{UID: uid, GID: gid}, u.HomeDir, nil
}

// listenSocket opens the API socket, refusing to start if another daemon is
// already serving it, and makes it usable by the git user's group.
func listenSocket(path string, owner *sshkeys.Owner) (net.Listener, error) {
	if len(path) >= 104 { // sun_path is 104 bytes on macOS, 108 on Linux
		return nil, fmt.Errorf("socket path %s is too long for a unix socket (%d bytes, max 103)", path, len(path))
	}
	if conn, err := net.Dial("unix", path); err == nil {
		conn.Close()
		return nil, fmt.Errorf("another jokku daemon is already listening on %s", path)
	}
	os.Remove(path) // stale socket from a crash
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	if owner != nil {
		if err := os.Chown(path, 0, owner.GID); err != nil {
			ln.Close()
			return nil, err
		}
	}
	return ln, nil
}

// outboundIP is the source address used to reach the internet, which on a
// typical server is its public IP. No packets are sent.
func outboundIP() string {
	conn, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// OutboundIP is exported for setup, which advertises it when joining.
func OutboundIP() string { return outboundIP() }

// TotalMemoryMB is this machine's memory, reported when joining a cluster.
func TotalMemoryMB() int { return totalMemoryMB() }
