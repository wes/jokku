// Package build turns a source tarball with a Dockerfile into a rootfs
// artifact: a read-only ext4 image that microVMs boot from.
//
//	source.tar -> BuildKit -> image (docker format) -> flattened layers
//	           -> ext4 with /.jokku/init added -> artifacts/<digest>-<jokku version>.ext4
package build

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/wes/jokku/internal/deps"
	"github.com/wes/jokku/internal/guest"
	"github.com/wes/jokku/internal/version"
)

// BuildKitSocket is where the jokku-buildkitd service listens.
const BuildKitSocket = "/run/jokku-buildkit/buildkitd.sock"

type Builder struct {
	DataDir string // builds/ and artifacts/ live here
	Exe     string // the jokku binary, copied into every rootfs as its init
}

type Options struct {
	App            string
	DeployID       int64
	Source         string // source tarball
	BuildDir       string // subdirectory to build from
	DockerfilePath string // relative to BuildDir
	// Logins let BuildKit pull from private registries.
	Logins []RegistryLogin
}

// RegistryLogin is a credential for one registry server, such as ghcr.io.
type RegistryLogin struct {
	Server, Username, Password string
}

// Result describes a built artifact.
type Result struct {
	Artifact   string
	SHA256     string // hex, for nodes that download it
	Size       int64
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
	Port       int    // $PORT, see Port
	PortFrom   string // where Port came from, for the deploy log
	StopSignal string // the image's STOPSIGNAL
}

// Build runs the Dockerfile build and converts the image. Progress goes to
// log, one line at a time.
func (b *Builder) Build(ctx context.Context, o Options, log func(string)) (*Result, error) {
	src, err := b.Unpack(ctx, o.DeployID, o.Source)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(src)
	contextDir := filepath.Join(src, filepath.FromSlash(path.Clean("/"+o.BuildDir)))
	return b.BuildTarget(ctx, o.DeployID, Target{
		Name: o.App, Context: contextDir,
		Dockerfile: filepath.Join(contextDir, filepath.FromSlash(path.Clean("/"+o.DockerfilePath))),
	}, o.Logins, log)
}

// Unpack extracts a deploy's source tarball into a directory the caller
// removes when done.
func (b *Builder) Unpack(ctx context.Context, deployID int64, source string) (string, error) {
	src := filepath.Join(b.DataDir, "builds", strconv.FormatInt(deployID, 10), "src")
	os.RemoveAll(src)
	if err := os.MkdirAll(src, 0o700); err != nil {
		return "", err
	}
	if err := run(ctx, nil, "tar", "-xf", source, "-C", src, "--no-same-owner"); err != nil {
		os.RemoveAll(src)
		return "", fmt.Errorf("unpacking source: %w", err)
	}
	return src, nil
}

// Target is one image to make: a Dockerfile build in Context, or Image
// pulled from a registry with Copies (files from Context) added on top.
type Target struct {
	Name       string // names the image while it is built: the app, or app-service
	Context    string
	Dockerfile string // path of the Dockerfile
	Inline     string // or its contents
	Args       map[string]string
	Stage      string // the stage to build (docker build --target)
	Image      string
	Copies     []Copy
}

// Copy puts a file or directory from the build context into the image.
type Copy struct {
	Src string // relative to the context
	Dst string // absolute path in the image
}

// BuildTarget builds (or pulls) one image and converts it.
func (b *Builder) BuildTarget(ctx context.Context, deployID int64, t Target, logins []RegistryLogin, log func(string)) (*Result, error) {
	work := filepath.Join(b.DataDir, "builds", strconv.FormatInt(deployID, 10))
	name := strings.ToLower(t.Name)
	contextDir, dockerfile := t.Context, t.Dockerfile
	if t.Image != "" || t.Inline != "" {
		// A generated Dockerfile: FROM the image, plus its copies.
		text := t.Inline
		if t.Image != "" {
			var sb strings.Builder
			sb.WriteString("FROM " + t.Image + "\n")
			for _, c := range t.Copies {
				fmt.Fprintf(&sb, "COPY [%q, %q]\n", c.Src, c.Dst)
			}
			text = sb.String()
			if len(t.Copies) == 0 {
				contextDir = filepath.Join(work, "empty-context")
				if err := os.MkdirAll(contextDir, 0o700); err != nil {
					return nil, err
				}
			}
		}
		dockerfile = filepath.Join(work, "dockerfiles", name, "Dockerfile")
		if err := os.MkdirAll(filepath.Dir(dockerfile), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dockerfile, []byte(text), 0o600); err != nil {
			return nil, err
		}
	}
	imageTar := filepath.Join(work, "image-"+name+".tar")
	defer os.Remove(imageTar)
	env, err := dockerConfig(filepath.Join(work, "docker"), logins)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(filepath.Join(work, "docker"))
	args := []string{"--addr", "unix://" + BuildKitSocket, "build",
		"--progress=plain",
		"--frontend=dockerfile.v0",
		"--local", "context=" + contextDir,
		"--local", "dockerfile=" + filepath.Dir(dockerfile),
		"--opt", "filename=" + filepath.Base(dockerfile),
		"--output", "type=docker,name=jokku/" + name + ":build,dest=" + imageTar,
	}
	keys := make([]string, 0, len(t.Args))
	for k := range t.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--opt", "build-arg:"+k+"="+t.Args[k])
	}
	if t.Stage != "" {
		args = append(args, "--opt", "target="+t.Stage)
	}
	if err := runEnv(ctx, env, func(line string) { log("       " + line) }, deps.BuildKit.Path("buildctl"), args...); err != nil {
		if t.Image != "" {
			return nil, fmt.Errorf("pulling %s failed", t.Image)
		}
		return nil, fmt.Errorf("the Dockerfile build failed")
	}

	img, err := tarball.ImageFromPath(imageTar, nil)
	if err != nil {
		return nil, fmt.Errorf("reading the built image: %w", err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	digest, err := img.Digest()
	if err != nil {
		return nil, err
	}
	res := &Result{
		Entrypoint: cfg.Config.Entrypoint,
		Cmd:        cfg.Config.Cmd,
		Env:        cfg.Config.Env,
		WorkingDir: cfg.Config.WorkingDir,
		User:       cfg.Config.User,
		StopSignal: cfg.Config.StopSignal,
	}
	res.Port, res.PortFrom = Port(cfg)

	// The init binary is part of the artifact, so the name includes the jokku
	// version: a rebuild after an update picks up the new init.
	artifacts := filepath.Join(b.DataDir, "artifacts")
	res.Artifact = filepath.Join(artifacts, digest.Hex[:20]+"-"+version.Version+".ext4")
	if _, err := os.Stat(res.Artifact); err == nil {
		log("-----> Image unchanged, reusing its root filesystem")
		return res, res.checksum()
	}
	log("-----> Creating the microVM root filesystem")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		return nil, err
	}
	rootfs := filepath.Join(work, "rootfs")
	defer os.RemoveAll(rootfs)
	if err := b.unpack(ctx, img, rootfs); err != nil {
		return nil, err
	}
	if err := makeExt4(ctx, rootfs, res.Artifact); err != nil {
		return nil, err
	}
	return res, res.checksum()
}

