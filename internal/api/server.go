// Package api is Jokku's HTTP API. Every client, including the CLI and the git
// hook, goes through it; nothing else mutates state.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/gitrepo"
	"github.com/wes/jokku/internal/sshkeys"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/version"
)

type Config struct {
	Store    *store.Store
	Git      *gitrepo.Manager
	Deployer Deployer
	Log      *slog.Logger

	// DataDir holds builds and other control-node files.
	DataDir string
	// Exe is the jokku binary path written into authorized_keys and hooks.
	Exe string
	// AuthorizedKeys is the jokku user's authorized_keys file. Empty disables
	// syncing (local development).
	AuthorizedKeys string
	// Owner owns AuthorizedKeys; nil leaves ownership alone.
	Owner *sshkeys.Owner
	// Cluster is the control plane: nodes, placement, agents.
	Cluster *cluster.Controller
}

// Server serves two listeners: the unix socket gets the whole API (whoever
// can open it is an admin), and the TLS port (Public) only joining and the
// agent endpoints, each authenticated with a token.
type Server struct {
	Config
	mux    *http.ServeMux
	public *http.ServeMux

	keysMu sync.Mutex // serializes authorized_keys rewrites
}

func New(cfg Config) *Server {
	s := &Server{Config: cfg, mux: http.NewServeMux(), public: http.NewServeMux()}
	s.routes()
	return s
}

// Public is the handler for the TLS port.
func (s *Server) Public() http.Handler { return s.public }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /v1/version", s.version)

	m.HandleFunc("GET /v1/apps", s.listApps)
	m.HandleFunc("POST /v1/apps", s.createApp)
	m.HandleFunc("GET /v1/apps/{app}", s.getApp)
	m.HandleFunc("DELETE /v1/apps/{app}", s.destroyApp)
	m.HandleFunc("POST /v1/apps/{app}/rename", s.renameApp)
	m.HandleFunc("POST /v1/apps/{app}/clone", s.cloneApp)
	m.HandleFunc("PUT /v1/apps/{app}/lock", s.lockApp(true))
	m.HandleFunc("DELETE /v1/apps/{app}/lock", s.lockApp(false))

	m.HandleFunc("GET /v1/config", s.getConfig)
	m.HandleFunc("PATCH /v1/config", s.patchConfig)
	m.HandleFunc("GET /v1/apps/{app}/config", s.getConfig)
	m.HandleFunc("PATCH /v1/apps/{app}/config", s.patchConfig)

	m.HandleFunc("GET /v1/domains", s.getDomains)
	m.HandleFunc("PATCH /v1/domains", s.patchDomains)
	m.HandleFunc("GET /v1/apps/{app}/domains", s.getDomains)
	m.HandleFunc("PATCH /v1/apps/{app}/domains", s.patchDomains)

	m.HandleFunc("GET /v1/properties/{plugin}", s.getProperties)
	m.HandleFunc("PUT /v1/properties/{plugin}/{key}", s.setProperty)
	m.HandleFunc("GET /v1/apps/{app}/properties/{plugin}", s.getProperties)
	m.HandleFunc("PUT /v1/apps/{app}/properties/{plugin}/{key}", s.setProperty)

	m.HandleFunc("GET /v1/apps/{app}/formation", s.getFormation)
	m.HandleFunc("POST /v1/apps/{app}/scale", s.scale)
	m.HandleFunc("GET /v1/apps/{app}/resources", s.getResources)
	m.HandleFunc("PATCH /v1/apps/{app}/resources", s.patchResources)

	m.HandleFunc("GET /v1/apps/{app}/deploys", s.listDeploys)
	m.HandleFunc("POST /v1/apps/{app}/deploys", s.createDeploy)
	m.HandleFunc("POST /v1/apps/{app}/git", s.ensureGitRepo)
	m.HandleFunc("POST /v1/apps/{app}/ps/{action}", s.psAction)
	m.HandleFunc("GET /v1/apps/{app}/instances", s.listInstances)
	m.HandleFunc("GET /v1/apps/{app}/releases", s.listReleases)
	m.HandleFunc("GET /v1/apps/{app}/logs", s.logs)

	m.HandleFunc("GET /v1/apps/{app}/volumes", s.listVolumes)
	m.HandleFunc("POST /v1/apps/{app}/volumes", s.createVolume)
	m.HandleFunc("GET /v1/apps/{app}/volumes/{name}", s.getVolume)
	m.HandleFunc("PATCH /v1/apps/{app}/volumes/{name}", s.resizeVolume)
	m.HandleFunc("DELETE /v1/apps/{app}/volumes/{name}", s.destroyVolume)
	m.HandleFunc("POST /v1/apps/{app}/volumes/{name}/mounts", s.mountVolume)
	m.HandleFunc("DELETE /v1/apps/{app}/volumes/{name}/mounts", s.unmountVolume)
	m.HandleFunc("POST /v1/apps/{app}/volumes/{name}/move", s.moveVolume)

	m.HandleFunc("GET /v1/registries", s.listRegistryLogins)
	m.HandleFunc("PUT /v1/registries/{server}", s.setRegistryLogin)
	m.HandleFunc("DELETE /v1/registries/{server}", s.deleteRegistryLogin)

	m.HandleFunc("GET /v1/ssh-keys", s.listSSHKeys)
	m.HandleFunc("POST /v1/ssh-keys", s.addSSHKey)
	m.HandleFunc("DELETE /v1/ssh-keys/{name}", s.removeSSHKey)

	m.HandleFunc("GET /v1/nodes", s.listNodes)
	m.HandleFunc("GET /v1/nodes/{name}", s.getNode)
	m.HandleFunc("PATCH /v1/nodes/{name}", s.patchNode)
	m.HandleFunc("DELETE /v1/nodes/{name}", s.removeNode)

	m.HandleFunc("POST /v1/cluster/join-tokens", s.createJoinToken)
	m.HandleFunc("GET /v1/cluster/status", s.clusterStatus)
	m.HandleFunc("GET /v1/events", s.listEvents)
	m.HandleFunc("GET /v1/requests", s.requests)

	notFound := func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, httpErrorf(http.StatusNotFound, "no such endpoint: %s %s", r.Method, r.URL.Path))
	}
	m.HandleFunc("/", notFound)

	p := s.public
	p.HandleFunc("GET /v1/version", s.version)
	p.HandleFunc("POST /v1/cluster/join", s.join)
	p.HandleFunc("GET /v1/agent/state", s.agentAuth(s.agentState))
	p.HandleFunc("POST /v1/agent/status", s.agentAuth(s.agentStatus))
	p.HandleFunc("GET /v1/agent/artifacts/{name}", s.agentAuth(s.agentArtifact))
	p.HandleFunc("GET /v1/agent/certs", s.agentAuth(s.certGet))
	p.HandleFunc("PUT /v1/agent/certs", s.agentAuth(s.certPut))
	p.HandleFunc("DELETE /v1/agent/certs", s.agentAuth(s.certDelete))
	p.HandleFunc("GET /v1/agent/certs/list", s.agentAuth(s.certList))
	p.HandleFunc("GET /v1/agent/certs/stat", s.agentAuth(s.certStat))
	p.HandleFunc("POST /v1/agent/certs/lock", s.agentAuth(s.certLock))
	p.HandleFunc("DELETE /v1/agent/certs/lock", s.agentAuth(s.certUnlock))
	p.HandleFunc("/", notFound)
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, types.Version{Version: version.Version, Commit: version.Commit})
}

