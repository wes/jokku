package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/wes/jokku/internal/types"
)

// Deployer turns sources into running releases.
type Deployer interface {
	// Deploy builds the source tarball at sourcePath and rolls it out,
	// reporting progress through log as Dokku-style lines.
	Deploy(ctx context.Context, d *types.Deploy, sourcePath string, log func(string)) error
	// Restart rolls the app's current release out again after a config,
	// scale or resource change. It reports false if nothing is deployed yet.
	Restart(ctx context.Context, app, actor string) (bool, error)
}

// maxSourceSize caps uploaded source tarballs.
const maxSourceSize = 2 << 30

func (s *Server) listDeploys(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	ds, err := s.Store.Deploys(r.Context(), r.PathValue("app"), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ds)
}

// createDeploy accepts a source tarball (Content-Type: application/x-tar),
// spools it to disk and streams the build and rollout as NDJSON events.
//
// Query parameters: source (git, archive; default archive) and ref (e.g. the
// pushed commit).
func (s *Server) createDeploy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("app")
	app, err := s.Store.App(ctx, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if app.Locked {
		s.fail(w, r, httpErrorf(http.StatusConflict, "App %s is locked, unlock it with: jokku apps:unlock %s", name, name))
		return
	}
	source := r.URL.Query().Get("source")
	switch source {
	case "":
		source = "archive"
	case "git", "archive":
	default:
		s.fail(w, r, badRequest("Unknown deploy source %q", source))
		return
	}

	d, err := s.Store.CreateDeploy(ctx, name, source, r.URL.Query().Get("ref"), actor(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	srcPath, size, err := s.spoolSource(d.ID, http.MaxBytesReader(w, r.Body, maxSourceSize))
	if err != nil {
		s.Store.SetDeployStatus(ctx, d.ID, types.StatusFailed, err.Error())
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			err = httpErrorf(http.StatusRequestEntityTooLarge, "Source is larger than %d bytes", maxSourceSize)
		}
		s.fail(w, r, err)
		return
	}

	st := newStream(w)
	st.Log(fmt.Sprintf("-----> Received %s source for %s (%s)", source, name, humanBytes(size)))
	// A dropped connection (Ctrl-C on git push, a flaky SSH session) must not
	// leave a half-finished rollout, so the deploy outlives the request.
	err = s.Deployer.Deploy(context.WithoutCancel(ctx), d, srcPath, st.Log)
	status, msg := types.StatusSucceeded, ""
	if err != nil {
		status, msg = types.StatusFailed, err.Error()
	}
	// Record the outcome even if the client went away mid-deploy.
	if serr := s.Store.SetDeployStatus(context.WithoutCancel(ctx), d.ID, status, msg); serr != nil {
		s.Log.Error("recording deploy status", "deploy", d.ID, "err", serr)
	}
	st.Done(err)
}

func (s *Server) spoolSource(deployID int64, body io.Reader) (string, int64, error) {
	dir := filepath.Join(s.DataDir, "builds", strconv.FormatInt(deployID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	path := filepath.Join(dir, "source.tar")
	f, err := os.Create(path)
	if err != nil {
		return "", 0, err
	}
	n, err := io.Copy(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return path, n, err
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
