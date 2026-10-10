package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/version"
)

// Edges are public machines that receive the traffic for the cluster's
// domains and send it over the mesh to the nodes behind them, which may sit
// behind NAT with no port open: a home lab, say. Joining works the other
// way round from a worker's, for that reason: the control node can't be
// reached, so it makes the edge's identity, credentials and WireGuard key
// itself, and hands them over in the command that installs the edge. The
// nodes then dial the edge, and the edge's agent reaches the control node
// over the mesh.

const bundlePrefix = "JOKKUEDGE1."

// meshPort is WireGuard's port on an edge unless its address says
// otherwise.
const meshPort = 51820

// EdgeBundle is everything an edge needs to come up: who it is, its
// credentials, and the control node as its first peer, so the mesh is up
// before it has heard anything.
type EdgeBundle struct {
	Version    string             `json:"version"`
	Control    string             `json:"control"` // the control node's API over the mesh, host:port
	Pin        string             `json:"pin"`
	NodeToken  string             `json:"node_token"`
	AgentToken string             `json:"agent_token"`
	Node       types.NodeIdentity `json:"node"`
	PrivateKey string             `json:"private_key"` // WireGuard
	Peers      []types.Peer       `json:"peers"`
}

func (b *EdgeBundle) Encode() string {
	j, _ := json.Marshal(b)
	return bundlePrefix + base64.RawURLEncoding.EncodeToString(j)
}

// ParseEdgeBundle reads what Encode wrote.
func ParseEdgeBundle(s string) (*EdgeBundle, error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(s), bundlePrefix)
	if !ok {
		return nil, errors.New("malformed edge bundle; copy the command again from jokku edge:add")
	}
	j, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("malformed edge bundle; copy the command again from jokku edge:add")
	}
	var b EdgeBundle
	if err := json.Unmarshal(j, &b); err != nil || b.NodeToken == "" || b.PrivateKey == "" || len(b.Peers) == 0 {
		return nil, errors.New("malformed edge bundle; copy the command again from jokku edge:add")
	}
	return &b, nil
}

// CreateEdge adds an edge at address (its public IP or DNS name, with the
// WireGuard port if not 51820) and returns the command that installs it.
func (c *Controller) CreateEdge(ctx context.Context, name, address string) (*types.Edge, error) {
	if !nodeNameRe.MatchString(name) {
		return nil, fmt.Errorf("edge name %q is invalid: use lowercase letters, digits and dashes", name)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host, port = address, strconv.Itoa(meshPort)
	}
	if host == "" || strings.ContainsAny(host, "/ ") {
		return nil, fmt.Errorf("invalid address %q: give the edge's public IP or DNS name", address)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("invalid WireGuard port %q", port)
	}
	control, err := c.Store.Node(ctx, c.Self)
	if err != nil {
		return nil, err
	}
	if control.WGPublicKey == "" {
		return nil, errors.New("the control node has no WireGuard key yet; is WireGuard available in its kernel?")
	}
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	nodeToken, agentToken := randomToken(32), randomToken(32)
	n := &store.Node{
		Name: name, Role: store.RoleEdge, Address: host, Version: c.Version,
		WGPublicKey: key.PublicKey().String(), WGEndpoint: net.JoinHostPort(host, port),
		TokenHash: HashToken(nodeToken), AgentToken: agentToken,
	}
	if err := c.Store.CreateNode(ctx, n); err != nil {
		return nil, err
	}
	c.Store.AddEvent(ctx, "node", "", name, "edge %s added at %s; waiting for it to be installed", name, host)
	c.Changed()

	subnet, meshIP := NodeSubnet(c.ClusterCIDR, n.SubnetIndex)
	csub, cip := NodeSubnet(c.ClusterCIDR, control.SubnetIndex)
	b := &EdgeBundle{
		Version: c.Version, Control: net.JoinHostPort(cip.String(), strconv.Itoa(APIPort)), Pin: c.Pin,
		NodeToken: nodeToken, AgentToken: agentToken, PrivateKey: key.String(),
		Node:  types.NodeIdentity{Name: name, Role: store.RoleEdge, Subnet: subnet.String(), MeshIP: meshIP.String(), ClusterCIDR: c.ClusterCIDR.String()},
		Peers: []types.Peer{{Name: control.Name, PublicKey: control.WGPublicKey, Subnet: csub.String(), MeshIP: cip.String()}},
	}
	edge := &types.Edge{Node: c.NodeInfo(*n, time.Now()), Version: c.Version, Bundle: b.Encode()}
	edge.Command = c.edgeCommand(edge.Bundle)
	return edge, nil
}

// edgeCommand installs an edge: the same installer as every server, at the
// control node's version.
func (c *Controller) edgeCommand(bundle string) string {
	env := ""
	if strings.HasPrefix(c.Version, "v") {
		env = "JOKKU_VERSION=" + c.Version + " "
	}
	return fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/%s/main/install.sh | sudo %ssh -s -- --edge %s",
		version.Repo, env, bundle)
}
