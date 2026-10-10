package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// createEdge adds an edge and returns the command that installs it, which
// carries its credentials: shown once, never stored.
func (s *Server) createEdge(w http.ResponseWriter, r *http.Request) {
	var req types.CreateEdgeRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Address == "" {
		s.fail(w, r, badRequest("Give the edge's public address (an IP or a DNS name)"))
		return
	}
	edge, err := s.Cluster.CreateEdge(r.Context(), req.Name, req.Address)
	if err != nil {
		if errors.Is(err, store.ErrExists) {
			s.fail(w, r, err)
			return
		}
		s.fail(w, r, badRequest("%v", err))
		return
	}
	writeJSON(w, http.StatusCreated, edge)
}

func (s *Server) listEdges(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.Store.Nodes(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := time.Now()
	out := []types.Edge{}
	for _, n := range nodes {
		if n.Edge() && !n.Removed() {
			out = append(out, types.Edge{Node: s.Cluster.NodeInfo(n, now), Version: n.Version})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// removeEdge takes an edge out of the cluster: the nodes behind it stop
// dialing it, and its proxy stops routing.
func (s *Server) removeEdge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	n, err := s.Store.Node(ctx, name)
	if err == nil && n.Removed() {
		err = &store.NotFoundError{What: "Edge " + name}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !n.Edge() {
		s.fail(w, r, badRequest("%s is not an edge; remove nodes with jokku nodes:remove", name))
		return
	}
	if err := s.Store.RemoveEdge(ctx, name); err != nil {
		s.fail(w, r, err)
		return
	}
	// It held the key that signs logins: a new one ends every session it
	// could otherwise forge.
	if !n.LastSeen.IsZero() {
		if err := s.Cluster.RotateAuthKey(ctx); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.Store.AddEvent(ctx, "node", "", name, "edge %s was removed from the cluster", name)
	s.Cluster.Changed()
	w.WriteHeader(http.StatusNoContent)
}
