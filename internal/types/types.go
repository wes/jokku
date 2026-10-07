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
// everything first.
type ConfigPatch struct {
	Set       map[string]string `json:"set,omitempty"`
	Unset     []string          `json:"unset,omitempty"`
	Clear     bool              `json:"clear,omitempty"`
	NoRestart bool              `json:"no_restart,omitempty"`
}

type ConfigVars struct {
	Vars map[string]string `json:"vars"`
	// Restarting is true when the change triggered a rollout.
	Restarting bool `json:"restarting,omitempty"`
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
	SkipDeploy bool           `json:"skip_deploy,omitempty"`
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
	Type    string `json:"type"` // log | done
	Message string `json:"message,omitempty"`
	Status  string `json:"status,omitempty"` // on done: succeeded | failed
	Error   string `json:"error,omitempty"`
}

const (
	EventLog  = "log"
	EventDone = "done"

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
