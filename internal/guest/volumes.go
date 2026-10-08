//go:build unix

package guest

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// isFresh reports whether a volume's filesystem has never been used: it
// holds nothing but mkfs's lost+found.
func isFresh(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name() != "lost+found" {
			return false, nil
		}
	}
	return true, nil
}

// copyTree copies src's contents into dst, which already exists, keeping
// owners, modes and symlinks, and gives dst src's owner and mode. Other
// special files are skipped.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			if rel != "." {
				if err := os.Mkdir(target, 0o700); err != nil && !os.IsExist(err) {
					return err
				}
			}
		case mode.IsRegular():
			if err := copyFile(path, target); err != nil {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		default:
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			if err := os.Lchown(target, int(st.Uid), int(st.Gid)); err != nil {
				return err
			}
		}
		if mode&fs.ModeSymlink == 0 {
			// After chown, which clears setuid and setgid bits.
			return os.Chmod(target, mode&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
		}
		return nil
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
