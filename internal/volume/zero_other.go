//go:build !linux

package volume

import "os"

// zeroRange clears n bytes at off.
func zeroRange(f *os.File, off, n int64) error { return writeZeros(f, off, n) }
