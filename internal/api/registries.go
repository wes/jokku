package api

import (
	"net/http"
	"strings"

	imageref "github.com/google/go-containerregistry/pkg/name"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

func (s *Server) listRegistryLogins(w http.ResponseWriter, r *http.Request) {
	logins, err := s.Store.RegistryLogins(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []types.RegistryLogin{}
	for _, l := range logins {
		out = append(out, types.RegistryLogin{Server: l.Server, Username: l.Username, CreatedAt: l.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) setRegistryLogin(w http.ResponseWriter, r *http.Request) {
	server, err := registryServer(r.PathValue("server"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req types.RegistryLoginRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Username == "" || req.Password == "" {
		s.fail(w, r, badRequest("A registry login needs a username and a password (or token)"))
		return
	}
	err = s.Store.SetRegistryLogin(r.Context(), store.RegistryLogin{Server: server, Username: req.Username, Password: req.Password})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteRegistryLogin(w http.ResponseWriter, r *http.Request) {
	server, err := registryServer(r.PathValue("server"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.DeleteRegistryLogin(r.Context(), server); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// registryServer checks and normalizes a registry host: ghcr.io,
// registry.example.com:5000. Docker Hub's aliases become docker.io.
func registryServer(s string) (string, error) {
	s = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://"), "/"))
	switch s {
	case "index.docker.io", "registry-1.docker.io", "index.docker.io/v1":
		s = "docker.io"
	}
	if _, err := imageref.NewRegistry(s, imageref.StrictValidation); err != nil || s == "" || strings.Contains(s, "/") {
		return "", badRequest("%q is not a registry server, like ghcr.io or registry.example.com:5000", s)
	}
	return s, nil
}
