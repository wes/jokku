//go:build !linux && !darwin

package daemon

func totalMemoryMB() int { return 0 }
