package types

import "time"

// ProtocolVersion is the control <-> agent protocol. The control node rejects
// agents speaking another version, with a message to update. Their state
// polls still work, and carry an update order for agents that can update
// themselves, so keep NodeState.Update readable by older agents.
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
	// EdgeAccess is what the cluster's edge nodes may reach on or through
	// this node; nil without edges, and on edges themselves.
	EdgeAccess *EdgeAccess `json:"edge_access,omitempty"`
	// Update tells the node to update jokku; nil the rest of the time.
	Update *UpdateOrder `json:"update,omitempty"`
}

// UpdateOrder asks a node's agent to update jokku to Version. A node acts on
// each Attempt once, so one that failed waits for a new order.
type UpdateOrder struct {
	Version string `json:"version"`
	Attempt string `json:"attempt"`
}

// UpdateFailure is a node saying an update order failed.
type UpdateFailure struct {
	Attempt string `json:"attempt"`
	Error   string `json:"error"`
}

type NodeIdentity struct {
	Name        string `json:"name"`
	Role        string `json:"role,omitempty"` // control | worker | edge
	Subnet      string `json:"subnet"`         // e.g. 10.210.2.0/24
	MeshIP      string `json:"mesh_ip"`        // e.g. 10.210.2.1
	ClusterCIDR string `json:"cluster_cidr"`
}

// Peer is another node on the WireGuard mesh.
type Peer struct {
	Name      string `json:"name"`
	Role      string `json:"role,omitempty"` // RoleEdge for an edge; empty otherwise
	PublicKey string `json:"public_key"`
	// Endpoint is where to reach its WireGuard, host:port. It is empty in an
	// edge's peer list: the nodes behind an edge may sit behind NAT, so they
	// dial it, never the other way round.
	Endpoint string `json:"endpoint,omitempty"`
	Subnet   string `json:"subnet"`
	MeshIP   string `json:"mesh_ip"`
	// Routes are more addresses (IP/32) reached through this peer: the
	// targets of external apps it forwards to. Only edges get them.
	Routes []string `json:"routes,omitempty"`
	// AgentAddr is its agent API, host:port, where nodes ask each other
	// whether they still hear from the control node. Edges have none: they
	// take no part in that vote.
	AgentAddr string `json:"agent_addr,omitempty"`
}

// RoleEdge is an edge node: a public machine that only receives traffic
// for the cluster's domains and sends it over the mesh to the nodes behind
// it, which dial it. It runs no apps.
const RoleEdge = "edge"

