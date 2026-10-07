package api

import (
	"net/http"

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
	if err := s.Store.Scale(r.Context(), app, req.Quantities); err != nil {
		s.fail(w, r, err)
		return
	}
	if !req.SkipDeploy {
		if _, err := s.Deployer.Restart(r.Context(), app, actor(r)); err != nil {
			s.fail(w, r, err)
			return
		}
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
	if _, err := s.Deployer.Restart(r.Context(), app, actor(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	s.getResources(w, r)
}
