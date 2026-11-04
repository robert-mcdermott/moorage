package api

import (
	"errors"
	"net/http"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/serverconfig"
)

func (s *Server) serverSettings() *serverconfig.Service {
	keys, _ := s.keyring.(serverconfig.Keys)
	return &serverconfig.Service{DB: s.db, Auth: s.auth, Keyring: keys, Notifier: s.notifier, Backups: s.backupsService, SSO: s.signIn}
}

func (s *Server) exportConfig(w http.ResponseWriter, r *http.Request) {
	f, err := s.serverSettings().Export(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) importConfig(w http.ResponseWriter, r *http.Request) {
	var req apitypes.ServerConfig
	if !decode(w, r, &req) {
		return
	}
	report, err := s.serverSettings().Import(r.Context(), req, r.URL.Query().Get("dry_run") == "true", who(r).Name, s.auth.ClientIP(r))
	var invalid *serverconfig.InvalidError
	if errors.As(err, &invalid) {
		problem(w, http.StatusUnprocessableEntity, "invalid_config", invalid.Msg)
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
