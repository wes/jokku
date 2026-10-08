package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/certmagic"
)

func init() {
	caddy.RegisterModule(ClusterStorage{})
}

// ClusterStorage is a Caddy storage module that keeps certificates, ACME
// accounts and challenge tokens on the control node, so every ingress node
// shares them: any node can answer an HTTP-01 challenge for a certificate
// another node asked for, and each certificate is issued once.
//
// Everything read or written is mirrored to a local directory. If the control
// node is unreachable, reads fall back to that copy, so a restarted proxy
// keeps serving the certificates it had. The control node stays the truth: a
// key it doesn't have is dropped from the mirror, never served from it.
//
// Legacy is the file storage a single server used before it joined a
// cluster; certificates found only there are moved to the control node.
type ClusterStorage struct {
	URL    string `json:"url"`    // the control node's API, https://host:7443
	Token  string `json:"token"`  // this node's token
	Pin    string `json:"pin"`    // the control node's TLS key pin
	Cache  string `json:"cache"`  // local mirror directory
	Legacy string `json:"legacy"` // pre-cluster local storage, optional

	client *http.Client
	local  *certmagic.FileStorage
	legacy *certmagic.FileStorage
	owner  string
}

func (ClusterStorage) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "caddy.storage.jokku", New: func() caddy.Module { return new(ClusterStorage) }}
}

func (s *ClusterStorage) Provision(caddy.Context) error {
	pin := s.Pin
	s.client = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // replaced by the pin check
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || spkiPin(cs.PeerCertificates[0]) != pin {
				return errors.New("the control node's TLS key does not match")
			}
			return nil
		},
	}}}
	s.local = &certmagic.FileStorage{Path: s.Cache}
	if s.Legacy != "" {
		s.legacy = &certmagic.FileStorage{Path: s.Legacy}
	}
	// Unique per proxy: cloned servers often share a hostname.
	host, _ := os.Hostname()
	b := make([]byte, 8)
	rand.Read(b)
	s.owner = host + "-" + hex.EncodeToString(b)
	return nil
}

func spkiPin(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *ClusterStorage) CertMagicStorage() (certmagic.Storage, error) { return s, nil }

// errUnreachable marks failures to reach the control node (as opposed to its
// answers), which fall back to the local mirror.
var errUnreachable = errors.New("control node unreachable")

func (s *ClusterStorage) call(ctx context.Context, method, path string, q url.Values, body []byte) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(s.URL, "/")+path+"?"+q.Encode(), r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", errUnreachable, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", errUnreachable, err)
	}
	if resp.StatusCode >= 500 {
		return nil, resp.StatusCode, fmt.Errorf("%w: %s", errUnreachable, resp.Status)
	}
	return b, resp.StatusCode, nil
}

func (s *ClusterStorage) Store(ctx context.Context, key string, value []byte) error {
	_, code, err := s.call(ctx, http.MethodPut, "/v1/agent/certs", url.Values{"key": {key}}, value)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent {
		return fmt.Errorf("storing %s: HTTP %d", key, code)
	}
	return s.local.Store(ctx, key, value)
}

func (s *ClusterStorage) Load(ctx context.Context, key string) ([]byte, error) {
	b, code, err := s.call(ctx, http.MethodGet, "/v1/agent/certs", url.Values{"key": {key}}, nil)
	switch {
	case errors.Is(err, errUnreachable):
		return s.local.Load(ctx, key)
	case err != nil:
		return nil, err
	case code == http.StatusNotFound:
		s.local.Delete(ctx, key) // deleted elsewhere: forget the stale copy
		if v, ok := s.promote(ctx, key); ok {
			return v, nil
		}
		return nil, fs.ErrNotExist
	case code != http.StatusOK:
		return nil, fmt.Errorf("loading %s: HTTP %d", key, code)
	}
	s.local.Store(ctx, key, b)
	return b, nil
}

// promote moves a key that only exists in the pre-cluster storage to the
// control node, once.
func (s *ClusterStorage) promote(ctx context.Context, key string) ([]byte, bool) {
	if s.legacy == nil {
		return nil, false
	}
	v, err := s.legacy.Load(ctx, key)
	if err != nil {
		return nil, false
	}
	if _, code, err := s.call(ctx, http.MethodPut, "/v1/agent/certs", url.Values{"key": {key}}, v); err != nil || code != http.StatusNoContent {
		return v, true // serve it; promotion retries next time
	}
	s.local.Store(ctx, key, v)
	s.legacy.Delete(ctx, key)
	return v, true
}

func (s *ClusterStorage) Delete(ctx context.Context, key string) error {
	_, code, err := s.call(ctx, http.MethodDelete, "/v1/agent/certs", url.Values{"key": {key}}, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent {
		return fmt.Errorf("deleting %s: HTTP %d", key, code)
	}
	s.local.Delete(ctx, key)
	return nil
}

func (s *ClusterStorage) Exists(ctx context.Context, key string) bool {
	_, err := s.Stat(ctx, key)
	return err == nil
}

func (s *ClusterStorage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	b, code, err := s.call(ctx, http.MethodGet, "/v1/agent/certs/list",
		url.Values{"prefix": {prefix}, "recursive": {strconv.FormatBool(recursive)}}, nil)
	if errors.Is(err, errUnreachable) {
		return s.local.List(ctx, prefix, recursive)
	}
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("listing %s: HTTP %d", prefix, code)
	}
	var keys []string
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fs.ErrNotExist
	}
	return keys, nil
}

func (s *ClusterStorage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	b, code, err := s.call(ctx, http.MethodGet, "/v1/agent/certs/stat", url.Values{"key": {key}}, nil)
	switch {
	case errors.Is(err, errUnreachable):
		return s.local.Stat(ctx, key)
	case err != nil:
		return certmagic.KeyInfo{}, err
	case code == http.StatusNotFound:
		if s.legacy != nil {
			if info, lerr := s.legacy.Stat(ctx, key); lerr == nil {
				return info, nil // Load will promote it
			}
		}
		return certmagic.KeyInfo{}, fs.ErrNotExist
	case code != http.StatusOK:
		return certmagic.KeyInfo{}, fmt.Errorf("stat %s: HTTP %d", key, code)
	}
	var info certmagic.KeyInfo
	return info, json.Unmarshal(b, &info)
}

// Lock waits for a cluster-wide lock, so only one node at a time obtains or
// renews a given certificate.
func (s *ClusterStorage) Lock(ctx context.Context, name string) error {
	for {
		_, code, err := s.call(ctx, http.MethodPost, "/v1/agent/certs/lock", url.Values{"key": {name}, "owner": {s.owner}}, nil)
		if err != nil {
			return err
		}
		if code == http.StatusNoContent {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (s *ClusterStorage) Unlock(ctx context.Context, name string) error {
	_, _, err := s.call(ctx, http.MethodDelete, "/v1/agent/certs/lock", url.Values{"key": {name}, "owner": {s.owner}}, nil)
	return err
}

var (
	_ caddy.Provisioner      = (*ClusterStorage)(nil)
	_ caddy.StorageConverter = (*ClusterStorage)(nil)
	_ certmagic.Storage      = (*ClusterStorage)(nil)
)
