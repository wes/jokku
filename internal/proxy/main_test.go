package proxy

import (
	"bufio"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wes/jokku/internal/journal"
	"github.com/wes/jokku/internal/types"
)

// The access log goes to stderr (the journal, in production), and Caddy
// opens stderr once per process. So stderr is a pipe for the whole run, and
// tests read the requests logged while they watch.
var accessLog struct {
	mu   sync.Mutex
	subs map[chan types.Request]bool
}

func TestMain(m *testing.M) {
	r, w, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stderr := os.Stderr
	os.Stderr = w
	accessLog.subs = map[chan types.Request]bool{}
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			req, ok := journal.ParseAccess(sc.Text())
			if !ok {
				fmt.Fprintln(stderr, sc.Text()) // anything else still shows
				continue
			}
			accessLog.mu.Lock()
			for ch := range accessLog.subs {
				select {
				case ch <- req:
				default:
				}
			}
			accessLog.mu.Unlock()
		}
	}()
	code := m.Run()
	os.Stderr = stderr
	os.Exit(code)
}

// watchAccessLog collects the requests logged from now until the test ends;
// the returned func waits a moment for stragglers and returns them.
func watchAccessLog(t *testing.T) func() []types.Request {
	ch := make(chan types.Request, 100)
	accessLog.mu.Lock()
	accessLog.subs[ch] = true
	accessLog.mu.Unlock()
	stop := func() {
		accessLog.mu.Lock()
		delete(accessLog.subs, ch)
		accessLog.mu.Unlock()
	}
	t.Cleanup(stop)
	return func() []types.Request {
		var out []types.Request
		timeout := time.After(time.Second)
		for {
			select {
			case req := <-ch:
				out = append(out, req)
			case <-timeout:
				stop()
				return out
			}
		}
	}
}
