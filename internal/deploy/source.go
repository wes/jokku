package deploy

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/wes/jokku/internal/props"
)

// Settings are the build-related properties resolved for one app.
type Settings struct {
	Builder        string // "" or dockerfile, or compose
	BuildDir       string
	DockerfilePath string
	ProcfilePath   string
	ComposeFile    string
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
	composeProps, err := get("builder-compose")
	if err != nil {
		return Settings{}, err
	}
	ps, err := get("ps")
	if err != nil {
		return Settings{}, err
	}
	return Settings{
		Builder:        builder["selected"],
		BuildDir:       builder["build-dir"],
		DockerfilePath: dockerfile["dockerfile-path"],
		ProcfilePath:   ps["procfile-path"],
		ComposeFile:    composeProps["compose-file"],
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

func sortedProcessTypes[V any](m map[string]V) []string {
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
