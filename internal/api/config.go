package api

import (
	"net/http"

	"github.com/wes/jokku/internal/types"
)

// Config and domains handlers serve both /v1/apps/{app}/... and the global
// /v1/... routes; an empty app path value means global.

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	vars, err := s.Store.ConfigVars(r.Context(), r.PathValue("app"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, types.ConfigVars{Vars: vars})
}

func (s *Server) patchConfig(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var p types.ConfigPatch
	if err := decode(r, &p); err != nil {
		s.fail(w, r, err)
		return
	}
	for k := range p.Set {
		if err := validConfigKey(k); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	vars, changed, err := s.Store.UpdateConfigVars(r.Context(), app, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res := types.ConfigVars{Vars: vars}
	if changed && !p.NoRestart && app != "" {
		res.Restarting, err = s.Deployer.Restart(r.Context(), app, actor(r))
		if err != nil {
			s.fail(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) getDomains(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app := r.PathValue("app")
	global, err := s.Store.Domains(ctx, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res := types.Domains{Global: global, App: []string{}, Vhosts: []string{}}
	if app != "" {
		if res.App, err = s.Store.Domains(ctx, app); err != nil {
			s.fail(w, r, err)
			return
		}
		proxy, err := s.computedProperties(ctx, app, "proxy")
		if err != nil {
			s.fail(w, r, err)
			return
		}
		res.Enabled = proxy["enabled"] == "true"
		if res.Enabled {
			res.Vhosts = res.App
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) patchDomains(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var p types.DomainsPatch
	if err := decode(r, &p); err != nil {
		s.fail(w, r, err)
		return
	}
	var err error
	for _, list := range []*[]string{&p.Add, &p.Remove, &p.Set} {
		if *list == nil {
			continue
		}
		if *list, err = normalizeDomains(*list); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if _, err := s.Store.UpdateDomains(r.Context(), app, p); err != nil {
		s.fail(w, r, err)
		return
	}
	s.getDomains(w, r)
}
