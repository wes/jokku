// Package types defines the JSON shapes of the Jokku HTTP API. The server and
// every client (CLI, git hook, future GUI) share them.
package types

import "time"

type Version struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}

type App struct {
	Name           string    `json:"name"`
	Locked         bool      `json:"locked"`
	Stopped        bool      `json:"stopped"` // ps:stop
	CreatedAt      time.Time `json:"created_at"`
	CurrentRelease int       `json:"current_release,omitempty"`
	DeploySource   string    `json:"deploy_source,omitempty"`
}

type CreateAppRequest struct {
	Name string `json:"name"`
}

type RenameAppRequest struct {
	NewName string `json:"new_name"`
}

type CloneAppRequest struct {
	NewName string `json:"new_name"`
}

// ConfigPatch changes config vars. Unset runs before Set; Clear removes
// everything first. Changes apply to running instances on the next restart
// (POST /v1/apps/{app}/ps/restart).
type ConfigPatch struct {
	Set   map[string]string `json:"set,omitempty"`
	Unset []string          `json:"unset,omitempty"`
	Clear bool              `json:"clear,omitempty"`
}

type ConfigVars struct {
	Vars    map[string]string `json:"vars"`
	Changed bool              `json:"changed,omitempty"` // on PATCH: whether anything changed
}

// DomainsPatch changes an app's (or the global) domains. Exactly one field
// should be set.
type DomainsPatch struct {
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
	Set    []string `json:"set,omitempty"`
	Clear  bool     `json:"clear,omitempty"`
}

type Domains struct {
	App     []string `json:"app"`
	Global  []string `json:"global"`
	Vhosts  []string `json:"vhosts"` // effective hostnames the proxy will serve
	Enabled bool     `json:"enabled"`
}

// RegistryLogin lets builds pull from a private registry. The password is
// never sent back.
type RegistryLogin struct {
	Server    string    `json:"server"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

type RegistryLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type SSHKey struct {
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	PublicKey   string    `json:"public_key"`
	CreatedAt   time.Time `json:"created_at"`
}

type AddSSHKeyRequest struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

// Process is one process type's formation and size.
type Process struct {
	Type     string `json:"type"`
	Quantity int    `json:"quantity"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`
}

type Formation struct {
	Processes []Process `json:"processes"`
}

type ScaleRequest struct {
	Quantities map[string]int `json:"quantities"`
}

// ResourceLimits sets vCPUs and memory for one process type, or for every
// process type of the app when ProcessType is empty. Nil fields are left as
// they are; zero clears the override.
type ResourceLimits struct {
	ProcessType string `json:"process_type,omitempty"`
	CPUs        *int   `json:"cpus,omitempty"`
	MemoryMB    *int   `json:"memory_mb,omitempty"`
}

type Resources struct {
	Default ResourceSize            `json:"default"`
	Process map[string]ResourceSize `json:"process"`
}

type ResourceSize struct {
	CPUs     int `json:"cpus,omitempty"`
	MemoryMB int `json:"memory_mb,omitempty"`
}

// Properties is the report shape for any Dokku-style "<plugin>:set" plugin.
type Properties struct {
	Plugin   string            `json:"plugin"`
	App      map[string]string `json:"app,omitempty"`
	Global   map[string]string `json:"global"`
	Computed map[string]string `json:"computed,omitempty"`
}

type SetPropertyRequest struct {
	Value string `json:"value"` // empty unsets
}

// BuilderRequest switches how an app is built: from a Dockerfile or a
// compose file (File, relative to the build dir; empty for the default).
type BuilderRequest struct {
	Type string `json:"type"`
	File string `json:"file,omitempty"`
}

type Node struct {
	Name        string    `json:"name"`
	Role        string    `json:"role"`   // control | worker
	Status      string    `json:"status"` // ready | down | draining
	Address     string    `json:"address"`
	MeshIP      string    `json:"mesh_ip"`
	Subnet      string    `json:"subnet"`
	Arch        string    `json:"arch"`
	CPUs        int       `json:"cpus"`
	MemoryMB    int       `json:"memory_mb"`
	Schedulable bool      `json:"schedulable"`
	Ingress     bool      `json:"ingress"`
	LastSeen    time.Time `json:"last_seen"`
	CreatedAt   time.Time `json:"created_at"`
}

// Instance is one running (or starting, or retiring) microVM.
type Instance struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"` // web.1
	Release   int       `json:"release"`
	Node      string    `json:"node"`
	IP        string    `json:"ip"`
	Port      int       `json:"port"`
	CPUs      int       `json:"cpus"`
	MemoryMB  int       `json:"memory_mb"`
	State     string    `json:"state"`   // pending | starting | healthy | crashed | failed
	Desired   string    `json:"desired"` // running | stopped
	Restarts  int       `json:"restarts"`
	StartedAt time.Time `json:"started_at"`
}

// Volume is persistent storage for an app's instances. Type "local" is an
// ext4 disk on one node: the instance it is mounted in runs on that node,
// and the disk is copied along when the instance moves (nodes:drain,
// storage:move).
type Volume struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"` // local
	SizeMB int    `json:"size_mb"`
	UsedMB int    `json:"used_mb"`
	// Node holds a local volume's disk; empty until an instance first uses
	// it.
	Node string `json:"node,omitempty"`
	// Disk is the disk image's path on Node.
	Disk   string        `json:"disk,omitempty"`
	Status string        `json:"status"` // new | ready | missing | moving | restoring | destroying
	Move   *VolumeMove   `json:"move,omitempty"`
	Mounts []VolumeMount `json:"mounts"`

	CreatedAt time.Time `json:"created_at"`
}