func (r *Result) checksum() error {
	sum, size, err := FileSHA256(r.Artifact)
	r.SHA256, r.Size = sum, size
	return err
}

// FileSHA256 returns a file's hex SHA-256 and size.
func FileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// unpack writes the image's flattened filesystem (whiteouts applied) to dir
// and adds what the guest needs to boot: mount points and the init binary.
func (b *Builder) unpack(ctx context.Context, img v1.Image, dir string) error {
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	layers := mutate.Extract(img)
	defer layers.Close()
	// GNU tar running as root keeps ownership, modes, links and device nodes,
	// and refuses members that escape dir.
	cmd := exec.CommandContext(ctx, "tar", "-x", "-p", "--numeric-owner", "-f", "-", "-C", dir)
	cmd.Stdin = layers
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("unpacking image layers: %v: %s", err, strings.TrimSpace(string(out)))
	}
	for _, d := range []struct {
		path string
		mode fs.FileMode
	}{
		{"proc", 0o555}, {"sys", 0o555}, {"dev", 0o755}, {"run", 0o755},
		{"tmp", 0o777 | fs.ModeSticky}, {"etc", 0o755}, {strings.TrimPrefix(guest.MountDir, "/"), 0o755},
	} {
		p := filepath.Join(dir, d.path)
		if _, err := os.Lstat(p); err == nil {
			continue
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(p, d.mode); err != nil {
			return err
		}
	}
	return copyFile(b.Exe, filepath.Join(dir, strings.TrimPrefix(guest.InitPath, "/")), 0o755)
}

// makeExt4 packs dir into a read-only ext4 image sized to fit.
func makeExt4(ctx context.Context, dir, dst string) error {
	var files, bytes int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		files++
		if d.IsDir() {
			bytes += 4096
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			bytes += (info.Size() + 4095) / 4096 * 4096
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Data plus 20%, 256 bytes of inode table per file, and 64 MiB of slack
	// for metadata. The image is read-only, so it needs no free space beyond
	// that.
	sizeKB := bytes/1024*12/10 + files/4 + 64*1024
	inodes := files + files/5 + 1024
	tmp := dst + ".tmp"
	defer os.Remove(tmp)
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := f.Truncate(sizeKB * 1024); err != nil {
		f.Close()
		return err
	}
	f.Close()
	err = run(ctx, nil, "mkfs.ext4", "-q", "-F", "-O", "^has_journal", "-m", "0",
		"-N", strconv.FormatInt(inodes, 10), "-L", "jokku", "-d", dir, tmp)
	if err != nil {
		return fmt.Errorf("creating the root filesystem: %w", err)
	}
	return os.Rename(tmp, dst)
}

func copyFile(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// dockerConfig writes the logins where buildctl looks for registry
// credentials (a Docker config.json in DOCKER_CONFIG) and returns the
// environment that points it there.
func dockerConfig(dir string, logins []RegistryLogin) ([]string, error) {
	if len(logins) == 0 {
		return nil, nil
	}
	auths := map[string]any{}
	for _, l := range logins {
		server := l.Server
		if server == "docker.io" || server == "index.docker.io" || server == "registry-1.docker.io" {
			server = "https://index.docker.io/v1/" // what Docker Hub logins are filed under
		}
		auths[server] = map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(l.Username + ":" + l.Password))}
	}
	b, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		return nil, err
	}
	return append(os.Environ(), "DOCKER_CONFIG="+dir), nil
}

// run runs a command. With onLine, its combined output is streamed line by
// line; otherwise it is included in the error.
func run(ctx context.Context, onLine func(string), name string, args ...string) error {
	return runEnv(ctx, nil, onLine, name, args...)
}

// runEnv is run with an environment (nil inherits this process's).
func runEnv(ctx context.Context, env []string, onLine func(string), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	if onLine == nil {
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			onLine(sc.Text())
		}
		io.Copy(io.Discard, pr)
		close(done)
	}()
	err := cmd.Wait()
	pw.Close()
	<-done
	return err
}
