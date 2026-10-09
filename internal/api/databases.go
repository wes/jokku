package api

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	imageref "github.com/google/go-containerregistry/pkg/name"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/database"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// Databases (db:<engine>:*) are apps of a kind, deployed from a compose
// file the database package writes. See package database.

// SourceDatabase is the deploy source of a database's compose file.
const SourceDatabase = "database"

// lookupDatabase finds an engine and the app of one of its databases.
func (s *Server) lookupDatabase(ctx context.Context, engine, name string) (database.Engine, *types.App, error) {
	e, ok := database.Lookup(engine)
	if !ok {
		return e, nil, httpErrorf(http.StatusNotFound, "Jokku runs %s databases, not %s", strings.Join(database.Names(), ", "), engine)
	}
	app, err := s.Store.App(ctx, e.AppName(name))
	if errors.Is(err, store.ErrNotFound) || (err == nil && app.Kind != e.Name) {
		return e, nil, httpErrorf(http.StatusNotFound, "There is no %s database %s (jokku db:%s:list)", engine, name, engine)
	}
	return e, app, err
}

func (s *Server) createDatabase(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e, ok := database.Lookup(r.PathValue("engine"))
	if !ok {
		s.fail(w, r, httpErrorf(http.StatusNotFound, "Jokku runs %s databases, not %s", strings.Join(database.Names(), ", "), r.PathValue("engine")))
		return
	}
	var req types.CreateDatabaseRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := database.CheckName(req.Name); err != nil {
		s.fail(w, r, badRequest("%v", err))
		return
	}
	image := e.ImageRef(req.Image, req.ImageVersion)
	if _, err := imageref.ParseReference(image); err != nil || strings.ContainsAny(image, " \t\r\n${}") {
		s.fail(w, r, badRequest("%q is not an image, like %s or %s", image, e.ImageRef("", ""), e.ImageRef("", "16")))
		return
	}
	app := e.AppName(req.Name)
	if _, err := s.Store.CreateAppOfKind(ctx, app, e.Name); err != nil {
		if errors.Is(err, store.ErrExists) {
			err = httpErrorf(http.StatusConflict, "%s already exists: a %s database %s, or an app with that name", app, e.Name, req.Name)
		}
		s.fail(w, r, err)
		return
	}
	setup := func() error {
		if err := s.Store.SetProperties(ctx, app, "builder", map[string]string{"type": "compose"}); err != nil {
			return err
		}
		if err := s.Store.SetProperty(ctx, app, "proxy", "enabled", "false"); err != nil {
			return err
		}
		if _, _, err := s.Store.UpdateConfigVars(ctx, app, types.ConfigPatch{Set: e.Vars(req.Name, image)}); err != nil {
			return err
		}
		if req.SizeMB > 0 {
			v := &store.Volume{App: app, Name: "data", Type: types.VolumeLocal, SizeMB: req.SizeMB}
			if err := s.Store.CreateVolume(ctx, v); err != nil {
				return err
			}
		}
		return nil
	}
	if err := setup(); err != nil {
		s.Store.DeleteApp(context.WithoutCancel(ctx), app)
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "app", app, "", "%s created %s database %s (%s)", actor(r), e.Name, req.Name, image)

	st := newStream(w)
	st.Log(fmt.Sprintf("-----> Creating %s database %s (%s)", e.Name, req.Name, image))
	err := s.deployDatabase(context.WithoutCancel(ctx), e, app, image, e.Compose(req.MemoryMB), actor(r), st)
	if err != nil {
		st.Done(fmt.Errorf("%w (remove it with jokku db:%s:destroy %s, or try again with jokku ps:rebuild %s)", err, e.Name, req.Name, app))
		return
	}
	s.autoBackup(context.WithoutCancel(ctx), app, st)
	st.Log(fmt.Sprintf("=====> %s database %s is running at %s:%d", e.Name, req.Name, e.Host(req.Name), e.Port))
	st.Log(fmt.Sprintf("       Link it to an app with: jokku db:%s:link %s <app>", e.Name, req.Name))
	st.Done(nil)
}

