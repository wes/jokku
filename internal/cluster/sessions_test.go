package cluster_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wes/jokku/internal/session"
)

// run opens an enter session and returns its output and exit status.
func (h *harness) run(app, process string, req session.Request, stdin string) (string, int, string) {
	h.t.Helper()
	conn, err := h.client.Enter(h.ctx, app, process)
	if err != nil {
		h.t.Fatal(err)
	}
	defer conn.Close()
	s := session.New(conn)
	if err := s.SendRequest(req); err != nil {
		h.t.Fatal(err)
	}
	s.Writer(session.FrameStdin).Write([]byte(stdin))
	s.Write(session.FrameEOF, nil)
	var out bytes.Buffer
	for {
		typ, p, err := s.Read()
		if err != nil {
			h.t.Fatalf("session: %v (output so far %q)", err, out.String())
		}
		switch typ {
		case session.FrameStdout, session.FrameStderr:
			out.Write(p)
		case session.FrameExit:
			code, msg := session.ParseExit(p)
			return out.String(), code, msg
		}
	}
}

func TestEnterAndVolumeCopies(t *testing.T) {
	h := newHarness(t)
	h.join("w1")
	h.waitReady(2)
	undo := h.onlyOn("w1")
	h.deployWith("db", 1, h.withVolume("db", "data"))
	undo()
	in := h.web1("db")
	if in.Node != "w1" {
		t.Fatalf("web.1 on %s", in.Node)
	}

	// A command in the VM on w1, through the control node and w1's agent,
	// which added the VM's token (the fake guest checks it).
	out, code, msg := h.run("db", "", session.Request{Op: session.OpExec, Argv: []string{"cat"}, Root: true}, "hello from stdin")
	if code != 0 || msg != "" || !strings.Contains(out, "ran [cat] root=true") || !strings.Contains(out, "hello from stdin") {
		t.Fatalf("exec: %q, %d %q", out, code, msg)
	}
	if _, code, _ := h.run("db", "web.1", session.Request{Op: session.OpExec, Argv: []string{"exit", "3"}}, ""); code != 3 {
		t.Errorf("exit status %d, want 3", code)
	}
	// Only the session's own fields reach the guest: a client can't ask an
	// enter session to restore files.
	out, code, _ = h.run("db", "web", session.Request{Op: session.OpImport, Path: "/data"}, "")
	if code != 0 || !strings.Contains(out, "ran []") {
		t.Errorf("an enter session ran %q (status %d), want a plain exec", out, code)
	}
	if _, err := h.client.Enter(h.ctx, "db", "worker"); err == nil || !strings.Contains(err.Error(), "no running worker") {
		t.Errorf("unknown process: %v", err)
	}

	// Export: the files of the volume, as the instance mounting it sees them.
	guest := filepath.Join(h.nodes["w1"].dir, "guests", in.ID, "data")
	os.MkdirAll(filepath.Join(guest, "sub"), 0o755)
	os.WriteFile(filepath.Join(guest, "kuma.db"), []byte("v1"), 0o640)
	os.WriteFile(filepath.Join(guest, "sub", "x"), []byte("x"), 0o600)
	var archive bytes.Buffer
	if err := h.client.ExportVolume(h.ctx, "db", "data", false, &archive); err != nil {
		t.Fatal(err)
	}
	got := t.TempDir()
	if err := session.Extract(bytes.NewReader(archive.Bytes()), got, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(got, "kuma.db")); string(b) != "v1" {
		t.Errorf("exported kuma.db = %q", b)
	}

	// Import: back into the volume (cleared first), and the app restarts.
	os.WriteFile(filepath.Join(got, "kuma.db"), []byte("v2"), 0o640)
	os.WriteFile(filepath.Join(guest, "stale"), []byte("old"), 0o644)
	var edited bytes.Buffer
	if err := session.Archive(got, &edited); err != nil {
		t.Fatal(err)
	}
	if err := h.client.ImportVolume(h.ctx, "db", "data", true, &edited); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(guest, "kuma.db")); string(b) != "v2" {
		t.Errorf("imported kuma.db = %q", b)
	}
	if _, err := os.Stat(filepath.Join(guest, "stale")); !os.IsNotExist(err) {
		t.Error("--clear left a file that wasn't in the archive")
	}
	h.nodes["w1"].rt.mu.Lock()
	restarts := h.nodes["w1"].rt.restarts[in.ID]
	h.nodes["w1"].rt.mu.Unlock()
	if restarts != 1 {
		t.Errorf("the app restarted %d times after the import, want 1", restarts)
	}
	if err := h.client.ImportVolume(h.ctx, "db", "data", false, strings.NewReader("not an archive at all, really")); err == nil {
		t.Error("garbage was imported")
	}

	// Every session is in the events.
	events, _ := h.st.Events(h.ctx, "db", 20)
	var all []string
	for _, e := range events {
		all = append(all, e.Message)
	}
	for _, want := range []string{"tester ran cat in web.1", "tester exported volume data", "tester restored volume data"} {
		if !strings.Contains(strings.Join(all, "\n"), want) {
			t.Errorf("no event %q in %v", want, all)
		}
	}

	// A stopped app has no instance to go through.
	h.ps("db", "stop")
	if err := h.client.ExportVolume(h.ctx, "db", "data", false, io.Discard); err == nil || !strings.Contains(err.Error(), "not mounted in a running instance") {
		t.Errorf("export of a stopped app: %v", err)
	}
	if _, err := h.client.Enter(h.ctx, "db", ""); err == nil || !strings.Contains(err.Error(), "is stopped") {
		t.Errorf("enter into a stopped app: %v", err)
	}
}
