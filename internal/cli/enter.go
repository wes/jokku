package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/wes/jokku/internal/session"
)

var enterCommand = &Command{
	Name: "enter", Help: "Open a shell (or run a command) in one of an app's running instances", App: NeedsApp,
	Args: "[<process>] [<command>...]", MaxArgs: -1, PassArgsAfter: 2,
	Flags: []Flag{{Name: "root", Help: "Run as root instead of the image's user (put it before the process)"}},
	Run:   enter,
}

// enter runs a shell, or the given command, inside a running instance:
// web (its first instance), web.2, or the app's only process when none is
// named. With a terminal on both ends it is interactive; otherwise stdin and
// stdout are plain pipes, for scripts. It exits with the command's status.
func enter(c *Context) error {
	process, argv := "", []string(nil)
	if len(c.Args) > 0 {
		process, argv = c.Args[0], c.Args[1:]
	}
	conn, err := c.API.Enter(c, c.App, process)
	if err != nil {
		return err
	}
	defer conn.Close()
	s := session.New(conn)
	tty := isTerminal(c.Stdin) && isTerminal(c.Stdout)
	req := session.Request{Op: session.OpExec, Argv: argv, TTY: tty, Root: c.Bool("root")}
	restore := func() {}
	if tty {
		in, out := int(c.Stdin.(*os.File).Fd()), int(c.Stdout.(*os.File).Fd())
		if cols, rows, err := term.GetSize(out); err == nil {
			req.Rows, req.Cols = uint16(rows), uint16(cols)
		}
		name := os.Getenv("TERM")
		if name == "" {
			name = "xterm-256color"
		}
		req.Env = []string{"TERM=" + name}
		if old, err := term.MakeRaw(in); err == nil {
			restore = func() { term.Restore(in, old) }
		}
		stop := onResize(func() {
			if cols, rows, err := term.GetSize(out); err == nil {
				s.Resize(uint16(rows), uint16(cols))
			}
		})
		defer stop()
	}
	defer restore()
	if err := s.SendRequest(req); err != nil {
		return err
	}
	go func() {
		io.Copy(s.Writer(session.FrameStdin), c.Stdin)
		s.Write(session.FrameEOF, nil)
	}()
	for {
		typ, p, err := s.Read()
		if err != nil {
			restore()
			return fmt.Errorf("the session ended: %v", err)
		}
		switch typ {
		case session.FrameStdout:
			c.Stdout.Write(p)
		case session.FrameStderr:
			c.Stderr.Write(p)
		case session.FrameExit:
			restore()
			code, msg := session.ParseExit(p)
			if msg != "" {
				return errors.New(msg)
			}
			if code != 0 {
				return &exitError{code: code}
			}
			return nil
		}
	}
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
