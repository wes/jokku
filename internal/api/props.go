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
