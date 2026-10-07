// Package client is the Go client for the Jokku API, used by the CLI and the
// git hook. It reaches the API over the local unix socket or over SSH.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/wes/jokku/internal/types"
)

// DefaultSocket is where the control node's daemon listens.
const DefaultSocket = "/run/jokku/jokku.sock"

// Target says how to reach the API. Exactly one field is set.
type Target struct {
	Socket string // path of the API unix socket
	SSH    string // [user@]host[:port], reached with "ssh ... jokku api:dial-stdio"
}

func (t Target) String() string {
	if t.SSH != "" {
		return "ssh://" + t.SSH
	}
	return t.Socket
}

type Client struct {
	target Target
	http   *http.Client
	actor  string
}

// New returns a client for target. actor is sent with every request so the
// API can record who did what (an SSH key name, for example); it may be empty.
func New(target Target, actor string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if target.SSH != "" {
				return dialSSH(ctx, target.SSH)
			}
			var d net.Dialer
			return d.DialContext(ctx, "unix", target.Socket)
		},
		MaxIdleConns: 1,
	}
	return &Client{target: target, http: &http.Client{Transport: tr}, actor: actor}
}

// Error is a non-2xx API response.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://jokku"+path, body)
	if err != nil {
		return nil, err
	}
	if c.actor != "" {
		req.Header.Set(types.HeaderActor, c.actor)
	}
	return req, nil
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the "Get http://jokku/..." noise
		}
		if c.target.SSH == "" {
			return nil, fmt.Errorf("cannot reach the jokku daemon (is it running? try: systemctl status jokku): %w", err)
		}
		return nil, fmt.Errorf("cannot reach jokku over ssh at %s: %w", c.target.SSH, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e types.Error
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
			if e.Error == "" {
				e.Error = resp.Status
			}
		}
		return nil, &Error{Status: resp.StatusCode, Message: e.Error}
	}
	return resp, nil
}

// call sends a JSON request and decodes a JSON response into out (if non-nil).
func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// stream reads NDJSON events, calling fn for each, and returns the error
// carried by the final "done" event.
func stream(r io.Reader, fn func(types.Event)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e types.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return fmt.Errorf("malformed event from server: %w", err)
		}
		if e.Type == types.EventDone {
			if e.Status != types.StatusSucceeded {
				return errors.New(e.Error)
			}
			return nil
		}
		fn(e)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("connection closed before the operation finished; it may still be running on the server")
}

// appPath builds /v1/apps/{app}/{rest}, or /v1/{rest} for the global scope
// when app is "".
func appPath(app, rest string) string {
	switch {
	case app == "":
		return "/v1/" + rest
	case rest == "":
		return "/v1/apps/" + url.PathEscape(app)
	}
	return "/v1/apps/" + url.PathEscape(app) + "/" + rest
}