// VolumeMove is a local volume's copy to another node, or a restore from a
// backup onto To.
type VolumeMove struct {
	To       string `json:"to"`
	State    string `json:"state"` // copying | synced (waiting for the instance to stop) | received
	CopiedMB int    `json:"copied_mb"`
	Error    string `json:"error,omitempty"`
}

// VolumeMount puts a volume at Path in a process type's instances.
type VolumeMount struct {
	ProcessType string `json:"process_type"`
	Path        string `json:"path"`
}

const (
	VolumeLocal = "local"
)

type CreateVolumeRequest struct {
	Name   string `json:"name"`
	Type   string `json:"type,omitempty"`    // default local
	SizeMB int    `json:"size_mb,omitempty"` // default 10 GiB
}

// ResizeVolumeRequest grows a volume. The instance using it picks up the new
// size when it restarts.
type ResizeVolumeRequest struct {
	SizeMB int `json:"size_mb"`
}

type MoveVolumeRequest struct {
	Node string `json:"node"`
}

// BackupDestination is an S3-compatible bucket that volumes are backed up
// to. The secret only travels to the node doing a backup or restore; lists
// leave it out.
type BackupDestination struct {
	Name            string    `json:"name"`
	Endpoint        string    `json:"endpoint"` // https://host[:port]; http:// for a local test server
	Region          string    `json:"region,omitempty"`
	Bucket          string    `json:"bucket"`
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key,omitempty"`
	Encrypt         bool      `json:"encrypt"`
	CreatedAt       time.Time `json:"created_at"`
}

type CreateBackupDestinationRequest struct {
	Name            string `json:"name"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region,omitempty"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	NoEncrypt       bool   `json:"no_encrypt,omitempty"`
}

// BackupKey is the key that encrypts the cluster's backups. Saved says the
// user confirmed keeping a copy; encrypted backups wait until then.
type BackupKey struct {
	Key   string `json:"key"`
	ID    string `json:"id"`
	Saved bool   `json:"saved"`
}

// SetVolumeBackupRequest says where a volume is backed up. Path defaults
// to jokku/<app>/<volume>.
type SetVolumeBackupRequest struct {
	Destination string `json:"destination"`
	Path        string `json:"path,omitempty"`
}

// VolumeBackups is where a volume is backed up, and the backups there.
type VolumeBackups struct {
	App         string       `json:"app"`
	Volume      string       `json:"volume"`
	Destination string       `json:"destination"`
	Path        string       `json:"path"`
	Backups     []BackupInfo `json:"backups,omitempty"` // newest first
	Last        *BackupRun   `json:"last,omitempty"`    // the latest attempt, failed or not
	Succeeded   *BackupRun   `json:"succeeded,omitempty"`
}

// BackupInfo is one backup in a bucket. The sizes are known for backups
// this cluster made.
type BackupInfo struct {
	Name      string    `json:"name"`
	Time      time.Time `json:"time"`
	Size      int64     `json:"size,omitempty"`      // the disk's size
	NewBytes  int64     `json:"new_bytes,omitempty"` // what it added to the bucket
	NewBlocks int       `json:"new_blocks,omitempty"`
}

// BackupRun is one attempt at a backup.
type BackupRun struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"` // running | succeeded | failed
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// BackupResult is what one backup stored.
type BackupResult struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Blocks    int    `json:"blocks"`
	NewBlocks int    `json:"new_blocks"`
	NewBytes  int64  `json:"new_bytes"`
}

// RestoreVolumeRequest restores a volume from one of its backups (the
// latest without Backup), on Node if given. Unless SkipBackup, the volume
// is backed up first, so the restore can be undone.
type RestoreVolumeRequest struct {
	Backup     string `json:"backup,omitempty"`
	Node       string `json:"node,omitempty"`
	SkipBackup bool   `json:"skip_backup,omitempty"`
}

type Release struct {
	Version     int       `json:"version"`
	Description string    `json:"description"`
	Current     bool      `json:"current"`
	CreatedAt   time.Time `json:"created_at"`
}

// LogOptions select app log lines.
type LogOptions struct {
	Tail    int    `json:"tail"`
	Follow  bool   `json:"follow"`
	Process string `json:"process,omitempty"` // "web" or "web.1"
}

type Deploy struct {
	ID         int64      `json:"id"`
	App        string     `json:"app"`
	Source     string     `json:"source"` // git | archive | image | rebuild
	SourceRef  string     `json:"source_ref,omitempty"`
	Status     string     `json:"status"` // pending | building | deploying | succeeded | failed
	Error      string     `json:"error,omitempty"`
	Actor      string     `json:"actor,omitempty"`
	Release    int        `json:"release,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Event is one line of a streamed (application/x-ndjson) response.
type Event struct {
	Type    string   `json:"type"` // log | request | done
	Message string   `json:"message,omitempty"`
	Request *Request `json:"request,omitempty"`
	Status  string   `json:"status,omitempty"` // on done: succeeded | failed
	Error   string   `json:"error,omitempty"`
	// Backup, on a node's done event for a backup, is what it stored.
	Backup *BackupResult `json:"backup,omitempty"`
}

const (
	EventLog     = "log"
	EventRequest = "request"
	EventDone    = "done"

	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

type GitRepo struct {
	Path string `json:"path"`
}

type DeployKey struct {
	PublicKey string `json:"public_key"`
}

// Header names used by the API.
const (
	// HeaderActor names who is making the request (an SSH key name, a token
	// name, "root"). Informational in v1.
	HeaderActor = "X-Jokku-Actor"
)
