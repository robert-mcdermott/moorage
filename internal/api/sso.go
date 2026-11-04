package api

import (
	"errors"
	"net/http"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/sso"
)

func (s *Server) ssoConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.SSOConfig(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apitypes.SSOConfig{
		Issuer: cfg.Issuer, ClientID: cfg.ClientID, HasSecret: len(cfg.SealedSecret) > 0, RedirectURL: s.signIn.RedirectURL(), Label: cfg.Label, Scopes: cfg.Scopes,
		EmailDomains: cfg.EmailDomains, Provision: cfg.Provision, GroupClaim: cfg.GroupClaim, GroupRules: cfg.GroupRules, PasswordsOff: cfg.PasswordsOff,
	})
}

func (s *Server) configureSSO(w http.ResponseWriter, r *http.Request) {
	var req apitypes.SSOConfig
	if !decode(w, r, &req) {
		return
	}
	err := s.signIn.Configure(r.Context(), sso.Form{Issuer: req.Issuer, ClientID: req.ClientID, ClientSecret: req.ClientSecret, Label: req.Label, Scopes: req.Scopes, EmailDomains: req.EmailDomains,
		Provision: req.Provision, GroupClaim: req.GroupClaim, GroupRules: req.GroupRules, PasswordsOff: req.PasswordsOff}, who(r).Name)
	var invalid *sso.InvalidError
	if errors.As(err, &invalid) {
		problem(w, http.StatusUnprocessableEntity, "invalid_sso", invalid.Msg)
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	s.ssoConfig(w, r)
}

func (s *Server) turnOffSSO(w http.ResponseWriter, r *http.Request) {
	if err := s.signIn.TurnOff(r.Context()); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
