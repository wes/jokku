package session

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Archive writes dir's contents as a gzipped tar, keeping owners, modes and
// symlinks. Sockets, devices and pipes are left out.
func Archive(dir string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		link := ""
		switch mode := info.Mode(); {
		case mode&fs.ModeSymlink != 0:
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		case mode.IsDir(), mode.IsRegular():
		default:
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uname, hdr.Gname = "", "" // numeric owners only: names differ between images
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.CopyN(tw, f, hdr.Size)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// Extract unpacks a tar (gzipped or not) into dir, keeping owners and
// modes. With clear, dir is emptied first. Entries that would land outside
// dir, directly or through a symlink, are refused.
func Extract(r io.Reader, dir string, clear bool) error {
	br := bufio.NewReader(r)
	var src io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer gz.Close()
		src = gz
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if clear {
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
				return err
			}
		}
	}
	type dirTime struct {
		path string
		t    time.Time
	}
	var dirs []dirTime
	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(strings.TrimPrefix(hdr.Name, "./"))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) ||
			strings.Contains(name, string(filepath.Separator)+".."+string(filepath.Separator)) {
			return fmt.Errorf("%s: outside the directory", hdr.Name)
		}
		name = filepath.Clean(name)
		if name == "." {
			continue
		}
		target := filepath.Join(root, name)
		// The parent must be a real directory inside root: a symlink from the
		// archive (or already there) must not carry writes elsewhere.
		parent := filepath.Dir(target)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return err
		}
		if real, err := filepath.EvalSymlinks(parent); err != nil || (real != root && !strings.HasPrefix(real, root+string(filepath.Separator))) {
			return fmt.Errorf("%s: outside the directory", hdr.Name)
		}
		mode := fs.FileMode(hdr.Mode) & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if info, err := os.Lstat(target); err == nil && !info.IsDir() {
				os.Remove(target)
			}
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirTime{target, hdr.ModTime})
		case tar.TypeReg:
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			old := filepath.Join(root, filepath.Clean(filepath.FromSlash(hdr.Linkname)))
			if !strings.HasPrefix(old, root+string(filepath.Separator)) {
				return fmt.Errorf("%s: links outside the directory", hdr.Name)
			}
			os.RemoveAll(target)
			if err := os.Link(old, target); err != nil {
				return err
			}
			continue
		default:
			continue // devices, pipes: not restored
		}
		if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil && !os.IsPermission(err) && err != syscall.EPERM {
			return err
		}
		if hdr.Typeflag != tar.TypeSymlink {
			if err := os.Chmod(target, mode); err != nil {
				return err
			}
			os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Chtimes(dirs[i].path, dirs[i].t, dirs[i].t)
	}
	return nil
}
