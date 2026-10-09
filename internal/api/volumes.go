package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/vm"
)

// Volume sizes, in MiB.
const (
	minVolumeMB = 64
	maxVolumeMB = 16 * 1024 * 1024
)

func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request) {
	vols, err := s.Store.Volumes(r.Context(), r.PathValue("app"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []types.Volume{}
	for _, v := range vols {
		out = append(out, s.volumeInfo(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getVolume(w http.ResponseWriter, r *http.Request) {
	s.writeVolume(w, r, http.StatusOK)
}

func (s *Server) writeVolume(w http.ResponseWriter, r *http.Request, status int) {
	v, err := s.Store.Volume(r.Context(), r.PathValue("app"), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, status, s.volumeInfo(*v))
}

func (s *Server) createVolume(w http.ResponseWriter, r *http.Request) {
	var req types.CreateVolumeRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Type == "" {
		req.Type = types.VolumeLocal
	}
	if req.SizeMB == 0 {
		req.SizeMB = store.DefaultVolumeMB
	}
	switch {
	case !appNameRe.MatchString(req.Name):
		s.fail(w, r, badRequest("Volume name %q is invalid: use lowercase letters, digits and dashes", req.Name))
		return
	case req.Type != types.VolumeLocal:
		s.fail(w, r, badRequest("Unknown volume type %q (available: %s)", req.Type, types.VolumeLocal))
		return
	}
	if err := validVolumeSize(req.SizeMB); err != nil {
		s.fail(w, r, err)
		return
	}
	v := &store.Volume{App: r.PathValue("app"), Name: req.Name, Type: req.Type, SizeMB: req.SizeMB}
	if err := s.Store.CreateVolume(r.Context(), v); err != nil {
		s.fail(w, r, err)
		return
	}
	r.SetPathValue("name", req.Name)
	s.writeVolume(w, r, http.StatusCreated)
}

// resizeVolume grows a volume; its instance picks the size up on restart.
func (s *Server) resizeVolume(w http.ResponseWriter, r *http.Request) {
	var req types.ResizeVolumeRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.Store.Volume(r.Context(), r.PathValue("app"), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := validVolumeSize(req.SizeMB); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.SizeMB < v.SizeMB {
		s.fail(w, r, badRequest("Volumes can only grow: %s is %d MiB", v.Name, v.SizeMB))
		return
	}
	if err := s.Store.SetVolumeSize(r.Context(), v.ID, req.SizeMB); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Cluster.Changed()
	s.getVolume(w, r)
}

func (s *Server) destroyVolume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app := r.PathValue("app")
	v, err := s.Store.Volume(ctx, app, r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(v.Mounts) > 0 {
		m := v.Mounts[0]
		s.fail(w, r, httpErrorf(http.StatusConflict, "Volume %s is mounted at %s in %s; unmount it first: jokku storage:unmount %s %s:%s",
			v.Name, m.Path, m.ProcessType, app, v.Name, m.Path))
		return
	}
	if v.Moving() {
		s.fail(w, r, httpErrorf(http.StatusConflict, "Volume %s is moving to %s; destroy it once it is there", v.Name, v.MovingTo))
		return
	}
	if user, err := s.volumeUser(ctx, app, v.ID); err != nil {
		s.fail(w, r, err)
		return
	} else if user != "" {
		s.fail(w, r, httpErrorf(http.StatusConflict, "%s still has volume %s; restart the app to let go of it: jokku ps:restart %s", user, v.Name, app))
		return
	}
	if err := s.Store.DestroyVolume(ctx, v.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "volume", app, v.Node, "volume %s destroyed", v.Name)
	s.Cluster.Changed()
	w.WriteHeader(http.StatusNoContent)
}

// volumeUser names an instance that still mounts the volume, if any.
func (s *Server) volumeUser(ctx context.Context, app, id string) (string, error) {
	insts, err := s.Store.Instances(ctx, app)
	if err != nil {
		return "", err
	}
	for _, in := range insts {
		if in.Desired != store.DesiredRunning {
			continue
		}
		for _, m := range in.Volumes {
			if m.ID == id {
				return in.Name(), nil
			}
		}
	}
	return "", nil
}

func (s *Server) mountVolume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app := r.PathValue("app")
	if err := s.notCompose(ctx, app); err != nil {
		s.fail(w, r, err)
		return
	}
	var m types.VolumeMount
	if err := decode(r, &m); err != nil {
		s.fail(w, r, err)
		return
	}
	var err error
	if m, err = validMount(m); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.Store.Volume(ctx, app, r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v.Type == types.VolumeLocal && len(v.Mounts) > 0 {
		o := v.Mounts[0]
		s.fail(w, r, httpErrorf(http.StatusConflict, "Volume %s is already mounted at %s in %s: a local volume is attached to one instance",
			v.Name, o.Path, o.ProcessType))
		return
	}
	vols, err := s.Store.Volumes(ctx, app)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	count := 0
	for _, o := range vols {
		for _, om := range o.Mounts {
			if om.ProcessType != m.ProcessType {
				continue
			}
			count++
			if om.Path == m.Path || strings.HasPrefix(m.Path, om.Path+"/") || strings.HasPrefix(om.Path, m.Path+"/") {
				s.fail(w, r, httpErrorf(http.StatusConflict, "Volume %s is already mounted at %s in %s", o.Name, om.Path, m.ProcessType))
				return
			}
		}
	}
	if count >= vm.MaxVolumes {
		s.fail(w, r, badRequest("%s already mounts %d volumes, the most an instance can", m.ProcessType, vm.MaxVolumes))
		return
	}
	if v.Type == types.VolumeLocal {
		if err := s.oneInstance(ctx, app, m.ProcessType, v.Name); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if err := s.Store.AddVolumeMount(ctx, v.ID, m); err != nil {
		s.fail(w, r, err)
		return
	}
	s.getVolume(w, r)
}

// notCompose refuses mount changes for a compose app: its compose file says
// what is mounted where, and the next deploy would undo them.
func (s *Server) notCompose(ctx context.Context, app string) error {
	builder, err := s.Store.Properties(ctx, app, "builder")
	if err != nil {
		return err
	}
	if builder["type"] == "compose" {
		return httpErrorf(http.StatusConflict, "%s deploys a compose file, which says what each service mounts: change volumes: there", app)
	}
	return nil
}

// oneInstance refuses a local volume for a process type scaled past one.
func (s *Server) oneInstance(ctx context.Context, app, proc, volume string) error {
	procs, err := s.Store.Formation(ctx, app)
	if err != nil {
		return err
	}
	for _, p := range procs {
		if p.Type == proc && p.Quantity > 1 {
			return httpErrorf(http.StatusConflict, "%s runs %d instances, and local volume %s can only be attached to one; scale it down first: jokku ps:scale %s %s=1",
				proc, p.Quantity, volume, app, proc)
		}
	}
	return nil
}

func (s *Server) unmountVolume(w http.ResponseWriter, r *http.Request) {
	if err := s.notCompose(r.Context(), r.PathValue("app")); err != nil {
		s.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	m := types.VolumeMount{ProcessType: q.Get("process_type"), Path: q.Get("path")}
	v, err := s.Store.Volume(r.Context(), r.PathValue("app"), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if m.Path == "" && len(v.Mounts) == 1 {
		m = v.Mounts[0]
	}
	if m.ProcessType == "" {
		m.ProcessType = "web"
	}
	if err := s.Store.RemoveVolumeMount(r.Context(), v.ID, types.VolumeMount{ProcessType: m.ProcessType, Path: path.Clean(m.Path)}); err != nil {
		s.fail(w, r, err)
		return
	}
	s.getVolume(w, r)
}

func (s *Server) moveVolume(w http.ResponseWriter, r *http.Request) {
	var req types.MoveVolumeRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Cluster.MoveVolume(r.Context(), r.PathValue("app"), r.PathValue("name"), req.Node); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, err)
			return
		}
		s.fail(w, r, httpErrorf(http.StatusConflict, "%v", err))
		return
	}
	s.getVolume(w, r)
}

func validVolumeSize(mb int) error {
	if mb < minVolumeMB || mb > maxVolumeMB {
		return badRequest("Volume size must be between %d MiB and %d TiB", minVolumeMB, maxVolumeMB>>20)
	}
	return nil
}

// reservedPaths are where jokku's init and the kernel's filesystems live.
var reservedPaths = []string{"/.jokku", "/proc", "/sys", "/dev", "/run"}

func validMount(m types.VolumeMount) (types.VolumeMount, error) {
	if m.ProcessType == "" {
		m.ProcessType = "web"
	}
	if err := validProcessType(m.ProcessType); err != nil {
		return m, err
	}
	if !strings.HasPrefix(m.Path, "/") {
		return m, badRequest("Mount path %q must be absolute, like /app/storage", m.Path)
	}
	m.Path = path.Clean(m.Path)
	if m.Path == "/" {
		return m, badRequest("A volume cannot be mounted at /")
	}
	for _, p := range reservedPaths {
		if m.Path == p || strings.HasPrefix(m.Path, p+"/") {
			return m, badRequest("A volume cannot be mounted at %s", m.Path)
		}
	}
	return m, nil
}

func (s *Server) volumeInfo(v store.Volume) types.Volume {
	out := types.Volume{
		ID: v.ID, Name: v.Name, Type: v.Type, SizeMB: v.SizeMB, UsedMB: v.UsedMB, Node: v.Node, Mounts: v.Mounts, CreatedAt: v.CreatedAt,
	}
	if v.Node != "" {
		// Every node keeps its data in the same place as this one.
		out.Disk = filepath.Join(s.DataDir, "volumes", v.ID+".ext4")
	}
	switch {
	case v.State == store.VolumeDestroying:
		out.Status = "destroying"
	case v.Moving():
		out.Status = "moving"
		state := v.Transfer
		if state == "" {
			state = types.VolumeCopying
		}
		out.Move = &types.VolumeMove{To: v.MovingTo, State: state, CopiedMB: v.CopiedMB, Error: v.MoveError}
	case v.State == store.VolumeNew:
		out.Status = "new"
	case v.Status == types.VolumeMissing:
		out.Status = "missing"
	default:
		out.Status = "ready"
	}
	return out
}

// scaleWithVolumes refuses to scale a process type with a local volume past
// one instance.
func (s *Server) scaleWithVolumes(ctx context.Context, app string, quantities map[string]int) error {
	vols, err := s.Store.Volumes(ctx, app)
	if err != nil {
		return err
	}
	for _, v := range vols {
		if v.Type != types.VolumeLocal || v.State == store.VolumeDestroying {
			continue
		}
		for _, m := range v.Mounts {
			if n := quantities[m.ProcessType]; n > 1 {
				return httpErrorf(http.StatusConflict, "%s mounts local volume %s, which can only be attached to one instance, so it cannot run %d (jokku storage:list %s)",
					m.ProcessType, v.Name, n, app)
			}
		}
	}
	return nil
}

// nodeVolumes lists the volumes whose disks are on a node, as app/name.
func (s *Server) nodeVolumes(ctx context.Context, node string) ([]string, error) {
	vols, err := s.Store.Volumes(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range vols {
		if v.State != store.VolumeDestroying && (v.Node == node || v.MovingTo == node) {
			out = append(out, fmt.Sprintf("%s/%s", v.App, v.Name))
		}
	}
	return out, nil
}