// httpError is an error with a status code and a message safe to show users.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func httpErrorf(status int, format string, args ...any) error {
	return &httpError{status: status, msg: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) error {
	return httpErrorf(http.StatusBadRequest, format, args...)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	msg := "internal error"
	var he *httpError
	switch {
	case errors.As(err, &he):
		status, msg = he.status, he.msg
	case errors.Is(err, store.ErrNotFound):
		status, msg = http.StatusNotFound, err.Error()
	case errors.Is(err, store.ErrExists):
		status, msg = http.StatusConflict, err.Error()
	default:
		s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeJSON(w, status, types.Error{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid request body: %v", err)
	}
	return nil
}

func actor(r *http.Request) string {
	if a := r.Header.Get(types.HeaderActor); a != "" {
		return a
	}
	return "local"
}

// stream writes newline-delimited JSON events for long-running operations.
type stream struct {
	w   http.ResponseWriter
	enc *json.Encoder
	rc  *http.ResponseController
	mu  sync.Mutex
}

func newStream(w http.ResponseWriter) *stream {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	return &stream{w: w, enc: json.NewEncoder(w), rc: http.NewResponseController(w)}
}

func (st *stream) send(e types.Event) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.enc.Encode(e)
	st.rc.Flush()
}

// Log sends one line of output.
func (st *stream) Log(line string) { st.send(types.Event{Type: types.EventLog, Message: line}) }

// Done ends the stream with the operation's outcome.
func (st *stream) Done(err error) {
	if err != nil {
		st.send(types.Event{Type: types.EventDone, Status: types.StatusFailed, Error: err.Error()})
		return
	}
	st.send(types.Event{Type: types.EventDone, Status: types.StatusSucceeded})
}
