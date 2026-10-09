package agent

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/wes/jokku/internal/journal"
)

// Handler is the agent's API. It serves the control node, which presents
// token: this node's app logs, sessions with its VMs (jokku enter, volume
// copies) and volume backups. It also serves volume disks to the node taking one over,
// which presents that move's token. With no token, only the latter.
func (a *Agent) Handler(token string) http.Handler {
	mux := http.NewServeMux()
	authorized := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	if token != "" {
		for _, path := range []string{"/v1/logs", "/v1/instance-logs", "/v1/requests"} {
			mux.HandleFunc("GET "+path, authorized(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				rc := http.NewResponseController(w)
				journal.Serve(r.Context(), path, r.URL.Query(), func(line string) {
					w.Write([]byte(line + "\n"))
					rc.Flush()
				})
			}))
		}
		mux.HandleFunc("POST /v1/instances/{id}/session", authorized(a.serveSession))
		mux.HandleFunc("POST /v1/volumes/{id}/backup", authorized(a.serveBackup))
	}
	mux.HandleFunc("POST /v1/volumes/{id}/copy", a.serveVolume)
	return mux
}

// Serve runs the agent's API on addr (this node's mesh address) until ctx is
// done. The firewall keeps VMs away from the port as well.
func (a *Agent) Serve(ctx context.Context, addr, token string) {
	srv := &http.Server{Handler: a.Handler(token), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	// The mesh address appears when the network is set up; keep trying.
	for ctx.Err() == nil {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			sleep(ctx, 5*time.Second)
			continue
		}
		a.Log.Info("agent API listening", "addr", addr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.Log.Error("agent API", "err", err)
			sleep(ctx, 5*time.Second)
		}
	}
}
