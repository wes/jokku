// Package daemon runs a Jokku node. In milestone 0 that is the control plane:
// the SQLite store and the API on a unix socket.
package daemon

import (
	"context"
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

	"github.com/wes/jokku/internal/api"
	"github.com/wes/jokku/internal/deploy"
	"github.com/wes/jokku/internal/gitrepo"
	"github.com/wes/jokku/internal/sshkeys"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/version"
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
}

func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if cfg.GitDir == "" {
		cfg.GitDir = filepath.Join(cfg.DataDir, "git")
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
	cidr, err := netip.ParsePrefix(cfg.ClusterCIDR)
	if err != nil || !cidr.Addr().Is4() || cidr.Bits() != 16 {
		return fmt.Errorf("cluster CIDR must be an IPv4 /16, got %q", cfg.ClusterCIDR)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
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

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
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
	err = st.RegisterControlNode(ctx, store.Node{
		Name: nodeName, Address: outboundIP(), Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(), MemoryMB: totalMemoryMB(),
	})
	if err != nil {
		return fmt.Errorf("registering node: %w", err)
	}

	srv := api.New(api.Config{
		Store:          st,
		Git:            &gitrepo.Manager{Dir: cfg.GitDir, Exe: exe, Owner: owner},
		Deployer:       &deploy.Pipeline{Store: st, Log: log},
		Log:            log,
		DataDir:        cfg.DataDir,
		Exe:            exe,
		AuthorizedKeys: cfg.AuthorizedKeys,
		Owner:          owner,
		ClusterCIDR:    cidr,
	})
	if err := srv.SyncAuthorizedKeys(ctx); err != nil {
		return fmt.Errorf("writing %s: %w", cfg.AuthorizedKeys, err)
	}

	ln, err := listenSocket(cfg.Socket, owner)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}

	go heartbeat(ctx, st, nodeName, log)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("jokku daemon started", "version", version.Version, "node", nodeName, "socket", cfg.Socket,
		"data_dir", cfg.DataDir, "authorized_keys", cfg.AuthorizedKeys)
	if err := httpSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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

// heartbeat keeps the control node marked as up. Workers report through the
// agent API instead (milestone 2).
func heartbeat(ctx context.Context, st *store.Store, node string, log *slog.Logger) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.TouchNode(ctx, node); err != nil && ctx.Err() == nil {
				log.Warn("heartbeat", "err", err)
			}
		}
	}
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
