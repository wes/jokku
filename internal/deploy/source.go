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

	"github.com/wes/jokku/internal/compose"
)

// Settings are the build-related properties resolved for one app.
type Settings struct {
	Builder        string // dockerfile, compose or image
	BuildDir       string
	DockerfilePath string
	ProcfilePath   string
	ComposeFile    string // "" finds the usual names
	Image          string // what an image app runs
}

func (p *Pipeline) settings(ctx context.Context, app string) (Settings, error) {
	b, err := p.computed(ctx, app, "builder")
	if err != nil {
		return Settings{}, err
	}
	s := Settings{
		Builder: b["type"], BuildDir: b["build-dir"], ProcfilePath: b["procfile"], Image: b["image"],
		DockerfilePath: "Dockerfile",
	}
	switch s.Builder {
	case "dockerfile":
		if b["file"] != "" {
			s.DockerfilePath = b["file"]
		}
	case "compose":
		s.ComposeFile = b["file"]
	}
	return s, nil
}

// Source is what Inspect learned about a source tarball.
type Source struct {
	Dockerfile string            // path of the Dockerfile inside the tarball
	Procfile   map[string]string // process type -> command; nil without a Procfile
}

// Inspect checks app's source tarball has a Dockerfile and reads its
// Procfile.
func Inspect(app, tarPath string, s Settings) (*Source, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	base := cleanTarPath(s.BuildDir)
	dockerfile := cleanTarPath(path.Join(base, s.DockerfilePath))
	procfile := cleanTarPath(path.Join(base, s.ProcfilePath))

	src := &Source{}
	composeFile := "" // for a hint when there is no Dockerfile
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
		name := cleanTarPath(h.Name)
		if dir, file := path.Split(name); composeFile == "" && path.Clean("/"+dir) == path.Clean("/"+base) && slices.Contains(compose.DefaultFiles, file) {
			composeFile = name
		}
		switch name {
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
		if composeFile != "" {
			return nil, fmt.Errorf("no %s found in the source, but there is %s; to deploy it: jokku builder:compose %s", dockerfile, composeFile, app)
		}
		return nil, fmt.Errorf("no %s found in the source; Jokku builds apps from a Dockerfile (to use another one: jokku builder:dockerfile %s <path>)", dockerfile, app)
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
