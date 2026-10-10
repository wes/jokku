//go:build unix

package splash

import "golang.org/x/sys/unix"

// disk is the size of the filesystem holding dir, what's used of it, and
// what's free for anyone but root (df's Size, Used and Avail).
func disk(dir string) (total, used, avail int) {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return 0, 0, 0
	}
	bs := uint64(st.Bsize)
	return int(st.Blocks * bs >> 20), int((st.Blocks - st.Bfree) * bs >> 20), int(uint64(st.Bavail) * bs >> 20)
}
