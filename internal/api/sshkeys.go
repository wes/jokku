package api

import (
	"context"
	"net/http"

	"github.com/wes/jokku/internal/sshkeys"
	"github.com/wes/jokku/internal/types"
)

func (s *Server) listSSHKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Store.SSHKeys(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

func (s *Server) addSSHKey(w http.ResponseWriter, r *http.Request) {
	var req types.AddSSHKeyRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !sshkeys.ValidName.MatchString(req.Name) {
		s.fail(w, r, badRequest("Key name %q is invalid: use letters, digits, '.', '_', '@' and '-'", req.Name))
		return
	}
	pub, fp, err := sshkeys.Parse(req.PublicKey)
	if err != nil {
		s.fail(w, r, badRequest("%v", err))
		return
	}
	key := types.SSHKey{Name: req.Name, Fingerprint: fp, PublicKey: pub}
	if err := s.Store.AddSSHKey(r.Context(), key); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.SyncAuthorizedKeys(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, key)
}

func (s *Server) removeSSHKey(w http.ResponseWriter, r *http.Request) {
	byFingerprint := r.URL.Query().Get("fingerprint") == "true"
	if err := s.Store.RemoveSSHKey(r.Context(), r.PathValue("name"), byFingerprint); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.SyncAuthorizedKeys(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SyncAuthorizedKeys rewrites the jokku user's authorized_keys from the
// database. The daemon also calls it at startup to repair drift.
func (s *Server) SyncAuthorizedKeys(ctx context.Context) error {
	if s.AuthorizedKeys == "" {
		return nil
	}
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	keys, err := s.Store.SSHKeys(ctx)
	if err != nil {
		return err
	}
	return sshkeys.Write(s.AuthorizedKeys, s.Exe, keys, s.Owner)
}
