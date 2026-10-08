package guest

import (
	"crypto/subtle"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/wes/jokku/internal/session"
)

// The guest agent: jokku enter, storage:export and storage:import reach the
// VM through it. It listens on vsock, which only the node's agent can reach
// (through Firecracker), and every session must carry the VM's token from
// the config drive, which only root in the VM can read, so the app can't use
// it to become root either.

// app is the running app, as the agent needs it: to pause it while a
// volume is copied, and to restart it after one is restored.
type app struct {
	mu      sync.Mutex
	pgid    int
	restart chan struct{}
}

func newApp() *app { return &app{restart: make(chan struct{}, 1)} }

func (a *app) setPGID(pid int) {
	a.mu.Lock()
	a.pgid = pid
	a.mu.Unlock()
}

func (a *app) signal(sig unix.Signal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pgid > 0 {
		unix.Kill(-a.pgid, sig)
	}
}

// Processes the agent starts are reaped by supervise's loop like any other
// child; they are registered (under reapMu, so the loop can't reap one
// first) to get their exit status back.
var (
	reapMu   sync.Mutex
	children = map[int]chan unix.WaitStatus{}
)

// reaped hands a reaped child's status to the agent, if it started it.
func reaped(pid int, ws unix.WaitStatus) bool {
	ch, ok := children[pid]
	if ok {
		delete(children, pid)
		ch <- ws
	}
	return ok
}

// serveAgent accepts sessions until the VM stops.
func serveAgent(cfg *Config, a *app) {
	if cfg.AgentToken == "" {
		return
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		logf("no vsock, so no jokku enter: %v", err)
		return
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: session.Port}); err != nil {
		logf("vsock bind: %v", err)
		return
	}
	if err := unix.Listen(fd, 16); err != nil {
		logf("vsock listen: %v", err)
		return
	}
	for {
		nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			if err == unix.EINTR || err == unix.ECONNABORTED {
				continue
			}
			logf("vsock accept: %v", err)
			return
		}
		go handleSession(cfg, a, os.NewFile(uintptr(nfd), "session"))
	}
}

func handleSession(cfg *Config, a *app, conn *os.File) {
	c := session.New(conn)
	defer c.Close()
	req, err := c.ReadRequest()
	if err != nil {
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(cfg.AgentToken)) != 1 {
		c.Exit(1, "unauthorized")
		return
	}
	switch req.Op {
	case session.OpExec:
		code, err := execSession(cfg, c, req)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		c.Exit(code, msg)
	case session.OpExport:
		if !req.Live {
			a.signal(unix.SIGSTOP)
			defer a.signal(unix.SIGCONT)
		}
		if err := session.Archive(req.Path, c.Writer(session.FrameStdout)); err != nil {
			c.Exit(1, err.Error())
			return
		}
		c.Exit(0, "")
	case session.OpImport:
		code, msg := importSession(a, c, req)
		c.Exit(code, msg)
	default:
		c.Exit(1, "unknown operation "+req.Op)
	}
}

// importSession restores a volume's files with the app paused, then
// restarts the app so it opens what was restored.
func importSession(a *app, c *session.Conn, req session.Request) (int, string) {
	pr, pw := io.Pipe()
	go func() {
		for {
			typ, p, err := c.Read()
			if err != nil {
				pw.CloseWithError(fmt.Errorf("the upload stopped: %w", err))
				return
			}
			switch typ {
			case session.FrameStdin:
				if _, err := pw.Write(p); err != nil {
					return
				}
			case session.FrameEOF:
				pw.Close()
				return
			}
		}
	}()
	a.signal(unix.SIGSTOP)
	err := session.Extract(pr, req.Path, req.Clear)
	pr.CloseWithError(io.ErrClosedPipe) // stop the reader if Extract ended early
	if err != nil {
		a.signal(unix.SIGCONT)
		return 1, fmt.Sprintf("restoring %s: %v (the files already written stay; the app was not restarted)", req.Path, err)
	}
	unix.Sync()
	select {
	case a.restart <- struct{}{}:
	default:
	}
	return 0, ""
}

