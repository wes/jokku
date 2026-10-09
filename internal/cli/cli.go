// Package cli implements the jokku command line: Dokku's commands, arguments
// and output, backed by the Jokku API.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/wes/jokku/internal/client"
)

// AppMode says how a command takes its app argument.
type AppMode int

const (
	NoApp       AppMode = iota
	NeedsApp            // "<app>" is required
	AppOrGlobal         // "<app>" or --global
	OptionalApp         // "[<app>]"; reports cover every app without one
)

type Flag struct {
	Name  string // long name without dashes
	Short string // optional single-letter alias
	Value string // placeholder when the flag takes a value; empty for booleans
	Help  string
}

type Command struct {
	Name    string
	Args    string // usage after the app, e.g. "KEY=VALUE..."
	Help    string
	App     AppMode
	Flags   []Flag
	MinArgs int
	MaxArgs int // -1 for no limit
	// AnyFlags accepts arbitrary --flags; report commands use them to select
	// one value (apps:report myapp --app-locked).
	AnyFlags bool
	// PassArgsAfter stops flag parsing after this many positional
	// arguments (the app included), so the rest reach a command verbatim:
	// "jokku enter app web ls -la".
	PassArgsAfter int
	// Local commands run without an API connection.
	Local  bool
	Hidden bool
	Run    func(*Context) error
	// serverOnly commands are refused when sent over ssh.
	serverOnly bool
}

// IO is where a command reads and writes.
type IO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

type Context struct {
	context.Context
	IO
	Cmd   *Command
	App   string
	Args  []string
	API   *client.Client
	flags map[string]string
	extra []string // undeclared --flags accepted by AnyFlags commands, in order
}

func (c *Context) Bool(name string) bool { _, ok := c.flags[name]; return ok }

func (c *Context) String(name string) string { return c.flags[name] }

// Global reports whether the command targets global settings instead of an app.
func (c *Context) Global() bool { return c.Bool("global") }

var (
	commands = map[string]*Command{}
	ordered  []*Command
)

var namespaceHelp = map[string]string{
	"apps":        "Manage apps",
	"config":      "Manage global and app-specific config vars",
	"domains":     "Manage domains used by the proxy",
	"git":         "Manage app deploys via git",
	"builder":     "Choose how an app is built: a Dockerfile, a compose file or an image",
	"ps":          "List processes and scale an app",
	"resource":    "Manage vCPU and memory per process type",
	"storage":     "Manage volumes: disks for an app's data",
	"registry":    "Manage logins to private image registries",
	"checks":      "Manage zero-downtime deploy checks",
	"proxy":       "Manage the HTTP proxy for an app",
	"letsencrypt": "Manage automatic TLS certificates",
	"ssh-keys":    "Manage SSH keys used for git push and remote commands",
	"nodes":       "Manage the machines in the cluster",
	"cluster":     "Add servers and see the cluster",
	"events":      "List recent events",
	"enter":       "Open a shell in a running instance of an app",
	"top":         "Watch the cluster, apps and machines live",
	"version":     "Print the jokku version",
	"update":      "Update jokku on this server",
	"logs":        "Display an app's log output",
	"releases":    "List an app's releases",
	"help":        "Print the list of commands",
}

func register(cmds ...*Command) {
	for _, c := range cmds {
		if _, dup := commands[c.Name]; dup {
			panic("duplicate command " + c.Name)
		}
		if c.MaxArgs == 0 && c.MinArgs > 0 {
			c.MaxArgs = c.MinArgs
		}
		commands[c.Name] = c
		ordered = append(ordered, c)
	}
}

func init() {
	register(appsCommands...)
	register(configCommands...)
	register(domainsCommands...)
	register(psCommands...)
	register(resourceCommands...)
	register(storageCommands...)
	register(builderCommands...)
	register(registryCommands...)
	register(sshKeysCommands...)
	register(nodesCommands...)
	register(clusterCommands...)
	register(topCommand)
	register(enterCommand)
	register(serverCommands...)
	register(propertyCommands()...)
	register(updateCommand)
	register(&Command{Name: "version", Help: namespaceHelp["version"], Local: true, Run: runVersion})
	register(&Command{Name: "help", Help: namespaceHelp["help"], Local: true, MaxArgs: 1, Flags: []Flag{{Name: "all", Help: "List every command"}}, Run: runHelp})
}

