package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.Store.Nodes(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := time.Now()
	out := make([]types.Node, 0, len(nodes))
	for _, n := range nodes {
		if !n.Removed() {
			out = append(out, s.Cluster.NodeInfo(n, now))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.Node(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Cluster.NodeInfo(*n, time.Now()))
}

type nodePatch struct {
	Schedulable *bool `json:"schedulable,omitempty"`
	Ingress     *bool `json:"ingress,omitempty"`
	Draining    *bool `json:"draining,omitempty"`
}

func (s *Server) patchNode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	var p nodePatch
	if err := decode(r, &p); err != nil {
		s.fail(w, r, err)
		return
	}
	if n, err := s.Store.Node(ctx, name); err == nil && n.Edge() && ((p.Schedulable != nil && *p.Schedulable) || (p.Draining != nil && *p.Draining)) {
		s.fail(w, r, badRequest("%s is an edge: it only routes traffic, and runs no apps", name))
		return
	}
	for flag, v := range map[string]*bool{"schedulable": p.Schedulable, "ingress": p.Ingress, "draining": p.Draining} {
		if v == nil {
			continue
		}
		if err := s.Store.SetNodeFlag(ctx, name, flag, *v); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	switch {
	case p.Draining != nil && *p.Draining:
		s.Store.AddEvent(ctx, "node", "", name, "draining %s: moving its instances to other nodes", name)
	case p.Draining != nil:
		s.Store.AddEvent(ctx, "node", "", name, "%s accepts instances again", name)
	}
	s.Cluster.Changed()
	s.getNode(w, r)
}

// removeNode takes a worker out of the cluster. Its instances must have
// been moved off first (nodes:drain) unless force is set, in which case they
// are rescheduled like a dead node's.
func (s *Server) removeNode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	n, err := s.Store.Node(ctx, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if n.Role == store.RoleControl {
		s.fail(w, r, badRequest("The control node cannot be removed"))
		return
	}
	if n.Edge() {
		s.removeEdge(w, r)
		return
	}
	if r.URL.Query().Get("force") != "true" {
		insts, err := s.Store.Instances(ctx, "")
		if err != nil {
			s.fail(w, r, err)
			return
		}
		running := 0
		for _, in := range insts {
			if in.Node == name && in.Desired == store.DesiredRunning {
				running++
			}
		}
		if running > 0 {
			s.fail(w, r, httpErrorf(http.StatusConflict,
				"%s still runs %d instances. Move them first with: jokku nodes:drain %s (or remove it anyway with --force)", name, running, name))
			return
		}
		vols, err := s.nodeVolumes(ctx, name)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if len(vols) > 0 {
			s.fail(w, r, httpErrorf(http.StatusConflict,
				"%s holds the disks of volumes %s. Move them first with: jokku nodes:drain %s (or remove it anyway with --force, losing them)",
				name, strings.Join(vols, ", "), name))
			return
		}
	}
	if err := s.Store.DeleteNode(ctx, name); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "node", "", name, "%s was removed from the cluster", name)
	s.Cluster.Changed()
	w.WriteHeader(http.StatusNoContent)
}
