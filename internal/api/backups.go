package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wes/jokku/internal/backup"
	"github.com/wes/jokku/internal/cluster"
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
	if _, err := s.Store.ClusterBackup(ctx); errors.Is(err, store.ErrNotFound) {
		// Nothing backs the cluster itself up yet: now this does.
		set := store.VolumeBackup{Destination: d.Name, Path: cluster.DefaultClusterBackupPath, Every: store.DefaultClusterBackupEvery,
			KeepRecent: store.DefaultKeepRecent, KeepDaily: store.DefaultKeepDaily}
		if err := s.Store.SetClusterBackup(ctx, set); err == nil {
			d.BacksUpCluster = true
		}
	}
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
	if set, err := s.Store.ClusterBackup(ctx); err == nil && set.Destination == name {
		s.fail(w, r, httpErrorf(http.StatusConflict, "the cluster is backed up to %s; back it up elsewhere first (jokku backups:cluster <destination>), or stop with jokku backups:cluster-unset", name))
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
	set := store.VolumeBackup{VolumeID: v.ID, Every: store.DefaultBackupEvery, KeepRecent: store.DefaultKeepRecent, KeepDaily: store.DefaultKeepDaily,
		AutoRestore: true}
	if cur, err := s.Store.VolumeBackupFor(ctx, v.ID); err == nil {
		set = *cur
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	if err := applyBackupRequest(&set, req, "jokku/"+app+"/"+name); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetVolumeBackup(ctx, set); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "volume", app, "", "%s set volume %s to back up to %s %s", actor(r), name, set.Destination, set.Path)
	set.App, set.Volume = app, name
	runs, _ := s.Store.BackupRuns(ctx, v.ID, 100)
	writeJSON(w, http.StatusOK, cluster.BackupStatus(set, runs))
}

