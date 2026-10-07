//go:build !linux

package guest

// IsInit is always false off Linux: microVM guests run Linux.
func IsInit() bool { return false }

// Init only exists in Linux builds.
func Init() { panic("guest init only runs on Linux") }
