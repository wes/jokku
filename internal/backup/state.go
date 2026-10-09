package backup

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
)

// A cluster backup holds the control node's state, under <path>/state, and
// the root filesystems of recent releases, under <path>/artifacts (one
// backup per artifact, named by its file, sharing blocks).
const (
	StateDir     = "state"
	ArtifactsDir = "artifacts"
	// StateVolume and ArtifactsVolume stand for the volume ID in their
	// paths' jokku-backup.json.
	StateVolume     = "cluster-state"
	ArtifactsVolume = "cluster-artifacts"
)

// StateFiles are what the state backup holds, relative to the data dir: a
// snapshot of the database, and the control node's identity (its TLS key,
// which nodes pin, its WireGuard key, and its tokens).
var StateFiles = []string{"jokku.db", "tls/control.key", "tls/control.crt", "wireguard.key", "control-token", "agent-token"}

// PackState writes the state as a tar: db (a snapshot of the database) as
// jokku.db, and the other StateFiles from dataDir that exist. Uncompressed:
// it is backed up block by block, and blocks are compressed then.
func PackState(dataDir, db string, w io.Writer) error {
	tw := tar.NewWriter(w)
	for _, name := range StateFiles {
		src := filepath.Join(dataDir, filepath.FromSlash(name))
		if name == "jokku.db" {
			src = db
		}
		b, err := os.ReadFile(src)
		if errors.Is(err, fs.ErrNotExist) && name != "jokku.db" {
			continue
		}
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	return tw.Close()
}

// UnpackState extracts a state tar into dir (files 0600, directories 0700),
// returning what it held. Only StateFiles are extracted.
func UnpackState(r io.Reader, dir string) ([]string, error) {
	tr := tar.NewReader(r)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return names, err
		}
		name := path.Clean(h.Name)
		if h.Typeflag != tar.TypeReg || !slices.Contains(StateFiles, name) {
			return names, fmt.Errorf("%s is not part of a cluster's state", h.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return names, err
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return names, err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return names, err
		}
		names = append(names, name)
	}
	if !slices.Contains(names, "jokku.db") {
		return names, errors.New("the backup holds no database")
	}
	return names, nil
}

// Delete deletes one backup from a path. The blocks only it used stay until
// Collect.
func Delete(ctx context.Context, st Store, p, name string) error {
	return st.Delete(ctx, backupKey(p, name))
}
