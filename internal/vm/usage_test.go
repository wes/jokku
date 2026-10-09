package vm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAnonBytes(t *testing.T) {
	dir := t.TempDir()
	stat := filepath.Join(dir, "memory.stat")
	// A VM with 256 MiB of RAM that has read a lot from its disks: the page
	// cache (file) is the host's, not the VM's.
	os.WriteFile(stat, []byte("anon 272629760\nfile 352321536\nkernel 2048000\nanon_thp 0\n"), 0o644)
	if n, ok := anonBytes(stat); !ok || n != 272629760 {
		t.Errorf("anonBytes = %d, %v", n, ok)
	}
	if _, ok := anonBytes(filepath.Join(dir, "missing")); ok {
		t.Error("a missing memory.stat read as something")
	}
}
