package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/types"
)

// External apps are services Jokku routes a domain to but doesn't run,
// such as Home Assistant on the LAN. Each is an app of kind external, so
// domains:*, letsencrypt:*, proxy:* and http-auth:* work on it as on any
// app; the target is stored as properties (see cluster.ExternalPlugin).

func (s *Server) createExternal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req types.ExternalRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := validAppName(req.Name); err != nil {
		s.fail(w, r, err)
		return
	}
	values, err := s.externalValues(ctx, req, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.Store.CreateAppOfKind(ctx, req.Name, types.KindExternal); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetProperties(ctx, req.Name, cluster.ExternalPlugin, values); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.addDefaultDomains(ctx, req.Name); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "deploy", req.Name, "", "external app %s routes to %s", req.Name, values["url"])
	s.Cluster.Changed()
	s.writeExternal(w, r, req.Name, http.StatusCreated)
}

// externalValues checks a create or change request and returns the
// properties to store. current is the app's properties, nil for a new one.
func (s *Server) externalValues(ctx context.Context, req types.ExternalRequest, current map[string]string) (map[string]string, error) {
	values := map[string]string{}
	if req.URL != "" || current == nil {
		if _, _, err := cluster.ParseTarget(req.URL, s.Cluster.ClusterCIDR); err != nil {
			return nil, badRequest("%v", err)
		}
		values["url"] = req.URL
	}
	if req.Via != "" {
		n, err := s.Store.Node(ctx, req.Via)
		if err != nil {
			return nil, err
		}
		if n.Edge() {
			return nil, badRequest("%s is an edge; external apps are reached through a node on their network", req.Via)
		}
		values["via"] = req.Via
	}
	if req.Insecure != nil {
		values["insecure"] = ""
		if *req.Insecure {
			values["insecure"] = "true"
		}
	}
	return values, s.sameAddressSameVia(ctx, req.Name, values, current)
}

// sameAddressSameVia refuses two external apps at one address through
// different nodes: an edge sends an address through one node only.
func (s *Server) sameAddressSameVia(ctx context.Context, name string, values, current map[string]string) error {
	pick := func(k string) string {
		if v, ok := values[k]; ok {
			return v
		}
		return current[k]
	}
	via := func(v string) string {
		if v == "" {
			return s.Cluster.Self
		}
		return v
	}
	_, target, err := cluster.ParseTarget(pick("url"), s.Cluster.ClusterCIDR)
	if err != nil {
		return nil
	}
	apps, err := s.Store.Apps(ctx)
	if err != nil {
		return err
	}
	for _, a := range apps {
		if a.Kind != types.KindExternal || a.Name == name {
			continue
		}
		p, err := s.Store.Properties(ctx, a.Name, cluster.ExternalPlugin)
		if err != nil {
			return err
		}
		_, other, err := cluster.ParseTarget(p["url"], s.Cluster.ClusterCIDR)
		if err == nil && other.Addr() == target.Addr() && via(p["via"]) != via(pick("via")) {
			return badRequest("%s already reaches %s through %s; external apps at one address go through the same node",
				a.Name, target.Addr(), via(p["via"]))
		}
	}
	return nil
}

func (s *Server) patchExternal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	if err := s.isExternal(ctx, name); err != nil {
		s.fail(w, r, err)
		return
	}
	var req types.ExternalRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	current, err := s.Store.Properties(ctx, name, cluster.ExternalPlugin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	req.Name = name
	values, err := s.externalValues(ctx, req, current)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetProperties(ctx, name, cluster.ExternalPlugin, values); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Cluster.Changed()
	s.writeExternal(w, r, name, http.StatusOK)
}

func (s *Server) getExternal(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.isExternal(r.Context(), name); err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeExternal(w, r, name, http.StatusOK)
}

func (s *Server) listExternals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	apps, err := s.Store.Apps(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []types.External{}
	for _, a := range apps {
		if a.Kind != types.KindExternal {
			continue
		}
		e, err := s.external(ctx, a)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, *e)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) destroyExternal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	if err := s.isExternal(ctx, name); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.DeleteApp(ctx, name); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Cluster.Changed()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) isExternal(ctx context.Context, name string) error {
	app, err := s.Store.App(ctx, name)
	if err != nil {
		return err
	}
	if app.Kind != types.KindExternal {
		return httpErrorf(http.StatusNotFound, "%s is not an external app (see jokku external:list)", name)
	}
	return nil
}

func (s *Server) writeExternal(w http.ResponseWriter, r *http.Request, name string, status int) {
	app, err := s.Store.App(r.Context(), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	e, err := s.external(r.Context(), *app)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, status, e)
}

func (s *Server) external(ctx context.Context, app types.App) (*types.External, error) {
	p, err := s.Store.Properties(ctx, app.Name, cluster.ExternalPlugin)
	if err != nil {
		return nil, err
	}
	domains, err := s.Store.Domains(ctx, app.Name)
	if err != nil {
		return nil, err
	}
	auth, err := s.Store.Properties(ctx, app.Name, cluster.AuthPlugin)
	if err != nil {
		return nil, err
	}
	via := p["via"]
	if via == "" {
		via = s.Cluster.Self
	}
	insecure, _ := strconv.ParseBool(p["insecure"])
	e := &types.External{Name: app.Name, URL: p["url"], Via: via, Insecure: insecure, Domains: domains, CreatedAt: app.CreatedAt}
	if m := auth["mode"]; m == types.AuthPassword || m == types.AuthUsers {
		e.Auth = m
	}
	if e.Domains == nil {
		e.Domains = []string{}
	}
	return e, nil
}

// runsApp refuses what only apps Jokku runs have (deploys, processes,
// config, volumes, shells) for an external app, which is only routed to.
func (s *Server) runsApp(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if name := r.PathValue("app"); name != "" {
			if app, err := s.Store.App(r.Context(), name); err == nil && app.Kind == types.KindExternal {
				s.fail(w, r, httpErrorf(http.StatusConflict,
					"%s is an external app: Jokku routes its domains to it but doesn't run it. Manage it with jokku external:*", name))
				return
			}
		}
		next(w, r)
	}
}