// execSession runs a command in the VM, as the app's user unless asked for
// root, with the app's environment, and relays its I/O. It returns the
// command's exit status.
func execSession(cfg *Config, c *session.Conn, req session.Request) (int, error) {
	argv := req.Argv
	if len(argv) == 0 {
		for _, sh := range []string{"/bin/bash", "/bin/sh"} {
			if _, err := os.Stat(sh); err == nil {
				argv = []string{sh}
				break
			}
		}
		if len(argv) == 0 {
			return 127, fmt.Errorf("the image has no shell (/bin/bash or /bin/sh); name a command to run")
		}
	}
	env := withDefaults(cfg.Env)
	var cred *syscall.Credential
	home := "/root"
	if !req.Root {
		var err error
		if cred, home, err = lookupUser(cfg.User); err != nil {
			return 1, err
		}
	}
	env = append(append(env, "HOME="+home), req.Env...)
	path, err := lookPath(argv[0], getenv(env, "PATH"))
	if err != nil {
		return 127, err
	}
	dir := cfg.WorkDir
	if dir == "" {
		dir = "/"
	}
	cmd := &exec.Cmd{Path: path, Args: argv, Env: env, Dir: dir}

	var (
		input      io.WriteCloser // where stdin frames go
		outputs    []*os.File     // read until EOF, as stdout or stderr
		childFiles []*os.File     // the child's ends, closed here once it started
		master     *os.File
	)
	if req.TTY {
		m, s, err := openPTY()
		if err != nil {
			return 1, err
		}
		master = m
		defer master.Close()
		setSize(master, req.Rows, req.Cols)
		if cred != nil {
			unix.Fchown(int(s.Fd()), int(cred.Uid), int(cred.Gid))
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0, Credential: cred}
		input, outputs, childFiles = master, []*os.File{master}, []*os.File{s}
	} else {
		inR, inW, err := os.Pipe()
		if err != nil {
			return 1, err
		}
		outR, outW, err := os.Pipe()
		if err != nil {
			return 1, err
		}
		errR, errW, err := os.Pipe()
		if err != nil {
			return 1, err
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: cred}
		input, outputs, childFiles = inW, []*os.File{outR, errR}, []*os.File{inR, outW, errW}
		defer outR.Close()
		defer errR.Close()
	}

	status := make(chan unix.WaitStatus, 1)
	reapMu.Lock()
	err = cmd.Start()
	if err == nil {
		children[cmd.Process.Pid] = status
	}
	reapMu.Unlock()
	for _, f := range childFiles {
		f.Close()
	}
	if err != nil {
		input.Close()
		return 126, fmt.Errorf("starting %s: %w", argv[0], err)
	}
	pid := cmd.Process.Pid

	// Input: stdin, end of stdin, terminal size. A client that goes away
	// hangs the command up (unless it is already gone: its pid may be
	// someone else's by then).
	var exited sync.Mutex
	gone := false
	go func() {
		for {
			typ, p, err := c.Read()
			if err != nil {
				exited.Lock()
				if master != nil && !gone {
					unix.Kill(-pid, unix.SIGHUP)
				}
				exited.Unlock()
				if master == nil {
					input.Close()
				}
				return
			}
			switch typ {
			case session.FrameStdin:
				input.Write(p)
			case session.FrameEOF:
				if master == nil {
					input.Close()
				}
			case session.FrameResize:
				if master != nil {
					rows, cols := session.ParseResize(p)
					setSize(master, rows, cols)
				}
			}
		}
	}()
	var copying sync.WaitGroup
	for i, f := range outputs {
		typ := session.FrameStdout
		if i == 1 {
			typ = session.FrameStderr
		}
		copying.Add(1)
		go func() {
			defer copying.Done()
			io.Copy(c.Writer(typ), f) // a pty's master ends with EIO once the command is gone
		}()
	}

	ws := <-status
	exited.Lock()
	gone = true
	exited.Unlock()
	if master == nil {
		input.Close()
	}
	// Let the last output arrive, but don't wait on a background process
	// that kept the terminal open.
	done := make(chan struct{})
	go func() { copying.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	if ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return ws.ExitStatus(), nil
}

func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		m.Close()
		return nil, nil, err
	}
	n, err := unix.IoctlGetUint32(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	s, err := os.OpenFile(filepath.Join("/dev/pts", fmt.Sprint(n)), os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}

func setSize(f *os.File, rows, cols uint16) {
	if rows > 0 && cols > 0 {
		unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	}
}
