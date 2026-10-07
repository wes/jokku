package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"
)

// Dokku's output conventions.
const (
	headerPrefix = "=====> "
	stepPrefix   = "-----> "
	infoPrefix   = "       "
	errorPrefix  = " !     "
)

func (c *Context) Header(format string, args ...any) {
	fmt.Fprintf(c.Stdout, headerPrefix+format+"\n", args...)
}

func (c *Context) Step(format string, args ...any) {
	fmt.Fprintf(c.Stdout, stepPrefix+format+"\n", args...)
}

func (c *Context) Info(format string, args ...any) {
	fmt.Fprintf(c.Stdout, infoPrefix+format+"\n", args...)
}

func (c *Context) Warn(format string, args ...any) {
	fmt.Fprintf(c.Stderr, errorPrefix+format+"\n", args...)
}

func printError(w io.Writer, msg string) {
	for _, line := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
		fmt.Fprintln(w, errorPrefix+line)
	}
}

// row is one line of a Dokku-style report. Flag is what selects it on the
// command line: "apps:report myapp --app-locked".
type row struct {
	Flag  string
	Label string
	Value string
}

// report prints rows under a header, or only the value of the row selected by
// a --flag.
func (c *Context) report(header string, rows []row) error {
	if len(c.extra) > 0 {
		want := c.extra[0]
		for _, r := range rows {
			if r.Flag == want {
				fmt.Fprintln(c.Stdout, r.Value)
				return nil
			}
		}
		flags := make([]string, len(rows))
		for i, r := range rows {
			flags[i] = "--" + r.Flag
		}
		return usageErr("Invalid flag passed, valid flags: %s", strings.Join(flags, ", "))
	}
	c.Header("%s", header)
	width := 0
	for _, r := range rows {
		width = max(width, len(r.Label)+1)
	}
	for _, r := range rows {
		fmt.Fprintf(c.Stdout, "%s%-*s %s\n", infoPrefix, width, r.Label+":", r.Value)
	}
	return nil
}

// printVars prints KEY: value lines aligned the way "dokku config:show" does.
func printVars(w io.Writer, vars map[string]string) {
	keys := sortedKeys(vars)
	width := 0
	for _, k := range keys {
		width = max(width, len(k)+1)
	}
	for _, k := range keys {
		fmt.Fprintf(w, "%-*s  %s\n", width, k+":", vars[k])
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// confirm asks the user to type want, Dokku style. Without a terminal it
// refuses, so scripts must pass --force.
func (c *Context) confirm(action, want string) error {
	f, ok := c.Stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return fmt.Errorf("%s needs confirmation; pass --force to run it non-interactively", c.Cmd.Name)
	}
	c.Warn("WARNING: Potentially Destructive Action")
	c.Warn("This command will %s.", action)
	c.Warn("To proceed, type %q", want)
	fmt.Fprint(c.Stdout, "\n> ")
	line, _ := bufio.NewReader(c.Stdin).ReadString('\n')
	if strings.TrimSpace(line) != want {
		return fmt.Errorf("Confirmation did not match %s. Aborted.", want)
	}
	return nil
}

func yesNo(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
