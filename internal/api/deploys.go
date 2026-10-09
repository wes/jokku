package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	imageref "github.com/google/go-containerregistry/pkg/name"

	"github.com/wes/jokku/internal/deploy"
	"github.com/wes/jokku/internal/types"
)

// Deployer turns sources into running releases and manages what runs.
type Deployer interface {
	// Deploy builds the source tarball at sourcePath and rolls it out,
	// reporting progress through log as Dokku-style lines.
	Deploy(ctx context.Context, d *types.Deploy, sourcePath string, log func(string)) error
	// PS runs restart, start, stop or rebuild. Restart applies config,
	// scale and resource changes.
	PS(ctx context.Context, app, action, actor string, log func(string)) error
	// Logs streams an app's log lines.
	Logs(ctx context.Context, app string, o types.LogOptions, line func(string)) error
	// RoutesChanged tells the runtime domains or proxy settings changed.
	RoutesChanged()
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
// Query parameters: source (git, archive, image; default archive) and ref
// (the pushed commit, or for image the image to deploy, with no body).
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
	source, ref := r.URL.Query().Get("source"), r.URL.Query().Get("ref")
	var body io.Reader = http.MaxBytesReader(w, r.Body, maxSourceSize)
	switch source {
	case "":
		source = "archive"
	case "git", "archive":
	case deploy.SourceImage:
		if _, err := imageref.ParseReference(ref); err != nil || strings.ContainsAny(ref, " \t\r\n") {
			s.fail(w, r, badRequest("%q is not an image reference, like postgres:17 or ghcr.io/you/app:v2", ref))
			return
		}
		tarball, err := deploy.ImageSource(ref)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		body = bytes.NewReader(tarball)
	default:
		s.fail(w, r, badRequest("Unknown deploy source %q", source))
		return
	}
	builder, err := s.computedProperties(ctx, name, "builder")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if source != deploy.SourceImage && builder["type"] == "image" {
		s.fail(w, r, httpErrorf(http.StatusConflict,
			"%s runs the image %s, so pushes don't deploy it. To build it from this repo instead: jokku builder:dockerfile %s (or builder:compose); to deploy another image: jokku builder:image %s <image>",
			name, builder["image"], name, name))
		return
	}

	d, err := s.Store.CreateDeploy(ctx, name, source, ref, actor(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	srcPath, size, err := s.spoolSource(d.ID, body)
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
	if source == deploy.SourceImage {
		st.Log(fmt.Sprintf("-----> Deploying image %s to %s", ref, name))
	} else {
		st.Log(fmt.Sprintf("-----> Received %s source for %s (%s)", source, name, humanBytes(size)))
	}
	// A dropped connection (Ctrl-C on git push, a flaky SSH session) must not
	// leave a half-finished rollout, so the deploy outlives the request.
	err = s.Deployer.Deploy(context.WithoutCancel(ctx), d, srcPath, st.Log)
	if err == nil && source == deploy.SourceImage {
		// From now on the app runs this image, and ps:rebuild pulls it again.
		if serr := s.Store.SetProperties(context.WithoutCancel(ctx), name, "builder", map[string]string{"type": "image", "image": ref, "file": ""}); serr != nil {
			s.Log.Error("recording the image", "app", name, "err", serr)
		} else if builder["type"] != "image" {
			st.Log(fmt.Sprintf("-----> %s now runs an image: pushes no longer deploy it (to build from git again: jokku builder:dockerfile %s)", name, name))
		}
	}
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
