package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func accessDoc(state deploy.AccessState) apitypes.Access {
	return apitypes.Access{
		Mode: state.Mode, Source: state.Source, Setting: state.Set.Mode, PasswordSet: state.Set.PasswordHash != "",
		PasswordMissing: state.PasswordMissing(), AskedInVain: state.AskedInVain(), Allow: append([]string{}, state.Set.Allow...), AnyUser: state.Set.AnyUser,
	}
}

func shareDoc(l store.ShareLink) apitypes.ShareLink {
	return apitypes.ShareLink{
		ID: l.ID, CreatedBy: l.CreatedBy, CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt, MaxUses: l.MaxUses, Uses: l.Uses,
		LastUsedAt: l.LastUsedAt, RevokedAt: l.RevokedAt,
	}
}

func (s *Server) accessFailed(w http.ResponseWriter, err error) bool {
	var invalid *deploy.InvalidAccessError
	switch {
	case err == nil:
		return false
	case errors.As(err, &invalid):
		problem(w, http.StatusUnprocessableEntity, "invalid_access", sentenceOf(err))
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", sentenceOf(err))
	default:
		s.internal(w, err)
	}
	return true
}

func (s *Server) access(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	state, err := s.deployer.Access(r.Context(), app, env)
	if s.accessFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, accessDoc(state))
}

func (s *Server) setAccess(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	var req apitypes.AccessChange
	if !decode(w, r, &req) {
		return
	}
	state, err := s.deployer.SetAccess(lasting(r), app, env, who(r).Name, deploy.AccessChange{Mode: req.Mode, Password: req.Password, Allow: req.Allow, AnyUser: req.AnyUser})
	if s.accessFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, accessDoc(state))
}

func (s *Server) shareLinks(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	list, err := s.db.ShareLinks(r.Context(), app, env)
	if s.accessFailed(w, err) {
		return
	}
	docs := make([]apitypes.ShareLink, 0, len(list))
	for _, l := range list {
		docs = append(docs, shareDoc(l))
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) createShareLink(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	var req apitypes.NewShareLink
	if !decode(w, r, &req) {
		return
	}
	var life time.Duration
	if req.Expires != "" {
		var d manifest.Duration
		if err := d.UnmarshalText([]byte(req.Expires)); err != nil {
			problem(w, http.StatusUnprocessableEntity, "invalid_access", sentenceOf(err))
			return
		}
		life = d.Std()
	}
	l, address, err := s.deployer.CreateShareLink(lasting(r), app, env, who(r).Name, life, req.MaxUses)
	if s.accessFailed(w, err) {
		return
	}
	doc := shareDoc(l)
	doc.URL = address
	writeJSON(w, http.StatusCreated, doc)
}

func (s *Server) revokeShareLink(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	if s.accessFailed(w, s.deployer.RevokeShareLink(lasting(r), app, env, r.PathValue("id"), who(r).Name)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
