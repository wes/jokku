//go:build !unix

package agent

func diskMB(string) (int, int) { return 0, 0 }

func allocatedMB(string) int { return 0 }
