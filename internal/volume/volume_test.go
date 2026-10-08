package volume

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// owner serves the disk at path the way an agent does.
func owner(t *testing.T, path string) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	busy := &atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("final") == "1" && busy.Load() {
			http.Error(w, "busy", http.StatusConflict)
			return
		}
		have, err := ReadSummary(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		defer f.Close()
		Send(w, have, f)
	}))
	t.Cleanup(srv.Close)
	return srv, busy
}

func disk(t *testing.T, path string, size int64, blocks map[int64]byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for i, fill := range blocks {
		if _, err := f.WriteAt(bytes.Repeat([]byte{fill}, BlockSize), i*BlockSize); err != nil {
			t.Fatal(err)
		}
	}
}

func same(t *testing.T, a, b string) {
	t.Helper()
	x, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(x) != len(y) {
		t.Fatalf("sizes differ: %d and %d", len(x), len(y))
	}
	if sha256.Sum256(x) != sha256.Sum256(y) {
		t.Fatal("contents differ")
	}
}

func TestPullCopiesThenSendsOnlyChanges(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	disk(t, src, 64*BlockSize, map[int64]byte{0: 1, 5: 2, 6: 3, 40: 4})
	srv, _ := owner(t, src)
	p := &Puller{Client: srv.Client(), URL: srv.URL, Token: "secret", Path: dst}

	st, err := p.Pull(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Blocks != 4 || st.Zeroed != 0 {
		t.Fatalf("first pass sent %+v, want the 4 blocks with data", st)
	}
	same(t, src, dst)

	// The app writes on: one block changes, one is cleared, one is new.
	disk(t, src, 64*BlockSize, map[int64]byte{5: 9, 6: 0, 50: 7})
	st, err = p.Pull(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Blocks != 2 || st.Zeroed != 1 {
		t.Fatalf("second pass sent %+v, want 2 changed blocks and 1 cleared", st)
	}
	same(t, src, dst)

	// Nothing changed: nothing is sent, even by a fresh puller that has to
	// summarize the copy it finds.
	p2 := &Puller{Client: srv.Client(), URL: srv.URL, Token: "secret", Path: dst}
	if st, err := p2.Pull(context.Background(), true); err != nil || st.Blocks+st.Zeroed != 0 {
		t.Fatalf("unchanged pass: %+v, %v", st, err)
	}
}

func TestPullFollowsSizeChanges(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	disk(t, src, 8*BlockSize+123, map[int64]byte{8: 5}) // a partial last block
	srv, _ := owner(t, src)
	p := &Puller{Client: srv.Client(), URL: srv.URL, Token: "secret", Path: dst}
	if _, err := p.Pull(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	same(t, src, dst)
	if err := os.Truncate(src, 4*BlockSize); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Pull(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	same(t, src, dst)
	disk(t, src, 32*BlockSize, map[int64]byte{31: 6})
	if _, err := p.Pull(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	same(t, src, dst)
}

func TestFinalPassWaitsForTheOwner(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	disk(t, src, 4*BlockSize, map[int64]byte{1: 1})
	srv, busy := owner(t, src)
	busy.Store(true)
	p := &Puller{Client: srv.Client(), URL: srv.URL, Token: "secret", Path: filepath.Join(dir, "dst")}
	if _, err := p.Pull(context.Background(), true); !errors.Is(err, ErrBusy) {
		t.Fatalf("final pass while attached: %v, want ErrBusy", err)
	}
	p.Token = "wrong"
	if _, err := p.Pull(context.Background(), false); err == nil {
		t.Fatal("a wrong token was accepted")
	}
}

func TestReceiveRejectsCutOffAndCorruptStreams(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	disk(t, src, 4*BlockSize, map[int64]byte{0: 1, 2: 2})
	f, _ := os.Open(src)
	defer f.Close()
	var stream bytes.Buffer
	if _, err := Send(&stream, Summary{}, f); err != nil {
		t.Fatal(err)
	}
	full := stream.Bytes()

	receive := func(b []byte) error {
		dst, err := os.Create(filepath.Join(dir, "dst"))
		if err != nil {
			t.Fatal(err)
		}
		defer dst.Close()
		_, err = Receive(bytes.NewReader(b), dst, Summary{}, nil)
		return err
	}
	if err := receive(full); err != nil {
		t.Fatalf("whole stream: %v", err)
	}
	if err := receive(full[:len(full)-9]); err == nil {
		t.Error("a stream without its end marker was accepted")
	}
	corrupt := bytes.Clone(full)
	corrupt[len(streamMagic)+12+1+8+4+32+10] ^= 0xff // a byte of the first block
	if err := receive(corrupt); err == nil {
		t.Error("a corrupted block was accepted")
	}
	if err := receive([]byte("HTTP/1.1 500")); err == nil {
		t.Error("garbage was accepted")
	}
}

func TestInterruptedPassIsResumed(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	disk(t, src, 16*BlockSize, map[int64]byte{0: 1, 3: 2, 9: 3, 15: 4})
	cut := &atomic.Bool{}
	cut.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		have, _ := ReadSummary(r.Body)
		f, _ := os.Open(src)
		defer f.Close()
		if cut.Load() {
			// Deliver the header and a block and a half, then drop.
			var b bytes.Buffer
			Send(&b, have, f)
			w.Write(b.Bytes()[:len(streamMagic)+12+BlockSize*3/2])
			return
		}
		Send(w, have, f)
	}))
	defer srv.Close()
	p := &Puller{Client: srv.Client(), URL: srv.URL, Token: "x", Path: dst}
	if _, err := p.Pull(context.Background(), false); err == nil {
		t.Fatal("a cut-off pass reported success")
	}
	cut.Store(false)
	st, err := p.Pull(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Blocks != 3 {
		t.Errorf("resumed pass sent %d blocks, want the 3 that had not arrived", st.Blocks)
	}
	same(t, src, dst)
}

func TestSummaryRoundTrip(t *testing.T) {
	sum := Summary{0: sha256.Sum256([]byte("a")), 77: sha256.Sum256([]byte("b"))}
	var b bytes.Buffer
	if err := WriteSummary(&b, sum); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSummary(&b)
	if err != nil || len(got) != 2 || got[77] != sum[77] {
		t.Fatalf("round trip: %v, %v", got, err)
	}
	if _, err := ReadSummary(io.LimitReader(bytes.NewReader([]byte("JKSUM1\x00\x00\x00\x00\x00\x00\x00\x05")), 14)); err == nil {
		t.Error("a truncated summary was accepted")
	}
}
