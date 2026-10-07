// Command jokku is the Jokku CLI, server daemon, SSH forced command and git
// hook in one binary.
package main

import (
	"context"
	"os"

	"github.com/wes/jokku/internal/cli"
)

func main() {
	os.Exit(cli.Main(context.Background(), os.Args[1:], cli.IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
}
