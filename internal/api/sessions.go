package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/wes/jokku/internal/session"
	"github.com/wes/jokku/internal/store"
)

// enter opens a session in one of an app's running instances (jokku enter):
// the connection is upgraded, the client's request is passed on as an exec,
// and frames are relayed to and from the guest. Query: process (web, or
// web.2; empty picks the app's only process, or web).
func (s *Server) enter(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app := r.PathValue("app")
	in, err := s.pickInstance(ctx, app, r.URL.Query().Get("process"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	node, err := s.Cluster.OpenSession(ctx, *in)
	if err != nil {
		s.fail(w, r, httpErrorf(http.StatusBadGateway, "%v", err))
		return
	}
	defer node.Close()
	conn, brw, err := session.Hijack(w)
	if err != nil {
		return
	}
	defer conn.Close()
	client := session.NewReader(brw.Reader, conn)
	req, err := client.ReadRequest()
	if err != nil {
		return
	}
	req = session.Request{Op: session.OpExec, Argv: req.Argv, TTY: req.TTY, Rows: req.Rows, Cols: req.Cols, Root: req.Root, Env: req.Env}
	if err := session.New(node).SendRequest(req); err != nil {
		client.Exit(1, err.Error())
		return
	}
	what := "a shell"
	if len(req.Argv) > 0 {
		what = strings.Join(req.Argv, " ")
	}
	s.Store.AddEvent(ctx, "instance", app, in.Node, "%s ran %s in %s", actor(r), what, in.Name())
	session.Splice(session.ReadWriter{Reader: brw.Reader, Writer: conn}, node)
}

// pickInstance finds the running instance a session goes to.
func (s *Server) pickInstance(ctx context.Context, app, process string) (*store.Instance, error) {
	a, err := s.Store.App(ctx, app)
	if err != nil {
		return nil, err
	}
	if a.Stopped {
		return nil, httpErrorf(http.StatusConflict, "%s is stopped; start it with: jokku ps:start %s", app, app)
	}
	insts, err := s.Store.Instances(ctx, app)
	if err != nil {
		return nil, err
	}
	var running []store.Instance
	for _, in := range insts {
		if in.Desired == store.DesiredRunning && (in.State == store.StateHealthy || in.State == store.StateStarting) {
			running = append(running, in)
		}
	}
	sort.Slice(running, func(i, j int) bool {
		if running[i].ProcessType != running[j].ProcessType {
			return running[i].ProcessType < running[j].ProcessType
		}
		return running[i].Index < running[j].Index
	})
	var names, types []string
	for _, in := range running {
		names = append(names, in.Name())
		if !slices.Contains(types, in.ProcessType) {
			types = append(types, in.ProcessType)
		}
	}
	if len(running) == 0 {
		return nil, httpErrorf(http.StatusConflict, "%s has no running instances", app)
	}
	if process == "" {
		switch {
		case len(types) == 1:
			process = types[0]
		case slices.Contains(types, "web"):
			process = "web"
		default:
			return nil, httpErrorf(http.StatusBadRequest, "%s runs %s; say which: jokku enter %s <process>", app, strings.Join(types, ", "), app)
		}
	}
	for _, in := range running {
		if in.Name() == process || (!strings.Contains(process, ".") && in.ProcessType == process) {
			return &in, nil
		}
	}
	return nil, httpErrorf(http.StatusNotFound, "%s has no running %s (running: %s)", app, process, strings.Join(names, ", "))
}

// volumeInstance finds the running instance that mounts a volume, and where.
func (s *Server) volumeInstance(ctx context.Context, app, name string) (*store.Instance, string, error) {
	v, err := s.Store.Volume(ctx, app, name)
	if err != nil {
		return nil, "", err
	}
	a, err := s.Store.App(ctx, app)
	if err != nil {
		return nil, "", err
	}
	insts, err := s.Store.Instances(ctx, app)
	if err != nil {
		return nil, "", err
	}
	for _, in := range insts {
		if a.Stopped || in.Desired != store.DesiredRunning || (in.State != store.StateHealthy && in.State != store.StateStarting) {
			continue
		}
		for _, m := range in.Volumes {
			if m.ID == v.ID {
				return &in, m.Path, nil
			}
		}
	}
	return nil, "", httpErrorf(http.StatusConflict,
		"volume %s is not mounted in a running instance; its files are read and written through the instance that mounts it, so mount it and start the app first", name)
}

// exportVolume streams a volume's files as a .tar.gz, read inside the
// instance that mounts it. The app's processes pause meanwhile, so a database
// is copied at one point in time; ?live=true skips that.
func (s *Server) exportVolume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app, name := r.PathValue("app"), r.PathValue("name")
	in, path, err := s.volumeInstance(ctx, app, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	conn, err := s.Cluster.OpenSession(ctx, *in)
	if err != nil {
		s.fail(w, r, httpErrorf(http.StatusBadGateway, "%v", err))
		return
	}
	defer conn.Close()
	c := session.New(conn)
	if err := c.SendRequest(session.Request{Op: session.OpExport, Path: path, Live: r.URL.Query().Get("live") == "true"}); err != nil {
		s.fail(w, r, err)
		return
	}
	wrote := false
	for {
		typ, p, err := c.Read()
		if err != nil {
			if !wrote {
				s.fail(w, r, httpErrorf(http.StatusBadGateway, "the copy of %s was cut off: %v", name, err))
				return
			}
			panic(http.ErrAbortHandler) // the client sees a truncated archive, and an error
		}
		switch typ {
		case session.FrameStdout:
			if !wrote {
				w.Header().Set("Content-Type", "application/gzip")
				w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", app+"-"+name+".tar.gz"))
				w.WriteHeader(http.StatusOK)
				wrote = true
			}
			if _, err := w.Write(p); err != nil {
				return
			}
		case session.FrameExit:
			if code, msg := session.ParseExit(p); code != 0 {
				if !wrote {
					s.fail(w, r, httpErrorf(http.StatusInternalServerError, "exporting %s: %s", name, msg))
					return
				}
				panic(http.ErrAbortHandler)
			}
			s.Store.AddEvent(ctx, "volume", app, in.Node, "%s exported volume %s", actor(r), name)
			return
		}
	}
}

// importVolume restores a volume's files from a tar (gzipped or not) in the
// request body, inside the instance that mounts it, then restarts the app's
// process there. ?clear=true empties the volume first; files belong to the
// volume's owner unless ?keep-owners=true.
func (s *Server) importVolume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app, name := r.PathValue("app"), r.PathValue("name")
	in, path, err := s.volumeInstance(ctx, app, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	conn, err := s.Cluster.OpenSession(ctx, *in)
	if err != nil {
		s.fail(w, r, httpErrorf(http.StatusBadGateway, "%v", err))
		return
	}
	defer conn.Close()
	c := session.New(conn)
	q := r.URL.Query()
	if err := c.SendRequest(session.Request{Op: session.OpImport, Path: path, Clear: q.Get("clear") == "true", KeepOwners: q.Get("keep-owners") == "true"}); err != nil {
		s.fail(w, r, err)
		return
	}
	go func() {
		if _, err := io.Copy(c.Writer(session.FrameStdin), r.Body); err != nil {
			conn.Close() // the guest reports the cut-off upload
			return
		}
		c.Write(session.FrameEOF, nil)
	}()
	for {
		typ, p, err := c.Read()
		if err != nil {
			s.fail(w, r, httpErrorf(http.StatusBadGateway, "restoring %s was cut off: %v", name, err))
			return
		}
		if typ != session.FrameExit {
			continue
		}
		if code, msg := session.ParseExit(p); code != 0 {
			s.fail(w, r, httpErrorf(http.StatusInternalServerError, "%s", msg))
			return
		}
		s.Store.AddEvent(ctx, "volume", app, in.Node, "%s restored volume %s; %s restarted", actor(r), name, in.Name())
		w.WriteHeader(http.StatusNoContent)
		return
	}
}
