package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/store"
)

// TestWatchAddressFindsTheNetwork checks a control node that started before
// its network (and so registered no address) advertises its address once it
// has one, and follows it when it changes: the other nodes dial its mesh
// endpoint, and joining nodes its API.
func TestWatchAddressFindsTheNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var addr atomic.Value
	addr.Store("")
	addressCheckEvery, detectAddress = 10*time.Millisecond, func() string { return addr.Load().(string) }
	st, err := store.Open(filepath.Join(t.TempDir(), "jokku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.RegisterControlNode(ctx, store.Node{Name: "jokku1"}); err != nil {
		t.Fatal(err)
	}
	ctl := cluster.New(&cluster.Controller{Store: st, Self: "jokku1", Address: ":7443", Pin: "pin", Version: "test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	go watchAddress(ctx, st, ctl, "jokku1", "", ctl.Log)

	advertised := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			n, err := st.Node(ctx, "jokku1")
			if err != nil {
				t.Fatal(err)
			}
			if n.Address == want && n.WGEndpoint == want+":51820" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("advertising %q (endpoint %q), want %q", n.Address, n.WGEndpoint, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
		tok, err := ctl.CreateJoinToken(ctx, time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(tok.Command, " --join "+want+":7443 ") {
			t.Errorf("join command: %s", tok.Command)
		}
	}

	time.Sleep(50 * time.Millisecond)
	if n, _ := st.Node(ctx, "jokku1"); n.Address != "" || n.WGEndpoint != "" {
		t.Errorf("with no network: %q, %q", n.Address, n.WGEndpoint)
	}
	addr.Store("192.168.86.113")
	advertised("192.168.86.113")
	addr.Store("") // a moment without a route changes nothing
	time.Sleep(50 * time.Millisecond)
	advertised("192.168.86.113")
	addr.Store("192.168.86.120")
	advertised("192.168.86.120")
}
