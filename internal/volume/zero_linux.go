package volume

import (
	"os"

	"golang.org/x/sys/unix"
)

// zeroRange clears n bytes at off, punching a hole so the disk stays sparse.
func zeroRange(f *os.File, off, n int64) error {
	if unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, n) == nil {
		return nil
	}
	return writeZeros(f, off, n)
}
