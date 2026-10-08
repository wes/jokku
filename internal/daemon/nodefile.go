package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/wes/jokku/internal/types"
)

// NodeFile is a worker's identity and credentials, written when it joins a
// cluster. Its presence is what makes the daemon run as a worker.
type NodeFile struct {
	Role       string             `json:"role"`        // "worker"
	Control    string             `json:"control"`     // the control node's API, host:7443
	Pin        string             `json:"pin"`         // its TLS key
	NodeToken  string             `json:"node_token"`  // this node's credential with the control node
	AgentToken string             `json:"agent_token"` // the control node's credential with this node
	Node       types.NodeIdentity `json:"node"`
}

func nodeFilePath(dataDir string) string { return filepath.Join(dataDir, "node.json") }

func LoadNodeFile(dataDir string) (*NodeFile, error) {
	b, err := os.ReadFile(nodeFilePath(dataDir))
	if err != nil {
		return nil, err
	}
	var nf NodeFile
	return &nf, json.Unmarshal(b, &nf)
}

func SaveNodeFile(dataDir string, nf *NodeFile) error {
	b, err := json.MarshalIndent(nf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	tmp := nodeFilePath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, nodeFilePath(dataDir))
}
