package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wes/jokku/internal/types"
)

// proxyUnit is the service whose output carries the proxy's access log.
const proxyUnit = "jokku-proxy.service"

// accessEntry is the part of a Caddy access log line Jokku reads.
type accessEntry struct {
	Logger  string  `json:"logger"`
	TS      float64 `json:"ts"`
	Request struct {
		ClientIP string `json:"client_ip"`
		Proto    string `json:"proto"`
		Method   string `json:"method"`
		Host     string `json:"host"`
		URI      string `json:"uri"`
	} `json:"request"`
	Duration float64 `json:"duration"` // seconds
	Size     int64   `json:"size"`
	Status   int     `json:"status"`
	App      string  `json:"jokku_app"`
	Upstream string  `json:"jokku_upstream"`
}

// ParseAccess turns one proxy log line into a Request.
func ParseAccess(line string) (types.Request, bool) {
	var e accessEntry
	if json.Unmarshal([]byte(line), &e) != nil || len(e.Logger) < 15 || e.Logger[:15] != "http.log.access" {
		return types.Request{}, false
	}
	sec, frac := math.Modf(e.TS)
	return types.Request{
		At: time.Unix(int64(sec), int64(frac*1e9)).UTC(), App: e.App, Upstream: e.Upstream,
		Method: e.Request.Method, Host: e.Request.Host, Path: e.Request.URI, Proto: e.Request.Proto,
		Status: e.Status, DurationMS: e.Duration * 1000, Bytes: e.Size, Client: e.Request.ClientIP,
	}, true
}

// Requests streams the requests this node's proxy handled, optionally only
// one app's: the last tail of them, then new ones while following.
func Requests(ctx context.Context, app string, tail int, follow bool, fn func(types.Request)) error {
	err := requests(ctx, app, tail, follow, true, fn)
	if errors.Is(err, errNoGrep) {
		// journalctl without pattern matching: filter here instead (the tail
		// then counts every app's requests).
		return requests(ctx, app, tail, follow, false, fn)
	}
	return err
}

var errNoGrep = errors.New("journalctl has no --grep")

func requests(ctx context.Context, app string, tail int, follow, grep bool, fn func(types.Request)) error {
	args := []string{"--no-pager", "--output=json", "--lines=" + strconv.Itoa(tail), "--unit=" + proxyUnit}
	if app != "" && grep {
		args = append(args, "--grep=\"jokku_app\":\""+regexp.QuoteMeta(app)+"\"")
	}
	if follow {
		args = append(args, "--follow")
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	seen := false
	err = forEachEntry(out, func(e journalEntry) {
		seen = true
		if r, ok := ParseAccess(e.message()); ok && (app == "" || r.App == app) {
			fn(r)
		}
	})
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return nil
	}
	if werr != nil && !seen && strings.Contains(stderr.String(), "pattern") {
		return errNoGrep
	}
	return err
}
