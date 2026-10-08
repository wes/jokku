//go:build unix

package agent

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// diskMB is the size and free space of the filesystem holding dir.
func diskMB(dir string) (total, free int) {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	return int(st.Blocks * bs >> 20), int(st.Bavail * bs >> 20)
}

// allocatedMB is the space a (sparse) file uses on disk.
func allocatedMB(path string) int {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Blocks * 512 >> 20)
	}
	return int(info.Size() >> 20)
}