// parseDays reads a duration that may be in days (7d), or "off" for none.
func parseDays(s string) (time.Duration, error) {
	switch s {
	case "off", "never", "0":
		return 0, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("%q is not a duration, like 15m, 6h or 7d", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not a duration, like 15m, 6h or 7d", s)
	}
	return d, nil
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
	if set, err := s.Store.ClusterBackup(ctx); err == nil && app == "" {
		runs, err := s.Store.ClusterBackupRuns(ctx, 100)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		status := cluster.BackupStatus(*set, runs)
		status.Cluster = true
		out = append(out, status)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	for _, b := range sets {
		runs, err := s.Store.BackupRuns(ctx, b.VolumeID, 100)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, cluster.BackupStatus(b, runs))
	}
	writeJSON(w, http.StatusOK, out)
}

// runVolumeBackup backs a volume up now, streaming the node's progress.
func (s *Server) runVolumeBackup(w http.ResponseWriter, r *http.Request) {
	app, name := r.PathValue("app"), r.PathValue("name")
	st := newStream(w)
	st.Log(fmt.Sprintf("-----> Backing up volume %s of %s", name, app))
	_, err := s.backUp(context.WithoutCancel(r.Context()), app, name, actor(r), "", st)
	st.Done(err)
}

func (s *Server) backUp(ctx context.Context, app, name, who, keep string, st *stream) (*types.BackupResult, error) {
	res, err := s.Cluster.BackupVolume(ctx, app, name, who, keep, func(line string) { st.Log("       " + line) })
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
			if _, err := s.backUp(bctx, app, name, actor(r), req.Backup, st); err != nil {
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

// discardOldCopy deletes the copy of a volume's disk that a node kept after
// the volume was restored elsewhere while it was down.
func (s *Server) discardOldCopy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app, name := r.PathValue("app"), r.PathValue("name")
	v, err := s.Store.Volume(ctx, app, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.DiscardStaleCopy(ctx, v.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "volume", app, v.StaleNode, "%s discarded the old copy of volume %s on %s", actor(r), name, v.StaleNode)
	s.Cluster.Changed()
	w.WriteHeader(http.StatusNoContent)
}

// applyBackupRequest changes a volume's (or the cluster's) backup settings
// as asked, leaving alone what the request doesn't say.
func applyBackupRequest(set *store.VolumeBackup, req types.SetVolumeBackupRequest, defaultPath string) error {
	set.Destination = req.Destination
	if req.Path != "" || set.Path == "" {
		set.Path = req.Path
	}
	if set.Path == "" {
		set.Path = defaultPath
	}
	set.Path = strings.Trim(set.Path, "/")
	if err := backup.CheckPath(set.Path); err != nil {
		return badRequest("%v", err)
	}
	if req.Every != "" {
		every, err := parseDays(req.Every)
		switch {
		case err != nil:
			return badRequest("--every: %v", err)
		case every > 0 && every < 5*time.Minute:
			return badRequest("--every: back up at most every 5 minutes")
		}
		set.Every = every
	}
	if req.KeepRecent != "" {
		recent, err := parseDays(req.KeepRecent)
		if err != nil {
			return badRequest("--keep-recent: %v", err)
		}
		set.KeepRecent = recent
	}
	switch req.AutoRestore {
	case "":
	case "on":
		set.AutoRestore = true
	case "off":
		set.AutoRestore = false
	default:
		return badRequest("--auto-restore is on or off")
	}
	if req.KeepDaily != nil {
		if *req.KeepDaily < 0 {
			return badRequest("--keep-daily: a number of days")
		}
		set.KeepDaily = *req.KeepDaily
	}
	return nil
}

// The cluster's own backups

func (s *Server) setClusterBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req types.SetVolumeBackupRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.AutoRestore != "" {
		s.fail(w, r, badRequest("--auto-restore is for volumes; a lost control node is restored with jokku restore-cluster"))
		return
	}
	if _, err := s.Store.BackupDestination(ctx, req.Destination); err != nil {
		s.fail(w, r, err)
		return
	}
	set := store.VolumeBackup{Every: store.DefaultClusterBackupEvery, KeepRecent: store.DefaultKeepRecent, KeepDaily: store.DefaultKeepDaily}
	if cur, err := s.Store.ClusterBackup(ctx); err == nil {
		set = *cur
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	if err := applyBackupRequest(&set, req, cluster.DefaultClusterBackupPath); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetClusterBackup(ctx, set); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "backups", "", "", "%s set the cluster to back up to %s %s", actor(r), set.Destination, set.Path)
	runs, _ := s.Store.ClusterBackupRuns(ctx, 100)
	status := cluster.BackupStatus(set, runs)
	status.Cluster = true
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) unsetClusterBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteClusterBackup(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(r.Context(), "backups", "", "", "%s stopped backing up the cluster; its backups stay in the bucket", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listClusterBackups(w http.ResponseWriter, r *http.Request) {
	out, err := s.Cluster.ListClusterBackups(r.Context())
	if err != nil {
		s.fail(w, r, clusterErr(err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) runClusterBackup(w http.ResponseWriter, r *http.Request) {
	st := newStream(w)
	st.Log("-----> Backing up the cluster: the control node's database and identity, and recent releases")
	res, err := s.Cluster.BackupCluster(context.WithoutCancel(r.Context()), actor(r), func(line string) { st.Log("       " + line) })
	if err == nil {
		st.Log(fmt.Sprintf("=====> Backed up as %s; %s uploaded", res.Name, humanBytes(res.NewBytes)))
	}
	st.Done(err)
}

// recentPoints is how many backups jokku top charts per volume.
const recentPoints = 24

// backupsOverview gathers everything about backups for jokku top.
func (s *Server) backupsOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := types.BackupsOverview{Volumes: []types.VolumeProtection{}}
	var err error
	if out.Destinations, err = s.Store.BackupDestinations(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	for i := range out.Destinations {
		out.Destinations[i].SecretAccessKey = ""
	}
	if key, saved, err := s.Store.BackupKey(ctx); err == nil {
		if k, err := backup.ParseKey(key); err == nil {
			out.Key = &types.BackupKeyStatus{ID: k.ID(), Saved: saved}
		}
	}
	if set, err := s.Store.ClusterBackup(ctx); err == nil {
		runs, err := s.Store.ClusterBackupRuns(ctx, recentPoints)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		status := cluster.BackupStatus(*set, runs)
		status.Cluster = true
		out.Cluster, out.ClusterRecent = &status, points(runs)
	}
	sets, err := s.Store.VolumeBackups(ctx, "", "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	byVolume := map[string]store.VolumeBackup{}
	for _, b := range sets {
		byVolume[b.VolumeID] = b
	}
	vols, err := s.Store.Volumes(ctx, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, v := range vols {
		if v.App == "" || v.State == store.VolumeDestroying {
			continue
		}
		info := s.volumeInfo(v)
		p := types.VolumeProtection{App: v.App, Volume: v.Name, Node: v.Node, SizeMB: v.SizeMB, UsedMB: v.UsedMB,
			Status: info.Status, Mounts: v.Mounts, OldCopy: info.OldCopy}
		if b, ok := byVolume[v.ID]; ok {
			runs, err := s.Store.BackupRuns(ctx, v.ID, recentPoints)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			status := cluster.BackupStatus(b, runs)
			p.Backups, p.Recent = &status, points(runs)
		}
		out.Volumes = append(out.Volumes, p)
	}
	writeJSON(w, http.StatusOK, out)
}

// points turns backup runs (newest first) into chart points, oldest first.
func points(runs []store.BackupRun) []types.BackupPoint {
	out := make([]types.BackupPoint, 0, len(runs))
	for i := len(runs) - 1; i >= 0; i-- {
		out = append(out, types.BackupPoint{At: runs[i].StartedAt, Status: runs[i].Status, NewBytes: runs[i].NewBytes})
	}
	return out
}