// EdgeAccess is what edge nodes may reach on or through a node over the
// mesh. An edge faces the internet, so it gets only what its routes need:
// the web ports of this node's instances, the external-app targets this
// node forwards to, and on the control node, the API.
type EdgeAccess struct {
	Edges   []string `json:"edges"`             // the edges' mesh subnets
	Allow   []string `json:"allow,omitempty"`   // IP:port they may reach
	Targets []string `json:"targets,omitempty"` // IPs outside the cluster reached for them (masqueraded)
	Control bool     `json:"control,omitempty"` // this is the control node: edges may reach its API
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

// FeatureRelay is reported by agents whose proxy relays an edge's requests
// (ProxyState.Edges), and, on an edge, sends them through the nodes'
// proxies (ProxyRoute.Relayed), from v0.8.7. An edge reaches nodes without
// it straight at their instances, as before.
const FeatureRelay = "relay"

// FeatureStreams is reported by agents whose proxy carries TCP routes
// (ProxyState.Streams), from v0.8.10.
const FeatureStreams = "streams"

// FeatureUpdate is reported by agents that update jokku when told to
// (NodeState.Update), from v0.8.5. Older ones are updated by hand.
const FeatureUpdate = "update"

type ProxyState struct {
	Routes []ProxyRoute `json:"routes"`
	Email  string       `json:"email,omitempty"`
	// Auth is what routes with a login need: the session key, the users and
	// the login domain. Nil when no route has a login.
	Auth *ProxyAuth `json:"auth,omitempty"`
	// Edges are the edges' mesh subnets, on a node an edge sends requests
	// through: this node's proxy answers them for the edge, compressing
	// before they go back over the mesh.
	Edges []string `json:"edges,omitempty"`
	// Streams are the TCP routes: databases reached from outside
	// (db:<engine>:expose). Only nodes reporting FeatureStreams get them.
	Streams []ProxyStream `json:"streams,omitempty"`
}

type ProxyRoute struct {
	App       string   `json:"app"`
	Hosts     []string `json:"hosts"`
	Upstreams []string `json:"upstreams"`
	TLS       bool     `json:"tls"`
	// UpstreamTLS says the upstreams speak HTTPS (an external app's target),
	// and Insecure that their certificates aren't checked.
	UpstreamTLS bool `json:"upstream_tls,omitempty"`
	Insecure    bool `json:"insecure,omitempty"`
	// Auth puts a login in front of the route; nil for none.
	Auth *RouteAuth `json:"auth,omitempty"`
	// Sleeps says the app sleeps when idle (ps:set sleep-after): the proxy
	// tells the control node it is being used, and a request that finds it
	// asleep (no upstreams) wakes it and waits.
	Sleeps bool `json:"sleeps,omitempty"`
	// NoCompress: the app opted out of compression (proxy:set compress
	// false).
	NoCompress bool `json:"no_compress,omitempty"`
	// Relayed, on an edge: the upstreams are nodes' proxies, which pass the
	// request on to an instance and compress the response.
	Relayed bool `json:"relayed,omitempty"`
}

// ProxyStream sends the TCP connections for Host on Port to Upstreams. A
// postgres or tls stream shares its port with the others of its protocol,
// told apart by the name the client asks for (TLS SNI), and the proxy
// terminates its TLS; a plain one has its port to itself and passes through
// as it is (MySQL, whose TLS starts inside its own protocol).
type ProxyStream struct {
	App       string   `json:"app"`
	Protocol  string   `json:"protocol"` // StreamPostgres, StreamTLS or StreamPlain
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Upstreams []string `json:"upstreams"` // IP:port; none while the database isn't running
	// ACME: Host's certificate comes from Let's Encrypt; otherwise from the
	// proxy's own CA, for a name Let's Encrypt can't reach (clients then
	// encrypt without verifying, as sslmode=require does).
	ACME bool `json:"acme,omitempty"`
	// Allow is the client addresses let in (CIDRs); empty lets in any.
	Allow []string `json:"allow,omitempty"`
}

// Stream protocols.
const (
	StreamPostgres = "postgres" // TLS, after Postgres's SSLRequest or directly (PG17's sslnegotiation=direct)
	StreamTLS      = "tls"      // TLS from the first byte (rediss://)
	StreamPlain    = "plain"    // passed through untouched
)

// ProxyActivity is what a node's proxy tells the control node about apps
// that sleep when idle: those it handled requests for since it last said,
// and those a request is waiting to wake.
type ProxyActivity struct {
	Active []string `json:"active,omitempty"`
	Wake   []string `json:"wake,omitempty"`
}

// ProxyAuth is the cluster's login settings, shared by every route that
// has a login.
type ProxyAuth struct {
	Key         string      `json:"key"`                    // signs sessions, base64
	LoginDomain string      `json:"login_domain,omitempty"` // one login page for every domain
	SessionDays int         `json:"session_days"`
	Users       []ProxyUser `json:"users,omitempty"`
}

type ProxyUser struct {
	Name         string `json:"name"`
	PasswordHash string `json:"password_hash"` // bcrypt
	TOTPSecret   string `json:"totp_secret,omitempty"`
}

// RouteAuth is the login in front of one app.
type RouteAuth struct {
	Mode         string       `json:"mode"`                    // AuthPassword or AuthUsers
	PasswordHash string       `json:"password_hash,omitempty"` // bcrypt, for AuthPassword
	Users        []string     `json:"users,omitempty"`         // allowed users; empty allows every user
	AllowIPs     []string     `json:"allow_ips,omitempty"`     // CIDRs that skip the login
	BypassPaths  []string     `json:"bypass_paths,omitempty"`  // paths that skip it: exact, or a prefix ending in *
	Shares       []ProxyShare `json:"shares,omitempty"`
}

// ProxyShare is a link that lets anyone holding it in, until it expires.
type ProxyShare struct {
	ID        string    `json:"id"`
	TokenHash string    `json:"token_hash"` // hex SHA-256 of the token in the link
	ExpiresAt time.Time `json:"expires_at"`
}

// Login modes.
const (
	AuthPassword = "password" // one shared password (or PIN) for the app
	AuthUsers    = "users"    // the cluster's users, each with a password and optionally a TOTP code
)

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
	// UpdateFailed is set once an update order has failed here.
	UpdateFailed *UpdateFailure `json:"update_failed,omitempty"`
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
	// Update is where the node stands in an update of the cluster; nil when
	// none is asking anything of it.
	Update *NodeUpdate `json:"update,omitempty"`
}

