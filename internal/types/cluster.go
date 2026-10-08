package types

import "time"

// ProtocolVersion is the control <-> agent protocol. The control node rejects
// agents speaking another version, with a message to update.
const ProtocolVersion = 1

// NodeState is everything one node's agent needs, computed by the control
// node from the store. Agents long-poll for it and cache it, so a node keeps
// running its last known state while the control node is unreachable.
type NodeState struct {
	ETag      string         `json:"etag"` // changes whenever anything below changes
	Version   string         `json:"version"`
	Node      NodeIdentity   `json:"node"`
	Peers     []Peer         `json:"peers"`
	Instances []InstanceSpec `json:"instances"`
	Proxy     *ProxyState    `json:"proxy,omitempty"` // nil on non-ingress nodes
}

type NodeIdentity struct {
	Name        string `json:"name"`
	Subnet      string `json:"subnet"`  // e.g. 10.210.2.0/24
	MeshIP      string `json:"mesh_ip"` // e.g. 10.210.2.1
	ClusterCIDR string `json:"cluster_cidr"`
}

// Peer is another node on the WireGuard mesh.
type Peer struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
	Endpoint  string `json:"endpoint"` // host:port
	Subnet    string `json:"subnet"`
	MeshIP    string `json:"mesh_ip"`
}

// InstanceSpec is one microVM this node should hold. Run is false while the
// app is stopped (the VM is kept off, its files kept). Instances not listed
// are stopped and removed.
type InstanceSpec struct {
	ID          string `json:"id"`
	App         string `json:"app"`
	Process     string `json:"process"` // web.1
	ProcessType string `json:"process_type"`
	Run         bool   `json:"run"`

	Artifact       string `json:"artifact"` // file name under artifacts/
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
	ArtifactSize   int64  `json:"artifact_size,omitempty"`

	IP       string `json:"ip"`
	Port     int    `json:"port"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`

	Argv     []string `json:"argv"`
	Env      []string `json:"env"`
	User     string   `json:"user,omitempty"`
	WorkDir  string   `json:"workdir,omitempty"`
	Hostname string   `json:"hostname"`

	// MaxRestarts is how often a crashed instance is restarted: -1 for
	// always, 0 for never.
	MaxRestarts int `json:"max_restarts"`
}

type ProxyState struct {
	Routes []ProxyRoute `json:"routes"`
	Email  string       `json:"email,omitempty"`
}

type ProxyRoute struct {
	App       string   `json:"app"`
	Hosts     []string `json:"hosts"`
	Upstreams []string `json:"upstreams"`
	TLS       bool     `json:"tls"`
}

// NodeStatus is what an agent reports every few seconds.
type NodeStatus struct {
	Protocol  int              `json:"protocol"`
	Version   string           `json:"version"`
	ETag      string           `json:"etag"`              // the state it is applying
	CanRun    string           `json:"can_run,omitempty"` // why microVMs cannot run here; empty if they can
	Metrics   NodeMetrics      `json:"metrics"`
	Instances []InstanceStatus `json:"instances"`
}

type NodeMetrics struct {
	CPUs         int     `json:"cpus"`
	CPUPercent   float64 `json:"cpu_percent"` // whole machine, 0-100
	Load1        float64 `json:"load1"`
	MemoryMB     int     `json:"memory_mb"`
	MemoryUsedMB int     `json:"memory_used_mb"`
	DiskMB       int     `json:"disk_mb"`
	DiskFreeMB   int     `json:"disk_free_mb"`
}

type InstanceStatus struct {
	ID          string    `json:"id"`
	State       string    `json:"state"` // pulling | starting | healthy | crashed | failed | stopped
	HealthyOnce bool      `json:"healthy_once"`
	Restarts    int       `json:"restarts"`
	StartedAt   time.Time `json:"started_at"`
	CPUPercent  float64   `json:"cpu_percent"` // of one vCPU, so up to 100 * vCPUs
	MemoryMB    int       `json:"memory_mb"`
}

