// Package build turns a source tarball with a Dockerfile into a rootfs
// artifact: a read-only ext4 image that microVMs boot from.
//
//	source.tar -> BuildKit -> image (docker format) -> flattened layers
//	           -> ext4 with /.jokku/init added -> artifacts/<digest>-<jokku version>.ext4
package build

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
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

// DefaultPort is $PORT when the image does not EXPOSE exactly one port.
const DefaultPort = 5000

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
}

// Result describes a built artifact.
type Result struct {
	Artifact   string
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
	Port       int
}

// Build runs the Dockerfile build and converts the image. Progress goes to
// log, one line at a time.
func (b *Builder) Build(ctx context.Context, o Options, log func(string)) (*Result, error) {
	work := filepath.Join(b.DataDir, "builds", strconv.FormatInt(o.DeployID, 10))
	src := filepath.Join(work, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(src)
	if err := run(ctx, nil, "tar", "-xf", o.Source, "-C", src, "--no-same-owner"); err != nil {
		return nil, fmt.Errorf("unpacking source: %w", err)
	}

	contextDir := filepath.Join(src, filepath.FromSlash(path.Clean("/"+o.BuildDir)))
	dockerfile := filepath.Join(contextDir, filepath.FromSlash(path.Clean("/"+o.DockerfilePath)))
	imageTar := filepath.Join(work, "image.tar")
	defer os.Remove(imageTar)
	err := run(ctx, func(line string) { log("       " + line) },
		deps.BuildKit.Path("buildctl"), "--addr", "unix://"+BuildKitSocket, "build",
		"--progress=plain",
		"--frontend=dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+filepath.Dir(dockerfile),
		"--opt", "filename="+filepath.Base(dockerfile),
		"--output", "type=docker,name=jokku/"+o.App+":build,dest="+imageTar,
	)
	if err != nil {
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
		Port:       DefaultPort,
	}
	if len(cfg.Config.ExposedPorts) == 1 {
		for p := range cfg.Config.ExposedPorts {
			if n, err := strconv.Atoi(strings.TrimSuffix(p, "/tcp")); err == nil {
				res.Port = n
			}
		}
	}

	// The init binary is part of the artifact, so the name includes the jokku
	// version: a rebuild after an update picks up the new init.
	artifacts := filepath.Join(b.DataDir, "artifacts")
	res.Artifact = filepath.Join(artifacts, digest.Hex[:20]+"-"+version.Version+".ext4")
	if _, err := os.Stat(res.Artifact); err == nil {
		log("-----> Image unchanged, reusing its root filesystem")
		return res, nil
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
	return res, nil
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

// run runs a command. With onLine, its combined output is streamed line by
// line; otherwise it is included in the error.
func run(ctx context.Context, onLine func(string), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
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
