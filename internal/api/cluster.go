package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// Admin endpoints (unix socket)

func (s *Server) createJoinToken(w http.ResponseWriter, r *http.Request) {
	var req types.CreateJoinTokenRequest
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	tok, err := s.Cluster.CreateJoinToken(r.Context(), time.Duration(req.TTLSeconds)*time.Second, req.Reusable)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, tok)
}

func (s *Server) clusterStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.Cluster.Status(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	events, err := s.Store.Events(r.Context(), r.URL.Query().Get("app"), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// Public endpoints (TLS on :7443)

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var req types.JoinRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.Cluster.Join(r.Context(), req)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrExists) {
			s.fail(w, r, err)
			return
		}
		s.fail(w, r, badRequest("%v", err))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// agentAuth admits requests carrying a node's token and passes the node on.
func (s *Server) agentAuth(next func(http.ResponseWriter, *http.Request, *store.Node)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		node, err := s.Cluster.Authenticate(r.Context(), token)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, types.Error{Error: err.Error()})
			return
		}
		next(w, r, node)
	}
}

func (s *Server) agentState(w http.ResponseWriter, r *http.Request, n *store.Node) {
	st, err := s.Cluster.State(r.Context(), n.Name, r.URL.Query().Get("etag"))
	if err != nil {
		if r.Context().Err() != nil {
			return // the agent hung up
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) agentStatus(w http.ResponseWriter, r *http.Request, n *store.Node) {
	var st types.NodeStatus
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&st); err != nil {
		s.fail(w, r, badRequest("invalid status: %v", err))
		return
	}
	if err := s.Cluster.Report(r.Context(), n.Name, &st); err != nil {
		s.fail(w, r, badRequest("%v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentArtifact streams a root filesystem to a node that needs it.
func (s *Server) agentArtifact(w http.ResponseWriter, r *http.Request, _ *store.Node) {
	name := filepath.Base(r.PathValue("name"))
	if !strings.HasSuffix(name, ".ext4") {
		s.fail(w, r, httpErrorf(http.StatusNotFound, "no such artifact"))
		return
	}
	f, err := os.Open(filepath.Join(s.DataDir, "artifacts", name))
	if err != nil {
		s.fail(w, r, httpErrorf(http.StatusNotFound, "no such artifact"))
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, f)
}

// The shared certificate store behind every ingress node's Caddy.

func (s *Server) certGet(w http.ResponseWriter, r *http.Request, _ *store.Node) {
	v, _, err := s.Store.CertGet(r.Context(), r.URL.Query().Get("key"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(v)
}

func (s *Server) certPut(w http.ResponseWriter, r *http.Request, _ *store.Node) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		s.fail(w, r, badRequest("%v", err))
		return
	}
	if err := s.Store.CertPut(r.Context(), r.URL.Query().Get("key"), b); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) certDelete(w http.ResponseWriter, r *http.Request, _ *store.Node) {
	if err := s.Store.CertDelete(r.Context(), r.URL.Query().Get("key")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) certList(w http.ResponseWriter, r *http.Request, _ *store.Node) {
	keys, err := s.Store.CertList(r.Context(), r.URL.Query().Get("prefix"), r.URL.Query().Get("recursive") == "true")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if keys == nil {
		keys = []string{}
	}
	writeJSON(w, http.StatusOK, keys)
}

// certStat answers in certmagic's KeyInfo shape. A key that only exists as
// a prefix of others is a "directory".
func (s *Server) certStat(w http.ResponseWriter, r *http.Request, _ *store.Node) {
	ctx := r.Context()
	key := r.URL.Query().Get("key")
	v, modified, err := s.Store.CertGet(ctx, key)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"Key": key, "Modified": modified, "Size": len(v), "IsTerminal": true})
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	if children, err := s.Store.CertList(ctx, key, false); err == nil && len(children) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"Key": key, "IsTerminal": false})
		return
	}
	s.fail(w, r, store.ErrNotFound)
}

func (s *Server) certLock(w http.ResponseWriter, r *http.Request, _ *store.Node) {
	q := r.URL.Query()
	// Locks expire so a node that dies while holding one can't block the
	// cluster's certificate work for long.
	ok, err := s.Store.CertLock(r.Context(), q.Get("key"), q.Get("owner"), 2*time.Minute)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) certUnlock(w http.ResponseWriter, r *http.Request, _ *store.Node) {
	q := r.URL.Query()
	if err := s.Store.CertUnlock(r.Context(), q.Get("key"), q.Get("owner")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
