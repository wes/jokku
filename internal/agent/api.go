package agent

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/wes/jokku/internal/journal"
)

// Serve runs the agent's API on addr (this node's mesh address) until ctx is
// done. It serves this node's app logs to the control node, which presents
// the agent token. The firewall keeps VMs away from the port as well.
func Serve(ctx context.Context, addr, token string, log *slog.Logger) {
	mux := http.NewServeMux()
	for _, path := range []string{"/v1/logs", "/v1/instance-logs"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			rc := http.NewResponseController(w)
			journal.Serve(r.Context(), path, r.URL.Query(), func(line string) {
				w.Write([]byte(line + "\n"))
				rc.Flush()
			})
		})
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
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
		log.Info("agent API listening", "addr", addr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("agent API", "err", err)
			sleep(ctx, 5*time.Second)
		}
	}
}