// deployDatabase deploys a database's compose file to its app.
func (s *Server) deployDatabase(ctx context.Context, e database.Engine, app, image, compose, who string, st *stream) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "compose.yaml", Mode: 0o644, Size: int64(len(compose)), Typeflag: tar.TypeReg})
	tw.Write([]byte(compose))
	tw.Close()
	d, err := s.Store.CreateDeploy(ctx, app, SourceDatabase, image, who)
	if err != nil {
		return err
	}
	path, _, err := s.spoolSource(d.ID, &buf)
	if err == nil {
		err = s.Deployer.Deploy(ctx, d, path, st.Log)
	}
	status, msg := types.StatusSucceeded, ""
	if err != nil {
		status, msg = types.StatusFailed, err.Error()
	}
	s.Store.SetDeployStatus(ctx, d.ID, status, msg)
	return err
}

// autoBackup backs a new database's volume up where the cluster is backed
// up (or to the first destination), so it is safe from the start.
func (s *Server) autoBackup(ctx context.Context, app string, st *stream) {
	dests, err := s.Store.BackupDestinations(ctx)
	if err != nil || len(dests) == 0 {
		st.Log("       It isn't backed up: add a destination (jokku backups:destination-add) and run jokku backups:set " + app + " data <destination>")
		return
	}
	dest := dests[0]
	if set, err := s.Store.ClusterBackup(ctx); err == nil {
		for _, d := range dests {
			if d.Name == set.Destination {
				dest = d
			}
		}
	}
	if dest.Encrypt {
		if _, saved, err := s.Store.BackupKey(ctx); err != nil || !saved {
			st.Log("       It isn't backed up yet: save the backup key (jokku backups:key), then run jokku backups:set " + app + " data " + dest.Name)
			return
		}
	}
	v, err := s.Store.Volume(ctx, app, "data")
	if err != nil {
		return
	}
	set := store.VolumeBackup{VolumeID: v.ID, Destination: dest.Name, Path: "jokku/" + app + "/data", Every: store.DefaultBackupEvery,
		KeepRecent: store.DefaultKeepRecent, KeepDaily: store.DefaultKeepDaily, AutoRestore: true}
	if err := s.Store.SetVolumeBackup(ctx, set); err != nil {
		st.Log("       Backing it up failed to start: " + err.Error())
		return
	}
	st.Log(fmt.Sprintf("-----> Backing it up to %s every 15 minutes, starting now", dest.Name))
}

func (s *Server) getDatabase(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e, app, err := s.lookupDatabase(ctx, r.PathValue("engine"), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	db, err := s.databaseInfo(ctx, e, app, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, db)
}

func (s *Server) listDatabases(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	apps, err := s.Store.Apps(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []types.Database{}
	for _, a := range apps {
		e, ok := database.Lookup(a.Kind)
		if !ok || (r.URL.Query().Get("engine") != "" && a.Kind != r.URL.Query().Get("engine")) {
			continue
		}
		db, err := s.databaseInfo(ctx, e, &a, false)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, *db)
	}
	writeJSON(w, http.StatusOK, out)
}

// databaseInfo describes a database; withVars adds its config vars.
func (s *Server) databaseInfo(ctx context.Context, e database.Engine, app *types.App, withVars bool) (*types.Database, error) {
	name := strings.TrimPrefix(app.Name, e.Name+"-")
	vars, err := s.Store.ConfigVars(ctx, app.Name)
	if err != nil {
		return nil, err
	}
	db := &types.Database{
		Engine: e.Name, Name: name, App: app.Name, Image: vars[database.VarImage], CreatedAt: app.CreatedAt,
		Host: fmt.Sprintf("%s:%d", e.Host(name), e.Port), URL: e.URL(name, vars), Links: []types.DatabaseLink{}, Status: "none",
	}
	if withVars {
		db.Vars = vars
	}
	insts, err := s.Store.Instances(ctx, app.Name)
	if err != nil {
		return nil, err
	}
	for _, in := range insts {
		if in.Desired != store.DesiredRunning || in.ProcessType != e.Process() {
			continue
		}
		db.Node, db.MemoryMB = in.Node, in.MemoryMB
		switch in.State {
		case store.StateHealthy:
			db.Status = "running"
		default:
			db.Status = in.State
		}
	}
	switch {
	case app.Stopped:
		db.Status = "stopped"
	case app.CurrentRelease == 0:
		db.Status = "deploying"
	}
	links, err := s.Store.DatabaseLinks(ctx, app.Name)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		db.Links = append(db.Links, types.DatabaseLink{App: l.App, Var: l.Var})
	}
	if v, err := s.Store.Volume(ctx, app.Name, "data"); err == nil {
		info := s.volumeInfo(*v)
		db.Volume = &info
		if set, err := s.Store.VolumeBackupFor(ctx, v.ID); err == nil {
			runs, err := s.Store.BackupRuns(ctx, v.ID, 10)
			if err != nil {
				return nil, err
			}
			status := cluster.BackupStatus(*set, runs)
			db.Backups = &status
		}
	}
	return db, nil
}