// UsageError is printed with the command's usage line.
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

func usageErr(format string, args ...any) error { return &UsageError{fmt.Sprintf(format, args...)} }

// Main runs the CLI with args (without the program name) and returns the exit
// code.
func Main(ctx context.Context, args []string, stdio IO) int {
	err := run(ctx, args, stdio)
	if err == nil {
		return 0
	}
	var exit *exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	printError(stdio.Stderr, err.Error())
	var ue *UsageError
	if errors.As(err, &ue) {
		if cmd := usageCommand(args); cmd != nil {
			fmt.Fprintf(stdio.Stderr, " !     Usage: jokku %s\n", usageLine(cmd))
		}
	}
	return 1
}

// exitError ends the program with a code and no message (the command already
// printed what it needed to).
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func run(ctx context.Context, args []string, stdio IO) error {
	var explicitApp string
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch a := args[0]; {
		case a == "--app" && len(args) > 1:
			explicitApp, args = args[1], args[2:]
		case strings.HasPrefix(a, "--app="):
			explicitApp, args = strings.TrimPrefix(a, "--app="), args[1:]
		case a == "-h" || a == "--help":
			args = []string{"help"}
		default:
			return usageErr("Unknown global flag %s", a)
		}
	}
	if len(args) == 0 {
		args = []string{"help"}
	}

	name, rest := args[0], args[1:]
	if ns, ok := strings.CutSuffix(name, ":help"); ok {
		return printNamespaceHelp(stdio.Stdout, ns)
	}
	cmd, ok := commands[name]
	if !ok {
		if aliased := aliases[name]; aliased != "" && len(rest) > 0 {
			cmd = commands[aliased]
		} else if namespaceHelp[name] != "" || hasNamespace(name) {
			return printNamespaceHelp(stdio.Stdout, name)
		} else {
			return fmt.Errorf("`%s` is not a jokku command. See `jokku help`", name)
		}
	}

	c := &Context{Context: ctx, IO: stdio, Cmd: cmd, flags: map[string]string{}}
	positional, err := parseFlags(c, rest)
	if err != nil {
		return err
	}
	if c.Bool("help") {
		return printCommandHelp(stdio.Stdout, cmd)
	}

	var target client.Target
	var inferredApp string
	if !cmd.Local {
		target, inferredApp, err = resolveTarget()
		if err != nil {
			return err
		}
	}
	if c.App, c.Args, err = resolveApp(cmd, c, explicitApp, inferredApp, positional); err != nil {
		return err
	}
	if len(c.Args) < cmd.MinArgs || (cmd.MaxArgs >= 0 && len(c.Args) > cmd.MaxArgs) {
		return usageErr("Wrong number of arguments")
	}
	if !cmd.Local {
		c.API = client.New(target, os.Getenv("JOKKU_ACTOR"))
	}
	return cmd.Run(c)
}

// aliases are bare commands Dokku accepts with an app argument, e.g.
// "jokku config myapp" for config:show.
var aliases = map[string]string{
	"config": "config:show",
}

func hasNamespace(ns string) bool {
	for name := range commands {
		if strings.HasPrefix(name, ns+":") {
			return true
		}
	}
	return false
}

