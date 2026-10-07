package client

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// dialSSH starts "ssh <dest> jokku api:dial-stdio" and returns its stdio as a
// connection to the remote API socket.
//
// On the server, the jokku user's forced command recognizes the request; with
// a regular account (root@host) the same string runs the remote jokku binary.
// Set JOKKU_SSH_COMMAND to replace "ssh" (like GIT_SSH_COMMAND), and
// JOKKU_SSH_MULTIPLEX=0 to turn off connection reuse.
func dialSSH(ctx context.Context, dest string) (net.Conn, error) {
	args := []string{"-T"}
	if os.Getenv("JOKKU_SSH_MULTIPLEX") != "0" {
		// Reuse one SSH connection across commands for a minute, so a burst of
		// CLI calls pays the handshake once.
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath=~/.ssh/jokku-%C", "-o", "ControlPersist=60")
	}
	host, port := dest, ""
	if h, p, err := net.SplitHostPort(dest); err == nil {
		host, port = h, p
	}
	if port != "" {
		args = append(args, "-p", port)
	}
	args = append(args, host, "jokku", "api:dial-stdio")

	var cmd *exec.Cmd
	if custom := os.Getenv("JOKKU_SSH_COMMAND"); custom != "" {
		cmd = exec.Command("sh", append([]string{"-c", custom + ` "$@"`, "ssh"}, args...)...)
	} else {
		cmd = exec.Command("ssh", args...)
	}
	cmd.Stderr = os.Stderr // host key prompts and ssh errors
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdConn{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

// cmdConn adapts a subprocess's stdin/stdout to net.Conn.
type cmdConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (c *cmdConn) Read(b []byte) (int, error)  { return c.stdout.Read(b) }
func (c *cmdConn) Write(b []byte) (int, error) { return c.stdin.Write(b) }

func (c *cmdConn) Close() error {
	c.stdin.Close()
	done := make(chan struct{})
	go func() { c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		c.cmd.Process.Kill()
		<-done
	}
	return nil
}

func (c *cmdConn) LocalAddr() net.Addr              { return pipeAddr{} }
func (c *cmdConn) RemoteAddr() net.Addr             { return pipeAddr{} }
func (c *cmdConn) SetDeadline(time.Time) error      { return nil }
func (c *cmdConn) SetReadDeadline(time.Time) error  { return nil }
func (c *cmdConn) SetWriteDeadline(time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "ssh" }
func (pipeAddr) String() string  { return "ssh" }

// ParseGitRemote extracts the SSH destination and app from a git remote URL
// such as "jokku@host:app", "jokku@host:app.git" or
// "ssh://jokku@host:2222/app".
func ParseGitRemote(remote string) (dest, app string, ok bool) {
	remote = strings.TrimSpace(remote)
	if rest, found := strings.CutPrefix(remote, "ssh://"); found {
		hostport, path, found := strings.Cut(rest, "/")
		if !found {
			return "", "", false
		}
		dest, app = hostport, path
	} else {
		host, path, found := strings.Cut(remote, ":")
		if !found || strings.Contains(host, "/") {
			return "", "", false
		}
		dest, app = host, path
	}
	app = strings.TrimSuffix(strings.Trim(app, "/"), ".git")
	if dest == "" || app == "" || strings.Contains(app, "/") {
		return "", "", false
	}
	return dest, app, true
}