// JoinRequest is sent by a new node to the control node.
type JoinRequest struct {
	Protocol  int    `json:"protocol"`
	Version   string `json:"version"`
	Token     string `json:"token"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"` // WireGuard
	Endpoint  string `json:"endpoint"`   // host:port other nodes reach its WireGuard on
	Arch      string `json:"arch"`
	CPUs      int    `json:"cpus"`
	MemoryMB  int    `json:"memory_mb"`
}

// JoinResponse gives the new node its identity and credentials.
type JoinResponse struct {
	Node       NodeIdentity `json:"node"`
	NodeToken  string       `json:"node_token"`  // authenticates the agent to the control node
	AgentToken string       `json:"agent_token"` // authenticates the control node to the agent
}

type JoinToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Command   string    `json:"command"` // the one-liner to run on the new server
}

type CreateJoinTokenRequest struct {
	TTLSeconds int  `json:"ttl_seconds,omitempty"` // default one hour
	Reusable   bool `json:"reusable,omitempty"`    // for autoscaling groups
}

// ClusterEvent is something that happened: a deploy, a crash, a node going
// down.
type ClusterEvent struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"` // deploy, instance, node
	App     string    `json:"app,omitempty"`
	Node    string    `json:"node,omitempty"`
	Message string    `json:"message"`
}

// ClusterStatus is the whole picture, for "jokku top" and dashboards.
type ClusterStatus struct {
	Version   string         `json:"version"`
	Control   string         `json:"control"`
	Nodes     []NodeView     `json:"nodes"`
	Apps      []AppView      `json:"apps"`
	Instances []InstanceView `json:"instances"`
	Events    []ClusterEvent `json:"events"`
	At        time.Time      `json:"at"`
	Totals    ClusterTotals  `json:"totals"`
}

type ClusterTotals struct {
	NodesReady       int `json:"nodes_ready"`
	Nodes            int `json:"nodes"`
	Apps             int `json:"apps"`
	InstancesHealthy int `json:"instances_healthy"`
	Instances        int `json:"instances"`
}

type NodeView struct {
	Node
	Version     string      `json:"version"`
	CanRun      string      `json:"can_run,omitempty"`
	Metrics     NodeMetrics `json:"metrics"`
	AllocatedMB int         `json:"allocated_mb"` // memory promised to instances
	Instances   int         `json:"instances"`
}

type AppView struct {
	Name      string    `json:"name"`
	Release   int       `json:"release"`
	Stopped   bool      `json:"stopped"`
	Locked    bool      `json:"locked"`
	Domains   []string  `json:"domains"`
	Healthy   int       `json:"healthy"`
	Wanted    int       `json:"wanted"`
	Deploy    *Deploy   `json:"deploy,omitempty"` // most recent
	CreatedAt time.Time `json:"created_at"`
}

type InstanceView struct {
	Instance
	App        string  `json:"app"`
	CPUPercent float64 `json:"cpu_percent"`
	MemoryUsed int     `json:"memory_used_mb"`
}

// Request is one HTTP request handled by a node's proxy: the router lines
// in "jokku logs" and the traffic view in "jokku top".
type Request struct {
	At           time.Time `json:"at"`
	App          string    `json:"app,omitempty"`  // empty for hosts no app serves
	Node         string    `json:"node,omitempty"` // the node whose proxy received it
	Instance     string    `json:"instance,omitempty"`
	InstanceNode string    `json:"instance_node,omitempty"`
	Upstream     string    `json:"upstream,omitempty"` // IP:port that answered
	Method       string    `json:"method"`
	Host         string    `json:"host"`
	Path         string    `json:"path"`
	Proto        string    `json:"proto,omitempty"`
	Status       int       `json:"status"`
	DurationMS   float64   `json:"duration_ms"`
	Bytes        int64     `json:"bytes"`
	Client       string    `json:"client,omitempty"`
}