func parseFlags(c *Context, args []string) ([]string, error) {
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(positional, args[i+1:]...), nil
		}
		if a == "-h" || a == "--help" {
			c.flags["help"] = ""
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			if n := c.Cmd.PassArgsAfter; n > 0 && len(positional) >= n {
				return append(positional, args[i+1:]...), nil
			}
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f, ok := c.Cmd.flag(name, !strings.HasPrefix(a, "--"))
		if !ok {
			if c.Cmd.AnyFlags && strings.HasPrefix(a, "--") {
				c.extra = append(c.extra, name)
				continue
			}
			return nil, usageErr("Unknown flag %s for %s", a, c.Cmd.Name)
		}
		if f.Value != "" && !hasValue {
			if i+1 >= len(args) {
				return nil, usageErr("Flag --%s needs a value", f.Name)
			}
			i++
			value = args[i]
		}
		c.flags[f.Name] = value
	}
	return positional, nil
}

func (cmd *Command) flag(name string, short bool) (Flag, bool) {
	for _, f := range cmd.allFlags() {
		if (short && f.Short == name) || (!short && f.Name == name) {
			return f, true
		}
	}
	return Flag{}, false
}

func (cmd *Command) allFlags() []Flag {
	if cmd.App == AppOrGlobal {
		return append([]Flag{{Name: "global", Help: "Apply to global settings instead of an app"}}, cmd.Flags...)
	}
	return cmd.Flags
}

// resolveApp works out which app a command targets. An explicit --app wins.
// Otherwise, if the app was inferred from the git remote, the first argument
// is still taken as the app when it names the inferred app or when the command
// would otherwise get too many arguments; this keeps "jokku config:show other"
// working from inside a repo, like the Dokku client.
func resolveApp(cmd *Command, c *Context, explicit, inferred string, args []string) (string, []string, error) {
	if cmd.App == NoApp || (cmd.App == AppOrGlobal && c.Global()) {
		if explicit != "" && cmd.App == NoApp {
			return "", nil, usageErr("%s does not take an app", cmd.Name)
		}
		return "", args, nil
	}
	if explicit != "" {
		return explicit, args, nil
	}
	if len(args) > 0 {
		takeFirst := inferred == "" || args[0] == inferred ||
			(cmd.MaxArgs >= 0 && len(args) > cmd.MaxArgs)
		if takeFirst {
			return args[0], args[1:], nil
		}
	}
	if inferred != "" {
		return inferred, args, nil
	}
	if cmd.App == OptionalApp {
		return "", args, nil
	}
	if cmd.App == AppOrGlobal {
		return "", nil, usageErr("Please specify an app or --global")
	}
	return "", nil, usageErr("Please specify an app to run the command on")
}

// resolveTarget finds the API: JOKKU_SOCKET, then JOKKU_HOST, then the local
// daemon socket, then the "jokku" git remote, which also names the default app.
func resolveTarget() (client.Target, string, error) {
	if s := os.Getenv("JOKKU_SOCKET"); s != "" {
		return client.Target{Socket: s}, "", nil
	}
	if h := os.Getenv("JOKKU_HOST"); h != "" {
		return client.Target{SSH: h}, "", nil
	}
	if _, err := os.Stat(client.DefaultSocket); err == nil {
		return client.Target{Socket: client.DefaultSocket}, "", nil
	}
	remote := os.Getenv("JOKKU_GIT_REMOTE")
	if remote == "" {
		remote = "jokku"
	}
	out, err := exec.Command("git", "remote", "get-url", remote).Output()
	if err == nil {
		if dest, app, ok := client.ParseGitRemote(string(out)); ok {
			return client.Target{SSH: dest}, app, nil
		}
		return client.Target{}, "", fmt.Errorf("git remote %q (%s) is not an ssh remote like jokku@host:app", remote, strings.TrimSpace(string(out)))
	}
	return client.Target{}, "", errors.New("no jokku server found: run this on the server, add a git remote (git remote add jokku jokku@host:app), or set JOKKU_HOST=jokku@host")
}

// Help

// usageLine is the full synopsis, flags included.
func usageLine(cmd *Command) string {
	parts := []string{cmd.Name}
	for _, f := range cmd.Flags { // --global is shown as "<app>|--global"
		if f.Value != "" {
			parts = append(parts, fmt.Sprintf("[--%s %s]", f.Name, f.Value))
		} else {
			parts = append(parts, fmt.Sprintf("[--%s]", f.Name))
		}
	}
	return strings.Join(append(parts, argsUsage(cmd)...), " ")
}

