package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/wes/jokku/internal/types"
)

func (s *Server) getFormation(w http.ResponseWriter, r *http.Request) {
	procs, err := s.Store.Formation(r.Context(), r.PathValue("app"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, types.Formation{Processes: procs})
}

func (s *Server) scale(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var req types.ScaleRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if len(req.Quantities) == 0 {
		s.fail(w, r, badRequest("No process types to scale"))
		return
	}
	for proc, n := range req.Quantities {
		if err := validProcessType(proc); err != nil {
			s.fail(w, r, err)
			return
		}
		if n < 0 || n > 1000 {
			s.fail(w, r, badRequest("Invalid quantity %d for %s", n, proc))
			return
		}
	}
	if err := s.scaleWithVolumes(r.Context(), app, req.Quantities); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.Scale(r.Context(), app, req.Quantities); err != nil {
		s.fail(w, r, err)
		return
	}
	s.getFormation(w, r)
}

func (s *Server) getResources(w http.ResponseWriter, r *http.Request) {
	res, err := s.Store.Resources(r.Context(), r.PathValue("app"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, types.Resources{Default: res.Default, Process: res.Process})
}

func (s *Server) patchResources(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var l types.ResourceLimits
	if err := decode(r, &l); err != nil {
		s.fail(w, r, err)
		return
	}
	if l.ProcessType != "" {
		if err := validProcessType(l.ProcessType); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if l.CPUs != nil && (*l.CPUs < 0 || *l.CPUs > 32) {
		s.fail(w, r, badRequest("CPUs must be between 1 and 32"))
		return
	}
	if l.MemoryMB != nil && *l.MemoryMB != 0 && (*l.MemoryMB < 128 || *l.MemoryMB > 256*1024) {
		s.fail(w, r, badRequest("Memory must be between 128MB and 256GB"))
		return
	}
	if err := s.Store.SetResources(r.Context(), app, l); err != nil {
		s.fail(w, r, err)
		return
	}
	s.getResources(w, r)
}

// psAction streams restart, start, stop or rebuild.
func (s *Server) psAction(w http.ResponseWriter, r *http.Request) {
	app, action := r.PathValue("app"), r.PathValue("action")
	switch action {
	case "restart", "start", "stop", "rebuild":
	default:
		s.fail(w, r, httpErrorf(http.StatusNotFound, "Unknown ps action %q", action))
		return
	}
	if _, err := s.Store.App(r.Context(), app); err != nil {
		s.fail(w, r, err)
		return
	}
	st := newStream(w)
	// Like deploys, an action outlives a dropped connection.
	st.Done(s.Deployer.PS(context.WithoutCancel(r.Context()), app, action, actor(r), st.Log))
}

func (s *Server) listInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := s.Store.Instances(r.Context(), r.PathValue("app"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []types.Instance{}
	for _, in := range insts {
		out = append(out, types.Instance{
			ID: in.ID, Name: in.Name(), Release: in.Release, Node: in.Node, IP: in.IP, Port: in.Port,
			CPUs: in.CPUs, MemoryMB: in.MemoryMB, State: in.State, Desired: in.Desired,
			Restarts: in.Restarts, StartedAt: in.StartedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listReleases(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	cur, err := s.Store.CurrentRelease(r.Context(), app)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rels, err := s.Store.Releases(r.Context(), app, 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []types.Release{}
	for _, rel := range rels {
		out = append(out, types.Release{
			Version: rel.Version, Description: rel.Description, CreatedAt: rel.CreatedAt,
			Current: cur != nil && cur.ID == rel.ID,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// logs streams app log lines. Query: tail (default 100), follow, process.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	if _, err := s.Store.App(r.Context(), app); err != nil {
		s.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	o := types.LogOptions{Tail: 100, Follow: q.Get("follow") == "true", Process: q.Get("process")}
	if n, err := strconv.Atoi(q.Get("tail")); err == nil && n >= 0 {
		o.Tail = n
	}
	st := newStream(w)
	st.Done(s.Deployer.Logs(r.Context(), app, o, st.Log))
}
