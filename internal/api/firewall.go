package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

func (s *Server) getFirewall(w http.ResponseWriter, r *http.Request) {
	fw, err := s.Cluster.Firewall(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, fw)
}

// patchFirewall changes the firewall's rules, or (with an app) whether
// it's on for one app.
func (s *Server) patchFirewall(w http.ResponseWriter, r *http.Request) {
	var p types.FirewallPatch
	if err := decode(r, &p); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Cluster.SetFirewall(r.Context(), r.PathValue("app"), p); err != nil {
		s.fail(w, r, firewallErr(err))
		return
	}
	settings, err := s.Cluster.FirewallSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) unbanFirewall(w http.ResponseWriter, r *http.Request) {
	var u types.FirewallUnban
	if err := decode(r, &u); err != nil {
		s.fail(w, r, err)
		return
	}
	had, err := s.Cluster.Unban(r.Context(), u.Addr)
	if err != nil {
		s.fail(w, r, firewallErr(err))
		return
	}
	writeJSON(w, http.StatusOK, types.FirewallUnbanned{Banned: had})
}

func firewallErr(err error) error {
	var bad *cluster.BadSetting
	if errors.As(err, &bad) {
		return badRequest("%s", bad.Error())
	}
	return err
}

// agentFirewall hears from a node's proxy what its firewall did.
func (s *Server) agentFirewall(w http.ResponseWriter, r *http.Request, n *store.Node) {
	var rep types.FirewallReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&rep); err != nil {
		s.fail(w, r, badRequest("invalid firewall report: %v", err))
		return
	}
	if err := s.Cluster.FirewallReport(r.Context(), n.Name, rep); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
