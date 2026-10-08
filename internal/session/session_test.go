package session

import (
	"archive/tar"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrames(t *testing.T) {
	a, b := net.Pipe()
	ca, cb := New(a), New(b)
	go func() {
		ca.SendRequest(Request{Op: OpExec, Argv: []string{"sh"}, TTY: true, Rows: 24, Cols: 80})
		ca.Writer(FrameStdin).Write(bytes.Repeat([]byte("x"), 70<<10)) // more than one frame
		ca.Resize(50, 132)
		ca.Exit(3, "boom")
	}()
	req, err := cb.ReadRequest()
	if err != nil || req.Op != OpExec || req.Argv[0] != "sh" || !req.TTY || req.Cols != 80 {
		t.Fatalf("request %+v, %v", req, err)
	}
	var stdin []byte
	for {
		typ, p, err := cb.Read()
		if err != nil {
			t.Fatal(err)
		}
		switch typ {
		case FrameStdin:
			stdin = append(stdin, p...)
			continue
		case FrameResize:
			if r, c := ParseResize(p); r != 50 || c != 132 {
				t.Errorf("resize %d x %d", r, c)
			}
			continue
		case FrameExit:
			if code, msg := ParseExit(p); code != 3 || msg != "boom" {
				t.Errorf("exit %d %q", code, msg)
			}
		}
		break
	}
	if len(stdin) != 70<<10 {
		t.Errorf("stdin %d bytes", len(stdin))
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(src, "sub", "deeper"), 0o750)
	os.WriteFile(filepath.Join(src, "kuma.db"), []byte("SQLite format 3"), 0o640)
	os.WriteFile(filepath.Join(src, "sub", "deeper", "x"), []byte("x"), 0o600)
	os.Symlink("kuma.db", filepath.Join(src, "current"))
	os.WriteFile(filepath.Join(dst, "stale"), []byte("old"), 0o644)

	var buf bytes.Buffer
	if err := Archive(src, &buf); err != nil {
		t.Fatal(err)
	}
	if err := Extract(bytes.NewReader(buf.Bytes()), dst, ExtractOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "kuma.db")); string(b) != "SQLite format 3" {
		t.Errorf("kuma.db = %q", b)
	}
	if info, _ := os.Stat(filepath.Join(dst, "kuma.db")); info.Mode().Perm() != 0o640 {
		t.Errorf("kuma.db mode %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Join(dst, "sub")); info == nil || info.Mode().Perm() != 0o750 {
		t.Errorf("sub: %v", info)
	}
	if link, _ := os.Readlink(filepath.Join(dst, "current")); link != "kuma.db" {
		t.Errorf("symlink %q", link)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale")); err != nil {
		t.Error("without clear, existing files stay")
	}
	if err := Extract(bytes.NewReader(buf.Bytes()), dst, ExtractOptions{Clear: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale")); !os.IsNotExist(err) {
		t.Error("clear left an old file")
	}
}

func tarOf(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len("data"))
		}
		tw.WriteHeader(h)
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte("data"))
		}
	}
	tw.Close()
	return buf.Bytes()
}

func TestExtractStaysInside(t *testing.T) {
	outside := t.TempDir()
	for name, entries := range map[string][]*tar.Header{
		"dot dot":  {{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"absolute": {{Name: "/etc/evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"through a symlink": {
			{Name: "link", Typeflag: tar.TypeSymlink, Linkname: outside},
			{Name: "link/evil", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		"hard link out": {{Name: "x", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd"}},
	} {
		t.Run(name, func(t *testing.T) {
			dst := t.TempDir()
			err := Extract(bytes.NewReader(tarOf(t, entries...)), dst, ExtractOptions{})
			if err == nil || !strings.Contains(err.Error(), "outside the directory") {
				t.Fatalf("got %v", err)
			}
			if _, err := os.Stat(filepath.Join(outside, "evil")); err == nil {
				t.Fatal("a file was written outside")
			}
		})
	}
	// A plain (not gzipped) tar is fine too.
	dst := t.TempDir()
	if err := Extract(bytes.NewReader(tarOf(t, &tar.Header{Name: "./ok", Typeflag: tar.TypeReg, Mode: 0o644})), dst, ExtractOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "ok")); string(b) != "data" {
		t.Errorf("ok = %q", b)
	}
	if err := Extract(strings.NewReader("not a tar at all, long enough to fail"), dst, ExtractOptions{}); err == nil {
		t.Error("garbage was accepted")
	}
}

func TestExtractOwners(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, Uid: 1001, Gid: 1001})
	tw.Write([]byte("x"))
	tw.Close()

	owners := map[string][2]int{}
	lchown = func(path string, uid, gid int) error {
		owners[filepath.Base(path)] = [2]int{uid, gid}
		return nil
	}
	defer func() { lchown = os.Lchown }()

	// By default the archive's owners are kept...
	if err := Extract(bytes.NewReader(buf.Bytes()), t.TempDir(), ExtractOptions{}); err != nil {
		t.Fatal(err)
	}
	if owners["f"] != [2]int{1001, 1001} {
		t.Errorf("owner %v, want the archive's", owners["f"])
	}
	// ...unless an owner is given: an import gives everything to the
	// volume's owner, whoever made the archive.
	if err := Extract(bytes.NewReader(buf.Bytes()), t.TempDir(), ExtractOptions{Owner: &Owner{UID: 70, GID: 70}}); err != nil {
		t.Fatal(err)
	}
	if owners["f"] != [2]int{70, 70} {
		t.Errorf("owner %v, want 70:70", owners["f"])
	}
}

// A server that answers and closes at once (a command that exits without
// reading its input) must still have its exit status relayed, even while
// the client is sending.
func TestSpliceDeliversTheExitStatus(t *testing.T) {
	for range 200 {
		clientEnd, relayClient := net.Pipe()
		relayServer, serverEnd := net.Pipe()
		go func() {
			s := New(serverEnd)
			s.Exit(3, "")
			s.Close()
		}()
		go func() {
			defer relayClient.Close()
			defer relayServer.Close()
			Splice(relayClient, relayServer)
		}()
		c := New(clientEnd)
		go c.Writer(FrameStdin).Write(bytes.Repeat([]byte("x"), 100<<10))
		typ, p, err := c.Read()
		if err != nil || typ != FrameExit {
			t.Fatalf("got frame %q, %v; want the exit status", typ, err)
		}
		if code, _ := ParseExit(p); code != 3 {
			t.Fatalf("exit %d", code)
		}
		c.Close()
	}
}
