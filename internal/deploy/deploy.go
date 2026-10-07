// Package deploy turns source into releases and rolls them out.
//
// Milestone 0 has the plumbing only: sources arrive, are inspected (Dockerfile,
// Procfile) and the deploy is recorded. The BuildKit build, the OCI-to-ext4
// conversion and the microVM rollout land in milestone 1; see
// docs/architecture.md.
package deploy

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/wes/jokku/internal/props"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

var ErrNoRuntime = errors.New("the microVM builder and runtime are not implemented yet (milestone 1)")

type Pipeline struct {
	Store *store.Store
	Log   *slog.Logger
}

func (p *Pipeline) Deploy(ctx context.Context, d *types.Deploy, sourcePath string, log func(string)) error {
	settings, err := p.settings(ctx, d.App)
	if err != nil {
		return err
	}
	src, err := Inspect(sourcePath, settings)
	if err != nil {
		return err
	}
	log("-----> Building " + d.App + " from " + src.Dockerfile)
	if len(src.Procfile) > 0 {
		log("-----> Found " + settings.ProcfilePath + ": " + strings.Join(sortedProcessTypes(src.Procfile), ", "))
	}
	return ErrNoRuntime
}

func (p *Pipeline) Restart(ctx context.Context, app, actor string) (bool, error) {
	// Nothing can be running before milestone 1.
	return false, nil
}

// Settings are the build-related properties resolved for one app.
type Settings struct {
	BuildDir       string
	DockerfilePath string
	ProcfilePath   string
}

func (p *Pipeline) settings(ctx context.Context, app string) (Settings, error) {
	get := func(plugin string) (map[string]string, error) {
		pl, _ := props.Lookup(plugin)
		global, err := p.Store.Properties(ctx, "", plugin)
		if err != nil {
			return nil, err
		}
		appProps, err := p.Store.Properties(ctx, app, plugin)
		if err != nil {
			return nil, err
		}
		return props.Compute(pl, appProps, global), nil
	}
	builder, err := get("builder")
	if err != nil {
		return Settings{}, err
	}
	dockerfile, err := get("builder-dockerfile")
	if err != nil {
		return Settings{}, err
	}
	ps, err := get("ps")
	if err != nil {
		return Settings{}, err
	}
	return Settings{
		BuildDir:       builder["build-dir"],
		DockerfilePath: dockerfile["dockerfile-path"],
		ProcfilePath:   ps["procfile-path"],
	}, nil
}

// Source is what Inspect learned about a source tarball.
type Source struct {
	Dockerfile string            // path of the Dockerfile inside the tarball
	Procfile   map[string]string // process type -> command; nil without a Procfile
}

// Inspect checks a source tarball has a Dockerfile and reads its Procfile.
func Inspect(tarPath string, s Settings) (*Source, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	base := cleanTarPath(s.BuildDir)
	dockerfile := cleanTarPath(path.Join(base, s.DockerfilePath))
	procfile := cleanTarPath(path.Join(base, s.ProcfilePath))

	src := &Source{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading source archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		switch cleanTarPath(h.Name) {
		case dockerfile:
			src.Dockerfile = dockerfile
		case procfile:
			b, err := io.ReadAll(io.LimitReader(tr, 64<<10))
			if err != nil {
				return nil, err
			}
			if src.Procfile, err = ParseProcfile(string(b)); err != nil {
				return nil, err
			}
		}
	}
	if src.Dockerfile == "" {
		return nil, fmt.Errorf("no %s found in the source; Jokku builds apps from a Dockerfile (set another path with: jokku builder-dockerfile:set <app> dockerfile-path <path>)", dockerfile)
	}
	return src, nil
}

func cleanTarPath(p string) string {
	p = path.Clean("/" + p)
	return strings.TrimPrefix(p, "/")
}

// ParseProcfile parses "type: command" lines, ignoring blanks and comments.
func ParseProcfile(s string) (map[string]string, error) {
	procs := map[string]string{}
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, cmd, ok := strings.Cut(line, ":")
		name, cmd = strings.TrimSpace(name), strings.TrimSpace(cmd)
		if !ok || name == "" || cmd == "" || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("Procfile line %d is not \"type: command\": %q", i+1, line)
		}
		procs[name] = cmd
	}
	return procs, nil
}

func sortedProcessTypes(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// web first, then alphabetical, matching how Dokku lists formations.
	slices.SortFunc(out, func(a, b string) int {
		switch {
		case a == "web":
			return -1
		case b == "web":
			return 1
		}
		return strings.Compare(a, b)
	})
	return out
}
