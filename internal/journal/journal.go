// Package journal reads app output from the systemd journal, where every
// microVM's console lands tagged with JOKKU_APP, JOKKU_PROCESS and
// JOKKU_INSTANCE.
package journal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wes/jokku/internal/types"
)

type journalEntry struct {
	Message   json.RawMessage `json:"MESSAGE"`
	Process   string          `json:"JOKKU_PROCESS"`
	Timestamp string          `json:"__REALTIME_TIMESTAMP"`
}

// format renders "2026-10-07T20:00:00.123456Z app[web.1]: message", the
// format "dokku logs" uses.
func (e journalEntry) format() string {
	us, _ := strconv.ParseInt(e.Timestamp, 10, 64)
	ts := time.UnixMicro(us).UTC().Format("2006-01-02T15:04:05.000000Z")
	return ts + " app[" + e.Process + "]: " + strings.TrimRight(e.message(), "\r\n")
}

// message decodes MESSAGE, which journald emits as a string, or as an array of
// bytes when it is not valid UTF-8.
func (e journalEntry) message() string {
	var s string
	if json.Unmarshal(e.Message, &s) == nil {
		return s
	}
	var b []byte
	if json.Unmarshal(e.Message, &b) == nil {
		return string(b)
	}
	var ints []int
	if json.Unmarshal(e.Message, &ints) == nil {
		buf := make([]byte, len(ints))
		for i, n := range ints {
			buf[i] = byte(n)
		}
		return string(buf)
	}
	return ""
}

func forEachEntry(r io.Reader, fn func(journalEntry)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e journalEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			fn(e)
		}
	}
	return sc.Err()
}

// Logs streams an app's log lines, formatted the way "dokku logs" prints them.
func Logs(ctx context.Context, app string, o types.LogOptions, line func(string)) error {
	// _TRANSPORT=stdout keeps only what the VM printed, not systemd's own
	// messages about the unit.
	args := []string{"--no-pager", "--output=json", "--lines=" + strconv.Itoa(o.Tail), "JOKKU_APP=" + app, "_TRANSPORT=stdout"}
	if o.Follow {
		args = append(args, "--follow")
	}
	switch {
	case strings.Contains(o.Process, "."):
		args = append(args, "JOKKU_PROCESS="+o.Process)
	case o.Process != "":
		args = append(args, "JOKKU_PROCESS_TYPE="+o.Process)
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	err = forEachEntry(out, func(e journalEntry) {
		line(e.format())
	})
	cmd.Wait()
	if ctx.Err() != nil {
		return nil // the client went away while following
	}
	return err
}

// InstanceLogs returns the last n lines an instance printed, for deploy
// failure messages.
func InstanceLogs(ctx context.Context, id string, n int) []string {
	out, _ := exec.CommandContext(ctx, "journalctl", "--no-pager", "--output=cat", "--lines="+strconv.Itoa(n), "JOKKU_INSTANCE="+id, "_TRANSPORT=stdout").Output()
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// Serve answers a log request from this node's journal. Agents serve it on
// their API; the control node calls it directly for its own instances.
func Serve(ctx context.Context, path string, q url.Values, line func(string)) error {
	tail, _ := strconv.Atoi(q.Get("tail"))
	switch path {
	case "/v1/logs":
		o := types.LogOptions{Tail: tail, Follow: q.Get("follow") == "true", Process: q.Get("process")}
		return Logs(ctx, q.Get("app"), o, line)
	case "/v1/instance-logs":
		for _, l := range InstanceLogs(ctx, q.Get("id"), tail) {
			line(l)
		}
		return nil
	}
	return fmt.Errorf("unknown log request %s", path)
}