func (s *Server) destroyDatabase(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e, app, err := s.lookupDatabase(ctx, r.PathValue("engine"), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	links, err := s.Store.DatabaseLinks(ctx, app.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(links) > 0 {
		var apps []string
		for _, l := range links {
			apps = append(apps, l.App)
		}
		s.fail(w, r, httpErrorf(http.StatusConflict, "%s is linked to %s; unlink it first: jokku db:%s:unlink %s <app>",
			r.PathValue("name"), strings.Join(apps, ", "), e.Name, r.PathValue("name")))
		return
	}
	if err := s.Store.DeleteApp(ctx, app.Name); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "app", "", "", "%s destroyed %s database %s; its backups stay in their bucket", actor(r), e.Name, r.PathValue("name"))
	s.Cluster.Changed()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) linkDatabase(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	e, db, err := s.lookupDatabase(ctx, r.PathValue("engine"), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req types.LinkDatabaseRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	target, err := s.Store.App(ctx, req.App)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if target.Kind != "" {
		s.fail(w, r, badRequest("%s is a database; link databases to apps", req.App))
		return
	}
	varName := e.EnvVar
	if req.Alias != "" {
		varName = strings.ToUpper(strings.TrimSuffix(req.Alias, "_URL")) + "_URL"
	}
	if err := validConfigKey(varName); err != nil {
		s.fail(w, r, err)
		return
	}
	all, err := s.Store.DatabaseLinks(ctx, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, l := range all {
		if l.App == req.App && l.Var == varName {
			s.fail(w, r, httpErrorf(http.StatusConflict, "%s already gets %s from %s; link this one under another name with --alias",
				req.App, varName, l.Database))
			return
		}
	}
	vars, err := s.Store.ConfigVars(ctx, db.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.LinkDatabase(ctx, db.Name, req.App, varName); err != nil {
		if errors.Is(err, store.ErrExists) {
			err = httpErrorf(http.StatusConflict, "%s is already linked to %s", name, req.App)
		}
		s.fail(w, r, err)
		return
	}
	_, changed, err := s.Store.UpdateConfigVars(ctx, req.App, types.ConfigPatch{Set: map[string]string{varName: e.URL(name, vars)}})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "app", req.App, "", "%s linked %s database %s to %s as %s", actor(r), e.Name, name, req.App, varName)
	st := newStream(w)
	st.Log(fmt.Sprintf("-----> Linked %s to %s: %s is set", name, req.App, varName))
	st.Done(s.restartLinked(ctx, target, changed && !req.NoRestart, actor(r), st))
}

func (s *Server) unlinkDatabase(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name, appName := r.PathValue("name"), r.PathValue("app")
	e, db, err := s.lookupDatabase(ctx, r.PathValue("engine"), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	target, err := s.Store.App(ctx, appName)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	varName, err := s.Store.UnlinkDatabase(ctx, db.Name, appName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = httpErrorf(http.StatusNotFound, "%s isn't linked to %s", name, appName)
		}
		s.fail(w, r, err)
		return
	}
	_, changed, err := s.Store.UpdateConfigVars(ctx, appName, types.ConfigPatch{Unset: []string{varName}})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "app", appName, "", "%s unlinked %s database %s from %s", actor(r), e.Name, name, appName)
	st := newStream(w)
	st.Log(fmt.Sprintf("-----> Unlinked %s from %s: %s is unset", name, appName, varName))
	st.Done(s.restartLinked(ctx, target, changed && r.URL.Query().Get("no_restart") != "true", actor(r), st))
}

// restartLinked restarts an app whose config a link changed, if it is
// deployed.
func (s *Server) restartLinked(ctx context.Context, app *types.App, restart bool, who string, st *stream) error {
	if !restart || app.CurrentRelease == 0 {
		return nil
	}
	return s.Deployer.PS(context.WithoutCancel(ctx), app.Name, "apply", who, st.Log)
}
