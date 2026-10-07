// Command jokku is the Jokku CLI, server daemon, proxy, SSH forced command,
// git hook and microVM init in one binary.
package main

import (
	"context"
	"os"

	"github.com/wes/jokku/internal/cli"
	"github.com/wes/jokku/internal/guest"
)

func main() {
	if guest.IsInit() {
		guest.Init() // never returns
	}
	os.Exit(cli.Main(context.Background(), os.Args[1:], cli.IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
}