// shortUsage is the synopsis used in command lists.
func shortUsage(cmd *Command) string {
	return strings.Join(append([]string{cmd.Name}, argsUsage(cmd)...), " ")
}

func argsUsage(cmd *Command) []string {
	var parts []string
	switch cmd.App {
	case NeedsApp:
		parts = append(parts, "<app>")
	case AppOrGlobal:
		parts = append(parts, "<app>|--global")
	case OptionalApp:
		parts = append(parts, "[<app>]")
	}
	if cmd.Args != "" {
		parts = append(parts, cmd.Args)
	}
	return parts
}

func usageCommand(args []string) *Command {
	for _, a := range args {
		if cmd, ok := commands[a]; ok {
			return cmd
		}
	}
	return nil
}

func runHelp(c *Context) error {
	if len(c.Args) == 1 {
		if cmd, ok := commands[c.Args[0]]; ok && !strings.Contains(cmd.Name, ":") {
			return printCommandHelp(c.Stdout, cmd)
		}
		return printNamespaceHelp(c.Stdout, c.Args[0])
	}
	w := c.Stdout
	fmt.Fprintln(w, "Usage: jokku [--app <app>] <command> [<app>] [command-specific-options]")
	fmt.Fprintln(w)
	if c.Bool("all") {
		fmt.Fprintln(w, "Commands:")
		fmt.Fprintln(w)
		printCommandList(w, visible(func(*Command) bool { return true }))
		return nil
	}
	fmt.Fprintln(w, `Primary help options, type "jokku COMMAND:help" for more details, or "jokku help --all" to see all commands.`)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w)
	var names []string
	for ns := range namespaceHelp {
		names = append(names, ns)
	}
	sort.Strings(names)
	width := 0
	for _, n := range names {
		width = max(width, len(n))
	}
	for _, n := range names {
		fmt.Fprintf(w, "    %-*s  %s\n", width, n, namespaceHelp[n])
	}
	return nil
}

func printNamespaceHelp(w io.Writer, ns string) error {
	cmds := visible(func(c *Command) bool { return c.Name == ns || strings.HasPrefix(c.Name, ns+":") })
	if len(cmds) == 0 {
		return fmt.Errorf("`%s` is not a jokku command. See `jokku help`", ns)
	}
	fmt.Fprintf(w, "Usage: jokku %s[:COMMAND]\n\n", ns)
	if h := namespaceHelp[ns]; h != "" {
		fmt.Fprintf(w, "%s\n\n", h)
	}
	fmt.Fprintln(w, "Additional commands:")
	printCommandList(w, cmds)
	return nil
}

func printCommandHelp(w io.Writer, cmd *Command) error {
	fmt.Fprintf(w, "Usage: jokku %s\n\n%s\n", usageLine(cmd), cmd.Help)
	if flags := cmd.allFlags(); len(flags) > 0 {
		fmt.Fprintln(w, "\nFlags:")
		for _, f := range flags {
			name := "--" + f.Name
			if f.Short != "" {
				name = "-" + f.Short + ", " + name
			}
			if f.Value != "" {
				name += " " + f.Value
			}
			fmt.Fprintf(w, "    %-28s %s\n", name, f.Help)
		}
	}
	return nil
}

func visible(keep func(*Command) bool) []*Command {
	var out []*Command
	for _, c := range ordered {
		if !c.Hidden && keep(c) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func printCommandList(w io.Writer, cmds []*Command) {
	lines := make([][2]string, len(cmds))
	width := 0
	for i, c := range cmds {
		lines[i] = [2]string{shortUsage(c), c.Help}
		width = max(width, len(lines[i][0]))
	}
	width = min(width, 60)
	for _, l := range lines {
		fmt.Fprintf(w, "    %-*s  %s\n", width, l[0], l[1])
	}
}
