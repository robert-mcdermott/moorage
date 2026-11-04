package api

import (
	"errors"
	"net/http"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func (s *Server) domainDoc(d store.Domain) apitypes.Domain {
	return apitypes.Domain{
		Address: d.Address(), Hostname: d.Hostname, Prefix: d.Prefix, StripPrefix: d.StripPrefix, Verified: d.Verified(), VerifiedAt: d.VerifiedAt,
		CheckedAt: d.CheckedAt, Problem: d.CheckError, Record: s.deployer.DomainRecord(d.App, d.Env, d.Hostname),
	}
}

func (s *Server) domainFailed(w http.ResponseWriter, err error) bool {
	var invalid *deploy.InvalidDomainError
	switch {
	case err == nil:
		return false
	case errors.As(err, &invalid):
		problem(w, http.StatusUnprocessableEntity, "invalid_domain", sentenceOf(err))
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", sentenceOf(err))
	default:
		s.internal(w, err)
	}
	return true
}

func (s *Server) domains(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	list, err := s.db.Domains(r.Context(), app, env)
	if s.domainFailed(w, err) {
		return
	}
	docs := make([]apitypes.Domain, 0, len(list))
	for _, d := range list {
		docs = append(docs, s.domainDoc(d))
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) addDomain(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	var req apitypes.NewDomain
	if !decode(w, r, &req) {
		return
	}
	d, err := s.deployer.AddDomain(lasting(r), app, env, who(r).Name, req.Address, req.StripPrefix)
	if s.domainFailed(w, err) {
		return
	}
	d.App, d.Env = app, env
	writeJSON(w, http.StatusCreated, s.domainDoc(d))
}

func (s *Server) verifyDomain(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	var req apitypes.NewDomain
	if !decode(w, r, &req) {
		return
	}
	d, err := s.deployer.VerifyDomain(lasting(r), app, env, req.Address)
	if s.domainFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, s.domainDoc(d))
}

func (s *Server) removeDomain(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	var req apitypes.NewDomain
	if !decode(w, r, &req) {
		return
	}
	if s.domainFailed(w, s.deployer.RemoveDomain(lasting(r), app, env, who(r).Name, req.Address)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
