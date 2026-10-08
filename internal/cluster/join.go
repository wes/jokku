package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/version"
)

// APIPort is the control node's TCP port for joins and agents.
const APIPort = 7443

// AgentPort is each node's agent API (log streams), reachable only over the
// mesh.
const AgentPort = 7444

const tokenPrefix = "JOKKU1"

var nodeNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// LoadIdentity loads (or creates, on first start) the control node's TLS key
// and self-signed certificate, and returns the pin: the base64url SHA-256 of
// its public key. Nodes pin that key instead of trusting a CA, so the
// certificate's names and the address used don't matter.
func LoadIdentity(dataDir string) (tls.Certificate, string, error) {
	dir := filepath.Join(dataDir, "tls")
	keyPath, certPath := filepath.Join(dir, "control.key"), filepath.Join(dir, "control.crt")
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return tls.Certificate{}, "", err
		}
		return cert, pinOf(leaf.RawSubjectPublicKeyInfo), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "jokku control node"},
		DNSNames:     []string{"jokku-control"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(30, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	return LoadIdentity(dataDir)
}

func pinOf(spki []byte) string {
	sum := sha256.Sum256(spki)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// PinnedTLS is a client TLS config that accepts only the server key matching
// pin.
func PinnedTLS(pin string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // replaced by the pin check below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the control node sent no certificate")
			}
			if got := pinOf(cs.PeerCertificates[0].RawSubjectPublicKeyInfo); got != pin {
				return errors.New("the control node's TLS key does not match the one in the join token; " +
					"check the address, or create a new token on the control node")
			}
			return nil
		},
		MinVersion: tls.VersionTLS13,
	}
}

// ParseJoinToken splits JOKKU1.<pin>.<secret>.
func ParseJoinToken(token string) (pin, secret string, err error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] != tokenPrefix || parts[1] == "" || parts[2] == "" {
		return "", "", errors.New("malformed join token; copy it again from jokku cluster:join-command")
	}
	return parts[1], parts[2], nil
}

// HashToken is how credentials are stored: never in the clear.
func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// CreateJoinToken makes a token for adding a node, and the one-liner to run
// on it.
func (c *Controller) CreateJoinToken(ctx context.Context, ttl time.Duration, reusable bool) (*types.JoinToken, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	secret := randomToken(24)
	expires := time.Now().Add(ttl).UTC().Truncate(time.Second)
	if err := c.Store.CreateJoinToken(ctx, HashToken(secret), expires, reusable); err != nil {
		return nil, err
	}
	token := tokenPrefix + "." + c.Pin + "." + secret
	env := ""
	if strings.HasPrefix(c.Version, "v") {
		env = "JOKKU_VERSION=" + c.Version + " " // the new node must run the same version
	}
	cmd := fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/%s/main/install.sh | sudo %ssh -s -- --join %s --token %s",
		version.Repo, env, c.Address, token)
	return &types.JoinToken{Token: token, ExpiresAt: expires, Command: cmd}, nil
}

// Join admits a node: it checks the token, gives the node a name, a subnet
// and credentials, and tells every node about its new peer.
func (c *Controller) Join(ctx context.Context, req types.JoinRequest) (*types.JoinResponse, error) {
	if req.Protocol != types.ProtocolVersion {
		return nil, fmt.Errorf("this node speaks protocol %d, the control node %d", req.Protocol, types.ProtocolVersion)
	}
	if req.Version != c.Version {
		return nil, fmt.Errorf("this node runs jokku %s but the control node runs %s; install the same version (JOKKU_VERSION=%s)",
			req.Version, c.Version, c.Version)
	}
	_, secret, err := ParseJoinToken(req.Token)
	if err != nil {
		return nil, err
	}
	if !nodeNameRe.MatchString(req.Name) {
		return nil, fmt.Errorf("node name %q is invalid: use lowercase letters, digits and dashes (set it with --name)", req.Name)
	}
	if key, err := base64.StdEncoding.DecodeString(req.PublicKey); err != nil || len(key) != 32 {
		return nil, errors.New("invalid WireGuard public key")
	}
	host, _, err := net.SplitHostPort(req.Endpoint)
	if err != nil || host == "" {
		return nil, fmt.Errorf("invalid WireGuard endpoint %q", req.Endpoint)
	}
	if err := c.Store.UseJoinToken(ctx, HashToken(secret)); err != nil {
		return nil, err
	}
	nodeToken, agentToken := randomToken(32), randomToken(32)
	n := &store.Node{
		Name: req.Name, Address: host, Arch: req.Arch, CPUs: req.CPUs, MemoryMB: req.MemoryMB,
		WGPublicKey: req.PublicKey, WGEndpoint: req.Endpoint, Version: req.Version,
		TokenHash: HashToken(nodeToken), AgentToken: agentToken,
	}
	if err := c.Store.CreateNode(ctx, n); err != nil {
		return nil, err
	}
	c.Store.AddEvent(ctx, "node", "", n.Name, "%s joined the cluster (%d CPUs, %d MiB)", n.Name, n.CPUs, n.MemoryMB)
	c.Changed()
	subnet, meshIP := NodeSubnet(c.ClusterCIDR, n.SubnetIndex)
	return &types.JoinResponse{
		Node:       types.NodeIdentity{Name: n.Name, Subnet: subnet.String(), MeshIP: meshIP.String(), ClusterCIDR: c.ClusterCIDR.String()},
		NodeToken:  nodeToken,
		AgentToken: agentToken,
	}, nil
}

// Authenticate maps an agent's credential to its node.
func (c *Controller) Authenticate(ctx context.Context, token string) (*store.Node, error) {
	if token == "" {
		return nil, errors.New("missing node token")
	}
	n, err := c.Store.NodeByTokenHash(ctx, HashToken(token))
	if err != nil {
		return nil, errors.New("unknown node token: this node is not (or no longer) part of the cluster")
	}
	return n, nil
}
