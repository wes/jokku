//go:build unix

package agent

import "golang.org/x/sys/unix"

// diskMB is the size and free space of the filesystem holding dir.
func diskMB(dir string) (total, free int) {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	return int(st.Blocks * bs >> 20), int(st.Bavail * bs >> 20)
}