// NodeUpdate is one node's part in updating the cluster to a version.
type NodeUpdate struct {
	To    string     `json:"to"`
	State string     `json:"state"` // one of the NodeUpdate states below
	Since *time.Time `json:"since,omitempty"`
	Error string     `json:"error,omitempty"`
}

// NodeUpdate states.
const (
	UpdateWaiting = "waiting"  // its turn is coming (once it is up, if it is down)
	UpdateRunning = "updating" // updating now
	UpdateFailed  = "failed"   // its update failed; the cluster's update stopped there
	UpdateOnHold  = "on-hold"  // waiting, but the cluster's update stopped at a failure
	UpdateByHand  = "by-hand"  // its jokku is too old to update itself
)

type AppView struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind,omitempty"` // a database's engine
	Release   int       `json:"release"`
	Stopped   bool      `json:"stopped"`
	Asleep    bool      `json:"asleep,omitempty"`
	Locked    bool      `json:"locked"`
	Domains   []string  `json:"domains"`
	Healthy   int       `json:"healthy"`
	Wanted    int       `json:"wanted"`
	Deploy    *Deploy   `json:"deploy,omitempty"` // most recent
	CreatedAt time.Time `json:"created_at"`
	// Links are the apps a database is linked to.
	Links []DatabaseLink `json:"links,omitempty"`
}

type InstanceView struct {
	Instance
	App        string  `json:"app"`
	CPUPercent float64 `json:"cpu_percent"`
	MemoryUsed int     `json:"memory_used_mb"`
}

// Request is one HTTP request handled by a node's proxy: the router lines
// in "jokku logs" and the traffic view in "jokku top".
// Latency is how long requests took over the last hour and day, from every
// node's proxy: overall and by app ("" for requests no app served).
type Latency struct {
	Hour LatencyWindow `json:"hour"`
	Day  LatencyWindow `json:"day"`
	// Missing are the nodes that didn't answer (down, or a release
	// behind), whose requests aren't counted.
	Missing []string `json:"missing,omitempty"`
}

type LatencyWindow struct {
	LatencyStats
	Apps map[string]LatencyStats `json:"apps,omitempty"`
}

type LatencyStats struct {
	Requests uint64  `json:"requests"`
	P50      float64 `json:"p50_ms"`
	P95      float64 `json:"p95_ms"`
}

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

