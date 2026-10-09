package api

import (
	"context"
	"net/http"

	"github.com/wes/jokku/internal/props"
	"github.com/wes/jokku/internal/types"
)

func (s *Server) getProperties(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app, plugin := r.PathValue("app"), r.PathValue("plugin")
	p, ok := props.Lookup(plugin)
	if !ok {
		s.fail(w, r, httpErrorf(http.StatusNotFound, "Unknown plugin %q", plugin))
		return
	}
	global, err := s.Store.Properties(ctx, "", plugin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res := types.Properties{Plugin: plugin, Global: global}
	if app != "" {
		if res.App, err = s.Store.Properties(ctx, app, plugin); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	res.Computed = props.Compute(p, res.App, res.Global)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) setProperty(w http.ResponseWriter, r *http.Request) {
	app, plugin, key := r.PathValue("app"), r.PathValue("plugin"), r.PathValue("key")
	var req types.SetPropertyRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := props.Check(plugin, key, req.Value); err != nil {
		s.fail(w, r, badRequest("%v", err))
		return
	}
	if err := s.Store.SetProperty(r.Context(), app, plugin, key, req.Value); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Deployer.RoutesChanged()
	s.getProperties(w, r)
}

// computedProperties resolves one plugin's settings for an app.
func (s *Server) computedProperties(ctx context.Context, app, plugin string) (map[string]string, error) {
	p, _ := props.Lookup(plugin)
	global, err := s.Store.Properties(ctx, "", plugin)
	if err != nil {
		return nil, err
	}
	appProps, err := s.Store.Properties(ctx, app, plugin)
	if err != nil {
		return nil, err
	}
	return props.Compute(p, appProps, global), nil
}

// setBuilder switches an app to Dockerfile or compose builds, from its next
// push. An app becomes an image app by deploying one (builder:image).
func (s *Server) setBuilder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app := r.PathValue("app")
	var req types.BuilderRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Type != "dockerfile" && req.Type != "compose" {
		s.fail(w, r, badRequest("the builder is dockerfile or compose (to deploy an image: jokku builder:image %s <image>)", app))
		return
	}
	if _, err := s.Store.App(ctx, app); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetProperties(ctx, app, "builder", map[string]string{"type": req.Type, "file": req.File, "image": ""}); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "app", app, "", "%s switched the builder to %s", actor(r), req.Type)
	w.WriteHeader(http.StatusNoContent)
}
