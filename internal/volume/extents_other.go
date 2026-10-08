//go:build !unix

package volume

import "os"

func eachDataBlock(_ *os.File, size int64, fn func(int64) error) error {
	return allBlocks(size, 0, fn)
}