// DatabaseStats is a look inside a database ("jokku top", db:<engine>:info):
// its connections now, and its queries over the last minute, hour and day.
// The control node takes a look at each running database every few seconds
// and keeps the history in memory, so it starts over when it restarts.
type DatabaseStats struct {
	App    string    `json:"app"`
	Engine string    `json:"engine"`
	Name   string    `json:"name"`
	Node   string    `json:"node,omitempty"`
	At     time.Time `json:"at,omitzero"` // the last look; zero before the first
	// Error is why the last look failed, and Note something missing, in a
	// sentence ("query times start at its next restart").
	Error string `json:"error,omitempty"`
	Note  string `json:"note,omitempty"`

	Connections    int              `json:"connections"`
	MaxConnections int              `json:"max_connections"`
	Active         int              `json:"active"`             // running a query now; -1 when the engine can't tell
	MaxUsed        int              `json:"max_used,omitempty"` // the most at once since it started, when the engine keeps it
	Clients        []DatabaseClient `json:"clients,omitempty"`

	// Per is what a query is: "query", "transaction" (Postgres without
	// pg_stat_statements) or "command" (Redis). HitsAre is what the hit
	// rate counts: "cache" (reads from memory rather than disk) or
	// "keys" (Redis: keys found).
	Per     string         `json:"per,omitempty"`
	HitsAre string         `json:"hits_are,omitempty"`
	Minute  DatabaseWindow `json:"minute"`
	Hour    DatabaseWindow `json:"hour"`
	Day     DatabaseWindow `json:"day"`

	Slowest    []DatabaseQuery `json:"slowest,omitempty"` // Postgres: by total time, since its statistics started
	Users      []DatabaseUser  `json:"users,omitempty"`
	MemoryUsed uint64          `json:"memory_used,omitempty"` // Redis, bytes
	MemoryMax  uint64          `json:"memory_max,omitempty"`  // 0: no limit
}

// DatabaseClient is a database's connections from one place as one user:
// an app's instance, the proxies (clients outside, db:<engine>:expose), or
// the database's own VM (db:<engine>:connect).
type DatabaseClient struct {
	App      string `json:"app,omitempty"`      // the app whose instance connects
	Instance string `json:"instance,omitempty"` // web.1
	Via      string `json:"via,omitempty"`      // the node whose proxy carries clients outside
	Local    bool   `json:"local,omitempty"`    // from inside the database's VM
	Addr     string `json:"addr,omitempty"`
	User     string `json:"user,omitempty"`
	Count    int    `json:"count"`
	Active   int    `json:"active"`
}

// DatabaseWindow is a database's queries over a span of time.
type DatabaseWindow struct {
	Seconds   float64 `json:"seconds"` // how much of the span there are looks for
	Queries   uint64  `json:"queries"`
	PerSecond float64 `json:"per_second"`
	// P50, P95 and P99 are how long queries took inside the database, in
	// milliseconds, from engines that keep a histogram (MySQL, Redis);
	// AvgMS is the mean, which every engine has. 0 without queries.
	P50   float64 `json:"p50_ms,omitempty"`
	P95   float64 `json:"p95_ms,omitempty"`
	P99   float64 `json:"p99_ms,omitempty"`
	AvgMS float64 `json:"avg_ms,omitempty"`
	// HitRate is the share of reads served from memory (or keys found),
	// 0 to 1; -1 without reads.
	HitRate float64 `json:"hit_rate"`
}

// DatabaseQuery is a statement and how long it has taken.
type DatabaseQuery struct {
	Query   string  `json:"query"`
	Calls   uint64  `json:"calls"`
	TotalMS float64 `json:"total_ms"`
	MeanMS  float64 `json:"mean_ms"`
}

// DatabaseUser is a login of a database's. Access is "all" (schema changes
// included), "read-write", "read-only" or "other" (privileges Jokku didn't
// set); Password and URLs are known for the users Jokku added.
type DatabaseUser struct {
	Name      string `json:"name"`
	Access    string `json:"access"`
	Password  string `json:"password,omitempty"`
	URL       string `json:"url,omitempty"`
	PublicURL string `json:"public_url,omitempty"`
}
