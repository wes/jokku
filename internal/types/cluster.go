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
	Volumes   []VolumeSpec   `json:"volumes,omitempty"`
	Proxy     *ProxyState    `json:"proxy,omitempty"` // nil on non-ingress nodes
	// DNS is the cluster's internal names (<process>.<app>.internal,
	// <app>.internal) and their instances' addresses.
	DNS map[string][]string `json:"dns,omitempty"`
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
	// AgentAddr is its agent API, host:port, where nodes ask each other
	// whether they still hear from the control node.
	AgentAddr string `json:"agent_addr,omitempty"`
}

// Contact is what a node's agent says about its contact with the control
// node.
type Contact struct {
	// HeardAgo is how long since it last heard from the control node; -1 if
	// it hasn't since it started.
	HeardAgoSeconds float64 `json:"heard_ago_seconds"`
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

	// Check is how the instance proves it is up: CheckTCP (it accepts
	// connections on Port) or CheckUp (it stays up a few seconds). Empty is
	// the original rule: tcp for the web process, up for the others.
	Check string `json:"check,omitempty"`
	// StopTimeout is the seconds the app gets after StopSignal (default
	// SIGTERM) before it is killed; 0 means the default, 10.
	StopTimeout int    `json:"stop_timeout,omitempty"`
	StopSignal  string `json:"stop_signal,omitempty"`

	// Volumes are mounted into the VM. Each is listed in the node's Volumes
	// as owned by it; the instance waits while one is still being copied
	// here.
	Volumes []InstanceVolume `json:"volumes,omitempty"`
}

const (
	CheckTCP = "tcp"
	CheckUp  = "up"
)

// InstanceVolume mounts a volume at Path in an instance.
type InstanceVolume struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// VolumeSpec is a local volume's disk this node holds, receives or deletes.
// Disks are deleted only on an explicit VolumeDestroy, never because a
// volume stopped being listed.
type VolumeSpec struct {
	ID     string `json:"id"`
	App    string `json:"app"`
	Name   string `json:"name"`
	SizeMB int    `json:"size_mb"`
	Role   string `json:"role"` // owner | incoming | previous | destroy

	// Create lets the owner make the disk when it has none: the volume has
	// never held data. Without it a missing disk is an error, not an empty
	// volume.
	Create bool `json:"create,omitempty"`
	// Token authorizes a move: the owner serves the disk to whoever presents
	// it, and the incoming node presents it to From (the owner's agent API,
	// host:port).
	Token string `json:"token,omitempty"`
	From  string `json:"from,omitempty"`
	// Restore, with role incoming, or owner when the disk is restored on
	// its own node, says to make the disk from a backup instead.
	Restore *RestoreSpec `json:"restore,omitempty"`
	// AutoRestore says the volume would be restored onto another node if
	// this one were cut off for long: so this node stops the instances
	// using it when it is, rather than let two copies of the app write.
	AutoRestore bool `json:"auto_restore,omitempty"`
}

// RestoreSpec is a backup to make a volume's disk from.
type RestoreSpec struct {
	Destination BackupDestination `json:"destination"`
	Path        string            `json:"path"`
	Backup      string            `json:"backup"`
	Key         string            `json:"key,omitempty"`
	// Swap, for a restore on the disk's own node, says the instances using
	// the disk were stopped: the restored copy can replace it.
	Swap bool `json:"swap,omitempty"`
}

// BackupJob asks the node holding a volume's disk to back it up.
type BackupJob struct {
	Volume      string            `json:"volume"` // the volume's ID
	App         string            `json:"app"`
	Name        string            `json:"name"`   // the volume's name
	Backup      string            `json:"backup"` // the backup's name
	Destination BackupDestination `json:"destination"`
	Path        string            `json:"path"`
	Key         string            `json:"key,omitempty"`
	// Instance, when a running VM has the disk attached, is that VM, and
	// MountPath where the volume is in it: writes there are paused for the
	// moment the backup is taken.
	Instance  string `json:"instance,omitempty"`
	MountPath string `json:"mount_path,omitempty"`
}

const (
	VolumeOwner    = "owner"    // the disk lives here
	VolumeIncoming = "incoming" // copy the disk from From
	VolumePrevious = "previous" // moved away; keep the old copy until the new node has the disk
	VolumeDestroy  = "destroy"  // delete the disk
	// VolumeStale: the volume was restored onto another node while this one
	// was down. Its disk here may hold newer writes, so it is kept aside
	// (.stale), never used.
	VolumeStale   = "stale"
	VolumeDiscard = "discard" // delete the copy kept aside
)

// VolumeStatus is what a node reports about a volume it holds.
type VolumeStatus struct {
	ID string `json:"id"`
	// State is ready, missing or moved (handed to the incoming node) for
	// an owner; copying, synced (copied while the instance runs; waiting for
	// it to stop) or received for an incoming node; destroyed once deleted.
	State    string `json:"state"`
	UsedMB   int    `json:"used_mb,omitempty"`
	CopiedMB int    `json:"copied_mb,omitempty"`
	Error    string `json:"error,omitempty"`
	// Restore is how a restore from a backup is going: copying, received
	// (downloaded, waiting for the instance to stop) or restored.
	Restore string `json:"restore,omitempty"`
}

const (
	VolumeReady     = "ready"
	VolumeMissing   = "missing"
	VolumeMoved     = "moved"
	VolumeCopying   = "copying"
	VolumeSynced    = "synced"
	VolumeReceived  = "received"
	VolumeRestored  = "restored"
	VolumeFailed    = "failed" // a restore that could not download its backup
	VolumeDestroyed = "destroyed"
	VolumeKept      = "kept"      // the old copy is kept aside
	VolumeDiscarded = "discarded" // the old copy is gone
)

// FeatureVolumes is reported by agents that can hold volumes. Instances with
// volumes are only placed on nodes reporting it, so an agent a release
// behind never starts one without its disk.
const FeatureVolumes = "volumes"

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
	Features  []string         `json:"features,omitempty"`
	Metrics   NodeMetrics      `json:"metrics"`
	Instances []InstanceStatus `json:"instances"`
	Volumes   []VolumeStatus   `json:"volumes,omitempty"`
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
	State       string    `json:"state"` // pulling | syncing | starting | healthy | crashed | failed | stopped
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
	Kind      string    `json:"kind,omitempty"` // a database's engine
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
