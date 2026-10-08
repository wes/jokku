package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wes/jokku/internal/types"
)

// Remote is the control plane as seen from a worker: the control node's API
// over HTTPS, with its TLS key pinned and this node's token.
type Remote struct {
	base   string // https://host:7443
	token  string
	client *http.Client
}

func NewRemote(base, token string, tlsConfig *tls.Config) *Remote {
	return &Remote{base: strings.TrimSuffix(base, "/"), token: token, client: &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig, ResponseHeaderTimeout: 60 * time.Second, IdleConnTimeout: 90 * time.Second},
	}}
}

func (r *Remote) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		resp.Body.Close()
		return nil, ErrRemoved
	case resp.StatusCode >= 300:
		defer resp.Body.Close()
		var e types.Error
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return nil, fmt.Errorf("control node: %s: %s", resp.Status, e.Error)
	}
	return resp, nil
}

func (r *Remote) State(ctx context.Context, etag string) (*types.NodeState, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second) // the poll itself lasts up to 25s
	defer cancel()
	resp, err := r.do(ctx, http.MethodGet, "/v1/agent/state?etag="+url.QueryEscape(etag), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var st types.NodeState
	return &st, json.NewDecoder(resp.Body).Decode(&st)
}

func (r *Remote) Report(ctx context.Context, st *types.NodeStatus) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := r.do(ctx, http.MethodPost, "/v1/agent/status", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (r *Remote) Artifact(ctx context.Context, name string, w io.Writer) error {
	resp, err := r.do(ctx, http.MethodGet, "/v1/agent/artifacts/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}
