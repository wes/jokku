package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/wes/jokku/internal/database"
	"github.com/wes/jokku/internal/session"
	"github.com/wes/jokku/internal/store"
)

// maxExecOutput bounds what Exec keeps of a command's output.
const maxExecOutput = 8 << 20

// ExecError is a command that ran in an instance and failed.
type ExecError struct {
	Code   int
	Stderr string
}

func (e *ExecError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("exited with status %d", e.Code)
	}
	return e.Stderr
}

// Exec runs argv in instance in, with env and fed stdin, and returns what
// it wrote to stdout. It is for the control node's own use (a database's
// statistics, its users), so unlike jokku enter it records no event. A
// command that fails is an *ExecError with what it wrote to stderr.
func (c *Controller) Exec(ctx context.Context, in store.Instance, argv, env []string, stdin string) ([]byte, error) {
	conn, err := c.OpenSession(ctx, in)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()
	s := session.New(conn)
	if err := s.SendRequest(session.Request{Op: session.OpExec, Argv: argv, Env: env}); err != nil {
		return nil, err
	}
	if stdin != "" {
		if _, err := io.Copy(s.Writer(session.FrameStdin), strings.NewReader(stdin)); err != nil {
			return nil, err
		}
	}
	if err := s.Write(session.FrameEOF, nil); err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	for {
		typ, b, err := s.Read()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("running %s in %s: %w", argv[0], in.Name(), err)
		}
		switch typ {
		case session.FrameStdout:
			if stdout.Len() < maxExecOutput {
				stdout.Write(b)
			}
		case session.FrameStderr:
			if stderr.Len() < maxExecOutput {
				stderr.Write(b)
			}
		case session.FrameExit:
			code, msg := session.ParseExit(b)
			if code != 0 {
				why := strings.TrimSpace(stderr.String())
				if why == "" {
					why = msg
				}
				return stdout.Bytes(), &ExecError{Code: code, Stderr: why}
			}
			return stdout.Bytes(), nil
		}
	}
}

// ErrNotRunning is a database (or app) with no running instance to run a
// command in.
var ErrNotRunning = errors.New("not running")

// RunInDatabase runs cmd in app's database, e.
func (c *Controller) RunInDatabase(ctx context.Context, app string, e database.Engine, cmd database.Command) ([]byte, error) {
	in, ok, err := c.databaseInstance(ctx, app, e)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotRunning
	}
	return c.Exec(ctx, in, cmd.Argv, cmd.Env, cmd.Stdin)
}
