//go:build unix

package guest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsFresh(t *testing.T) {
	dir := t.TempDir()
	if fresh, err := isFresh(dir); err != nil || !fresh {
		t.Fatalf("empty: %v, %v", fresh, err)
	}
	os.Mkdir(filepath.Join(dir, "lost+found"), 0o700)
	if fresh, _ := isFresh(dir); !fresh {
		t.Fatal("only lost+found should count as fresh")
	}
	os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17"), 0o600)
	if fresh, _ := isFresh(dir); fresh {
		t.Fatal("a volume with data is not fresh")
	}
}

func TestCopyTreeSeedsAVolume(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	os.Chmod(src, 0o750)
	os.MkdirAll(filepath.Join(src, "conf", "d"), 0o711)
	os.WriteFile(filepath.Join(src, "conf", "app.ini"), []byte("x=1"), 0o640)
	os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh"), 0o755)
	os.Symlink("conf/app.ini", filepath.Join(src, "current"))

	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		"":             os.ModeDir | 0o750,
		"conf/d":       os.ModeDir | 0o711,
		"conf/app.ini": 0o640,
		"run.sh":       0o755,
	} {
		info, err := os.Stat(filepath.Join(dst, path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != want {
			t.Errorf("%s: mode %v, want %v", path, info.Mode(), want)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dst, "current")); err != nil || string(b) != "x=1" {
		t.Errorf("symlink: %q, %v", b, err)
	}
	if link, _ := os.Readlink(filepath.Join(dst, "current")); link != "conf/app.ini" {
		t.Errorf("symlink target %q", link)
	}
}
