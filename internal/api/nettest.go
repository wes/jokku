package api

import (
	"errors"
	"net/http"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/types"
)

func (s *Server) getNetTests(w http.ResponseWriter, r *http.Request) {
	tests, err := s.Cluster.NetTests(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tests)
}

// startNetTest starts a network test and answers at once; GET follows it.
func (s *Server) startNetTest(w http.ResponseWriter, r *http.Request) {
	var req types.NetTestRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.Cluster.StartNetTest(r.Context(), req)
	var refused *cluster.NetTestRefused
	var bad *cluster.BadSetting
	switch {
	case errors.As(err, &refused):
		err = httpErrorf(http.StatusConflict, "%s", refused.Error())
	case errors.As(err, &bad):
		err = badRequest("%s", bad.Error())
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, p)
}
