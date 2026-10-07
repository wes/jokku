// Package deps pins the third-party software a Jokku node runs: Firecracker,
// the guest kernel and BuildKit. "jokku setup" installs exactly these
// versions, verified by SHA-256, so each Jokku release brings the versions it
// was tested with.
package deps

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// Dir holds installed dependencies, one directory per name and version.
const Dir = "/var/lib/jokku/deps"

type Dep struct {
	Name    string
	Version string
	URL     map[string]string // GOARCH -> download URL
	SHA256  map[string]string // GOARCH -> hex digest
	// Files maps tarball members to installed names; nil means the download is
	// the file itself, installed as Name.
	Files map[string]string
}

var (
	Firecracker = Dep{
		Name: "firecracker", Version: "v1.17.0",
		URL: map[string]string{
			"amd64": "https://github.com/firecracker-microvm/firecracker/releases/download/v1.17.0/firecracker-v1.17.0-x86_64.tgz",
			"arm64": "https://github.com/firecracker-microvm/firecracker/releases/download/v1.17.0/firecracker-v1.17.0-aarch64.tgz",
		},
		SHA256: map[string]string{
			"amd64": "06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558",
			"arm64": "e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256",
		},
		Files: map[string]string{
			"release-v1.17.0-{fcarch}/firecracker-v1.17.0-{fcarch}": "firecracker",
			"release-v1.17.0-{fcarch}/jailer-v1.17.0-{fcarch}":      "jailer",
		},
	}

	// Kernel is Firecracker's CI guest kernel: virtio, ext4, overlayfs,
	// vsock and kernel IP autoconfiguration are built in.
	Kernel = Dep{
		Name: "vmlinux", Version: "6.1.155",
		URL: map[string]string{
			"amd64": "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155",
			"arm64": "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/aarch64/vmlinux-6.1.155",
		},
		SHA256: map[string]string{
			"amd64": "e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2",
			"arm64": "e3544b10603acbf3db492cb52e000d22ba202cb4b63b9add027565683e11c591",
		},
	}

	BuildKit = Dep{
		Name: "buildkit", Version: "v0.33.1",
		URL: map[string]string{
			"amd64": "https://github.com/moby/buildkit/releases/download/v0.33.1/buildkit-v0.33.1.linux-amd64.tar.gz",
			"arm64": "https://github.com/moby/buildkit/releases/download/v0.33.1/buildkit-v0.33.1.linux-arm64.tar.gz",
		},
		SHA256: map[string]string{
			"amd64": "4e044bcd62a0c0bbe6a8c94d73989de2bfe4c04dbc0f9d6021cf96b72cd1d965",
			"arm64": "4e1ba91f139761f249a1fa6c71e632dfdbdafeda416176ecadefbb95225e56c4",
		},
		Files: map[string]string{
			"bin/buildkitd":     "buildkitd",
			"bin/buildctl":      "buildctl",
			"bin/buildkit-runc": "buildkit-runc",
		},
	}

	All = []Dep{Firecracker, Kernel, BuildKit}
)

// Dir is where this version is installed.
func (d Dep) Dir() string { return filepath.Join(Dir, d.Name+"-"+d.Version) }

// Path of one installed file, e.g. Firecracker.Path("firecracker").
func (d Dep) Path(file string) string { return filepath.Join(d.Dir(), file) }

// KernelPath is the guest kernel image.
func KernelPath() string { return Kernel.Path(Kernel.Name) }

// Installed reports whether this version is fully installed.
func (d Dep) Installed() bool {
	_, err := os.Stat(filepath.Join(d.Dir(), ".complete"))
	return err == nil
}

// Install downloads, verifies and unpacks the dependency for this machine's
// architecture. It is a no-op when already installed.
func (d Dep) Install() error {
	if d.Installed() {
		return nil
	}
	arch := runtime.GOARCH
	url, sum := d.URL[arch], d.SHA256[arch]
	if url == "" {
		return fmt.Errorf("%s is not available for %s", d.Name, arch)
	}
	tmp, err := os.MkdirTemp(Dir, ".download-")
	if err != nil {
		if err := os.MkdirAll(Dir, 0o755); err != nil {
			return err
		}
		if tmp, err = os.MkdirTemp(Dir, ".download-"); err != nil {
			return err
		}
	}
	defer os.RemoveAll(tmp)

	file := filepath.Join(tmp, path.Base(url))
	if err := download(url, file, sum); err != nil {
		return fmt.Errorf("downloading %s %s: %w", d.Name, d.Version, err)
	}
	staging := filepath.Join(tmp, "out")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	if d.Files == nil {
		if err := os.Rename(file, filepath.Join(staging, d.Name)); err != nil {
			return err
		}
		if err := os.Chmod(filepath.Join(staging, d.Name), 0o644); err != nil {
			return err
		}
	} else if err := extract(file, staging, d.files(arch)); err != nil {
		return fmt.Errorf("unpacking %s: %w", d.Name, err)
	}
	if err := os.WriteFile(filepath.Join(staging, ".complete"), nil, 0o644); err != nil {
		return err
	}
	os.RemoveAll(d.Dir())
	return os.Rename(staging, d.Dir())
}

// files resolves the {fcarch} placeholder in Firecracker's archive layout.
func (d Dep) files(arch string) map[string]string {
	fcarch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[arch]
	out := map[string]string{}
	for member, name := range d.Files {
		out[strings.ReplaceAll(member, "{fcarch}", fcarch)] = name
	}
	return out
}

// Prune removes installed versions that are no longer pinned.
func Prune() error {
	keep := map[string]bool{}
	for _, d := range All {
		keep[filepath.Base(d.Dir())] = true
	}
	entries, err := os.ReadDir(Dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			os.RemoveAll(filepath.Join(Dir, e.Name()))
		}
	}
	return nil
}

func download(url, dst, wantSHA256 string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", url, got, wantSHA256)
	}
	return nil
}

// extract copies the wanted members of a .tar.gz into dir as executables.
func extract(archive, dir string, want map[string]string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	found := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, ok := want[strings.TrimPrefix(h.Name, "./")]
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		found++
	}
	if found != len(want) {
		return fmt.Errorf("archive is missing files (found %d of %d)", found, len(want))
	}
	return nil
}
