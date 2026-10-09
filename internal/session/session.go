// Package session is how Jokku reaches inside a running microVM: a shell or
// command (jokku enter), and a volume's files (storage:export,
// storage:import). Every VM's init listens on a vsock port; the node's agent
// connects through Firecracker's vsock socket and relays a session from the
// control node, which relays it from the CLI:
//
//	CLI <-HTTP upgrade-> API <-HTTP upgrade over the mesh-> node agent <-vsock-> guest init
//
// All hops speak the same frames. The first is a Request; the guest answers
// with output frames and ends with an Exit frame. The node agent adds the
// VM's token to the request, so only it can open sessions with its VMs.
package session

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Port is the vsock port the guest listens on.
const Port = 1024

// Upgrade is the HTTP Upgrade token for a session.
const Upgrade = "jokku-session"

// Frame types.
const (
	FrameRequest byte = 'Q' // JSON Request, first, from the client
	FrameStdin   byte = 'I'
	FrameEOF     byte = 'C' // no more stdin
	FrameResize  byte = 'R' // rows and cols, uint16 each
	FrameStdout  byte = 'O'
	FrameStderr  byte = 'E'
	FrameExit    byte = 'X' // int32 status, then a message (empty on success)
)

// maxFrame bounds a frame's payload.
const maxFrame = 1 << 20

// Request is what a session does.
type Request struct {
	Op   string   `json:"op"` // exec, export or import
	Argv []string `json:"argv,omitempty"`
	TTY  bool     `json:"tty,omitempty"`
	Rows uint16   `json:"rows,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
	Root bool     `json:"root,omitempty"` // exec as root rather than the image's user
	Env  []string `json:"env,omitempty"`  // extra, e.g. TERM

	Path  string `json:"path,omitempty"`  // export, import and freeze: the directory
	Live  bool   `json:"live,omitempty"`  // export without pausing the app
	Clear bool   `json:"clear,omitempty"` // import into an emptied directory
	// KeepOwners restores the archive's numeric owners on import, instead
	// of giving everything to the directory's owner.
	KeepOwners bool `json:"keep_owners,omitempty"`

	Token string `json:"token,omitempty"` // set by the node agent
}

const (
	OpExec   = "exec"
	OpExport = "export"
	OpImport = "import"
	// OpFreeze pauses writes to the filesystem mounted at Path (a volume)
	// while the node backs it up. The guest says "frozen" on stdout, then
	// resumes writes at the client's EOF, when the client goes away, or
	// after FreezeLimit, and exits non-zero in the last case.
	OpFreeze = "freeze"
)

// FreezeLimit is the longest a backup may pause a volume's writes.
const FreezeLimit = 30 * time.Second

// Conn reads and writes frames. Writes are safe from several goroutines.
type Conn struct {
	r  *bufio.Reader
	w  io.Writer
	mu sync.Mutex
	c  io.Closer
}

func New(rw io.ReadWriteCloser) *Conn {
	return &Conn{r: bufio.NewReaderSize(rw, 64<<10), w: rw, c: rw}
}

// NewReader is New for a connection whose first bytes were already
// buffered (by an HTTP server that hijacked it).
func NewReader(r *bufio.Reader, rw io.ReadWriteCloser) *Conn {
	return &Conn{r: r, w: rw, c: rw}
}

func (c *Conn) Close() error { return c.c.Close() }

// Write sends one frame.
func (c *Conn) Write(typ byte, payload []byte) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("frame of %d bytes is too large", len(payload))
	}
	// One write per frame: no empty writes, which a synchronous pipe would
	// hold until the other side reads.
	frame := make([]byte, 5+len(payload))
	frame[0] = typ
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.w.Write(frame)
	return err
}

// Read receives one frame.
func (c *Conn) Read() (byte, []byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(c.r, hdr); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes is too large", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// SendRequest sends the opening frame.
func (c *Conn) SendRequest(req Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return c.Write(FrameRequest, b)
}

// ReadRequest receives the opening frame.
func (c *Conn) ReadRequest() (Request, error) {
	var req Request
	typ, b, err := c.Read()
	if err != nil {
		return req, err
	}
	if typ != FrameRequest {
		return req, errors.New("a session must start with a request")
	}
	return req, json.Unmarshal(b, &req)
}

// Exit ends a session with a status and, on failure, why.
func (c *Conn) Exit(status int, msg string) error {
	b := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(b, uint32(int32(status)))
	copy(b[4:], msg)
	return c.Write(FrameExit, b)
}

// ParseExit reads an Exit frame's payload.
func ParseExit(b []byte) (int, string) {
	if len(b) < 4 {
		return 1, "malformed exit"
	}
	return int(int32(binary.BigEndian.Uint32(b))), string(b[4:])
}

// Resize sends a terminal size.
func (c *Conn) Resize(rows, cols uint16) error {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b, rows)
	binary.BigEndian.PutUint16(b[2:], cols)
	return c.Write(FrameResize, b)
}

// ParseResize reads a Resize frame's payload.
func ParseResize(b []byte) (rows, cols uint16) {
	if len(b) < 4 {
		return 0, 0
	}
	return binary.BigEndian.Uint16(b), binary.BigEndian.Uint16(b[2:])
}

// Writer sends everything written to it as frames of typ.
func (c *Conn) Writer(typ byte) io.Writer { return frameWriter{c, typ} }

type frameWriter struct {
	c   *Conn
	typ byte
}

func (w frameWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), 32<<10)]
		if err := w.c.Write(w.typ, chunk); err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

// Hijack takes over an HTTP connection that asked to upgrade to a session,
// answering 101 Switching Protocols. Bytes the client sent after its request
// are in the returned reader.
func Hijack(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "cannot upgrade this connection", http.StatusInternalServerError)
		return nil, nil, err
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+Upgrade+"\r\n\r\n"); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, brw, nil
}

// ReadWriter pairs a reader with a writer.
type ReadWriter struct {
	io.Reader
	io.Writer
}

// Splice relays a session between a client and the server side (towards
// the guest) until the server's side ends, so its last frames, the exit
// status among them, always get through. Input the server no longer reads
// is dropped; a client that goes away closes the server's side, which ends
// the session.
func Splice(client io.ReadWriter, server io.ReadWriteCloser) {
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := client.Read(buf)
			if n > 0 {
				if _, werr := server.Write(buf[:n]); werr != nil {
					return // the server is done; its output is still on its way
				}
			}
			if err != nil {
				server.Close() // the client went away
				return
			}
		}
	}()
	io.Copy(client, server)
}
