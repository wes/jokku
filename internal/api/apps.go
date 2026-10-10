package api

import (
	"context"
	"net/http"

	"github.com/wes/jokku/internal/types"
)

// listApps lists the apps; databases too with ?all=true (db:list lists
// them on their own).
func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	all, err := s.Store.Apps(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apps := []types.App{}
	for _, a := range all {
		if a.Kind == "" || r.URL.Query().Get("all") == "true" {
			apps = append(apps, a)
		}
	}
	writeJSON(w, http.StatusOK, apps)
}

// notDatabase refuses what would break a database's app (renaming it,
// cloning it): it is managed with db:<engine>:*. An external app is
// managed with external:*.
func (s *Server) notDatabase(ctx context.Context, name string) error {
	app, err := s.Store.App(ctx, name)
	if err != nil {
		return err
	}
	switch app.Kind {
	case "":
	case types.KindExternal:
		return httpErrorf(http.StatusConflict, "%s is an external app; manage it with jokku external:*", name)
	default:
		return httpErrorf(http.StatusConflict, "%s is a %s database; manage it with jokku db:%s:*", name, app.Kind, app.Kind)
	}
	return nil
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.Store.App(r.Context(), r.PathValue("app"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	var req types.CreateAppRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := validAppName(req.Name); err != nil {
		s.fail(w, r, err)
		return
	}
	app, err := s.Store.CreateApp(r.Context(), req.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.addDefaultDomains(r.Context(), app.Name); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, app)
}

// addDefaultDomains gives an app <app>.<global> for each global domain, as
// Dokku does at creation. They are ordinary app domains afterwards.
func (s *Server) addDefaultDomains(ctx context.Context, app string) error {
	global, err := s.Store.Domains(ctx, "")
	if err != nil || len(global) == 0 {
		return err
	}
	var add []string
	for _, g := range global {
		add = append(add, app+"."+g)
	}
	_, err = s.Store.UpdateDomains(ctx, app, types.DomainsPatch{Add: add})
	return err
}

func (s *Server) destroyApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	if err := s.Store.DeleteApp(r.Context(), name); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Git.Remove(name); err != nil {
		s.Log.Warn("removing git repo", "app", name, "err", err)
	}
	s.Cluster.Changed() // stop its instances and delete its volumes' disks now
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) renameApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("app")
	if err := s.notDatabase(ctx, name); err != nil {
		s.fail(w, r, err)
		return
	}
	var req types.RenameAppRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := validAppName(req.NewName); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.RenameApp(ctx, name, req.NewName); err != nil {
		s.fail(w, r, err)
		return
	}
	// Carry default domains over: old.example.com becomes new.example.com.
	global, err := s.Store.Domains(ctx, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	patch := types.DomainsPatch{}
	for _, g := range global {
		patch.Remove = append(patch.Remove, name+"."+g)
		patch.Add = append(patch.Add, req.NewName+"."+g)
	}
	if _, err := s.Store.UpdateDomains(ctx, req.NewName, patch); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Git.Rename(name, req.NewName); err != nil {
		s.fail(w, r, err)
		return
	}
	s.getAppNamed(w, r, req.NewName)
}

func (s *Server) cloneApp(w http.ResponseWriter, r *http.Request) {
	if err := s.notDatabase(r.Context(), r.PathValue("app")); err != nil {
		s.fail(w, r, err)
		return
	}
	var req types.CloneAppRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := validAppName(req.NewName); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.CloneApp(r.Context(), r.PathValue("app"), req.NewName); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.addDefaultDomains(r.Context(), req.NewName); err != nil {
		s.fail(w, r, err)
		return
	}
	s.getAppNamed(w, r, req.NewName)
}

func (s *Server) lockApp(locked bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("app")
		if err := s.Store.SetAppLocked(r.Context(), name, locked); err != nil {
			s.fail(w, r, err)
			return
		}
		s.getAppNamed(w, r, name)
	}
}

func (s *Server) getAppNamed(w http.ResponseWriter, r *http.Request, name string) {
	app, err := s.Store.App(r.Context(), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) ensureGitRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	if _, err := s.Store.App(r.Context(), name); err != nil {
		s.fail(w, r, err)
		return
	}
	path, err := s.Git.Ensure(name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, types.GitRepo{Path: path})
}
