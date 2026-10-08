//go:build !unix

package cli

func onResize(func()) func() { return func() {} }
