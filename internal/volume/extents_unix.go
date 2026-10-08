//go:build unix

package volume

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// eachDataBlock calls fn, in order, for each block of f that may hold data,
// skipping holes where the filesystem reports them (SEEK_DATA).
func eachDataBlock(f *os.File, size int64, fn func(int64) error) error {
	fd := int(f.Fd())
	next := int64(0) // the first block not visited yet
	for off := int64(0); off < size; {
		start, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			return nil // only holes left
		}
		if err != nil {
			return allBlocks(size, next, fn) // no hole support: read everything
		}
		end, err := unix.Seek(fd, start, unix.SEEK_HOLE)
		if err != nil || end > size {
			end = size
		}
		if end <= start {
			return allBlocks(size, next, fn)
		}
		for i := max(start/BlockSize, next); i*BlockSize < end; i++ {
			if err := fn(i); err != nil {
				return err
			}
			next = i + 1
		}
		off = end
	}
	return nil
}
