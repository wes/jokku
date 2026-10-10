//go:build !unix

package splash

func disk(string) (total, used, avail int) { return 0, 0, 0 }
