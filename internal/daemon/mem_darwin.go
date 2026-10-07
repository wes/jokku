package daemon

import "golang.org/x/sys/unix"

// totalMemoryMB supports macOS for local development; Jokku nodes run Linux.
func totalMemoryMB() int {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int(n >> 20)
}
