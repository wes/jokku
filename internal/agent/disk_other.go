//go:build !unix

package agent

func diskMB(string) (int, int) { return 0, 0 }
