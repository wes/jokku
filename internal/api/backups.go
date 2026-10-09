package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wes/jokku/internal/backup"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// Backup destinations

func (s *Server) listBackupDestinations(w http.ResponseWriter, r *http.Request) {
	dests, err := s.Store.BackupDestinations(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for i := range dests {
		dests[i].SecretAccessKey = "" // never leaves the cluster but for a backup
	}
	writeJSON(w, http.StatusOK, dests)
}

func (s *Server) createBackupDestination(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req types.CreateBackupDestinationRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	d := types.BackupDestination{
		Name: req.Name, Endpoint: strings.TrimSpace(req.Endpoint), Region: req.Region, Bucket: req.Bucket,
		AccessKeyID: req.AccessKeyID, SecretAccessKey: req.SecretAccessKey, Encrypt: !req.NoEncrypt,
	}
	u, err := backup.ParseEndpoint(d.Endpoint)
	switch {
	case !appNameRe.MatchString(d.Name):
		err = badRequest("%q is not a destination name: lowercase letters, digits and dashes", d.Name)
	case err != nil:
		err = badRequest("%v", err)
	case d.Bucket == "":
		err = badRequest("which bucket? Pass --bucket")
	case d.AccessKeyID == "" || d.SecretAccessKey == "":
		err = badRequest("an access key ID and secret access key are needed")
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d.Endpoint = u.String()
	if d.Encrypt {
		if _, saved, err := s.Store.BackupKey(ctx); err != nil || !saved {
			s.fail(w, r, httpErrorf(http.StatusConflict, "encrypted backups need a key that you have saved: run jokku backups:key"))
			return
		}
	}
	st, err := backup.Open(d)
	if err == nil {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err = st.Check(cctx)
		cancel()
	}
	if err != nil {
		s.fail(w, r, badRequest("cannot use bucket %s at %s: %v", d.Bucket, d.Endpoint, err))
		return
	}
	if err := s.Store.CreateBackupDestination(ctx, d); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "backups", "", "", "%s added backup destination %s (bucket %s at %s)", actor(r), d.Name, d.Bucket, d.Endpoint)
	d.SecretAccessKey = ""
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) deleteBackupDestination(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	using, err := s.Store.VolumeBackups(ctx, "", name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(using) > 0 {
		var vols []string
		for _, b := range using {
			vols = append(vols, b.App+" "+b.Volume)
		}
		s.fail(w, r, httpErrorf(http.StatusConflict, "volumes are backed up to %s: %s; stop that first with jokku backups:unset <app> <volume>", name, strings.Join(vols, ", ")))
		return
	}
	if err := s.Store.DeleteBackupDestination(ctx, name); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "backups", "", "", "%s removed backup destination %s; its backups stay in the bucket", actor(r), name)
	w.WriteHeader(http.StatusNoContent)
}

// The backup key

func (s *Server) getBackupKey(w http.ResponseWriter, r *http.Request) {
	key, saved, err := s.Store.BackupKey(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeKey(w, r, http.StatusOK, key, saved)
}

// createBackupKey makes the cluster's backup key, or returns it if there is
// one already.
func (s *Server) createBackupKey(w http.ResponseWriter, r *http.Request) {
	key, saved, err := s.Store.CreateBackupKey(r.Context(), backup.NewKey().String())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeKey(w, r, http.StatusOK, key, saved)
}

func (s *Server) writeKey(w http.ResponseWriter, r *http.Request, status int, key string, saved bool) {
	k, err := backup.ParseKey(key)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, status, types.BackupKey{Key: key, ID: k.ID(), Saved: saved})
}

func (s *Server) saveBackupKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, _, err := s.Store.BackupKey(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetBackupKeySaved(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "backups", "", "", "%s confirmed saving the backup key", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

// A volume's backups

func (s *Server) setVolumeBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app, name := r.PathValue("app"), r.PathValue("name")
	var req types.SetVolumeBackupRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.Store.Volume(ctx, app, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v.Type != types.VolumeLocal {
		s.fail(w, r, badRequest("only local volumes are backed up"))
		return
	}
	if _, err := s.Store.BackupDestination(ctx, req.Destination); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Path == "" {
		req.Path = "jokku/" + app + "/" + name
	}
	req.Path = strings.Trim(req.Path, "/")
	if err := backup.CheckPath(req.Path); err != nil {
		s.fail(w, r, badRequest("%v", err))
		return
	}
	if err := s.Store.SetVolumeBackup(ctx, store.VolumeBackup{VolumeID: v.ID, Destination: req.Destination, Path: req.Path}); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "volume", app, "", "%s set volume %s to back up to %s %s", actor(r), name, req.Destination, req.Path)
	writeJSON(w, http.StatusOK, types.VolumeBackups{App: app, Volume: name, Destination: req.Destination, Path: req.Path})
}

func (s *Server) unsetVolumeBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app, name := r.PathValue("app"), r.PathValue("name")
	v, err := s.Store.Volume(ctx, app, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.DeleteVolumeBackup(ctx, v.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "volume", app, "", "%s stopped backing up volume %s; its backups stay in the bucket", actor(r), name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listVolumeBackups(w http.ResponseWriter, r *http.Request) {
	out, err := s.Cluster.ListBackups(r.Context(), r.PathValue("app"), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, clusterErr(err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// backupReport lists where volumes are backed up and how their latest
// backups went: every app's, or one's (?app=).
func (s *Server) backupReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app := r.URL.Query().Get("app")
	if app != "" {
		if _, err := s.Store.App(ctx, app); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	sets, err := s.Store.VolumeBackups(ctx, app, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []types.VolumeBackups{}
	for _, b := range sets {
		vb := types.VolumeBackups{App: b.App, Volume: b.Volume, Destination: b.Destination, Path: b.Path}
		runs, err := s.Store.BackupRuns(ctx, b.VolumeID, 100)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for i, run := range runs {
			info := &types.BackupRun{Name: run.Name, Status: run.Status, Error: run.Error, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt}
			if i == 0 {
				vb.Last = info
			}
			if run.Status == types.StatusSucceeded {
				vb.Succeeded = info
				break
			}
		}
		out = append(out, vb)
	}
	writeJSON(w, http.StatusOK, out)
}

// runVolumeBackup backs a volume up now, streaming the node's progress.
func (s *Server) runVolumeBackup(w http.ResponseWriter, r *http.Request) {
	app, name := r.PathValue("app"), r.PathValue("name")
	st := newStream(w)
	st.Log(fmt.Sprintf("-----> Backing up volume %s of %s", name, app))
	_, err := s.backUp(context.WithoutCancel(r.Context()), app, name, actor(r), st)
	st.Done(err)
}

func (s *Server) backUp(ctx context.Context, app, name, who string, st *stream) (*types.BackupResult, error) {
	res, err := s.Cluster.BackupVolume(ctx, app, name, who, func(line string) { st.Log("       " + line) })
	if err != nil {
		return nil, err
	}
	st.Log(fmt.Sprintf("=====> Backed up as %s: %d MB of data; %s uploaded", res.Name, res.Blocks, humanBytes(res.NewBytes)))
	return res, nil
}

// restoreVolume restores a volume from a backup, backing it up first unless
// asked not to, and follows the restore until it is done.
func (s *Server) restoreVolume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app, name := r.PathValue("app"), r.PathValue("name")
	var req types.RestoreVolumeRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.Store.Volume(ctx, app, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Backup == "" {
		// The latest backup now: not the one about to be taken of the
		// current data.
		list, err := s.Cluster.ListBackups(ctx, app, name)
		if err != nil {
			s.fail(w, r, clusterErr(err))
			return
		}
		if len(list.Backups) == 0 {
			s.fail(w, r, httpErrorf(http.StatusConflict, "volume %s has no backups in %s %s yet", name, list.Destination, list.Path))
			return
		}
		req.Backup = list.Backups[0].Name
	}
	st := newStream(w)
	bctx := context.WithoutCancel(ctx)
	if !req.SkipBackup && v.Node != "" {
		node, err := s.Store.Node(ctx, v.Node)
		if err == nil && node.Ready(time.Now()) {
			st.Log("-----> Backing up the current data first, so this restore can be undone")
			if _, err := s.backUp(bctx, app, name, actor(r), st); err != nil {
				st.Done(fmt.Errorf("%w (to restore without backing up first, pass --skip-backup)", err))
				return
			}
		} else {
			st.Log(fmt.Sprintf("-----> %s is down, so the current data can't be backed up first", v.Node))
		}
	}
	backupName, node, err := s.Cluster.RestoreVolume(bctx, app, name, req.Backup, req.Node, actor(r))
	if err != nil {
		st.Done(err)
		return
	}
	st.Log(fmt.Sprintf("-----> Restoring volume %s from backup %s on %s", name, backupName, node))
	st.Done(s.followRestore(ctx, v.ID, st))
}

// followRestore reports a restore's progress until it ends, or the client
// goes away (the restore carries on).
func (s *Server) followRestore(ctx context.Context, id string, st *stream) error {
	lastMB, lastState := -1, ""
	for {
		v, err := s.Store.VolumeByID(context.WithoutCancel(ctx), id)
		if err != nil {
			return err
		}
		if v.Restore == "" {
			if v.RestoreResult != "restored" {
				return fmt.Errorf("the restore failed, and the volume keeps its data: %s", v.RestoreResult)
			}
			st.Log("=====> Restored; the app is starting with it")
			return nil
		}
		switch {
		case v.Transfer == types.VolumeReceived && lastState != v.Transfer:
			st.Log("       Downloaded; stopping the app to put it in place")
		case v.Transfer == types.VolumeCopying && v.CopiedMB != lastMB && v.CopiedMB > 0:
			st.Log(fmt.Sprintf("       Downloaded %d MB", v.CopiedMB))
			lastMB = v.CopiedMB
		}
		lastState = v.Transfer
		select {
		case <-ctx.Done():
			return errors.New("stopped following the restore; it carries on (see jokku storage:report)")
		case <-time.After(time.Second):
		}
	}
}

// clusterErr turns the cluster's refusals (plain errors) into conflicts,
// keeping store errors as they are.
func clusterErr(err error) error {
	var he *httpError
	if errors.As(err, &he) || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrExists) {
		return err
	}
	return httpErrorf(http.StatusConflict, "%v", err)
}
