package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/forge"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func (s *Server) forgeDoc(c store.ForgeConnection) apitypes.ForgeConnection {
	return apitypes.ForgeConnection{App: c.App, Kind: c.Kind, URL: c.URL, APIURL: c.APIURL, DefaultBranch: c.DefaultBranch, HookURL: s.cfg.PublicURL(s.cfg.ConsoleHost) + basePath + "/hooks/" + c.HookID,
		HasToken: len(c.SealedToken) > 0, Previews: c.Previews, Poll: c.Poll, LastEventAt: c.LastEventAt, LastError: c.LastError, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt}
}

func (s *Server) pullDoc(app string, p store.ForgePull) apitypes.ForgePull {
	doc := apitypes.ForgePull{Number: p.Number, Title: p.Title, Author: p.Author, FromFork: p.FromFork, Commit: p.HeadSHA, State: p.State, Env: p.Env, Error: p.Error, ApprovedBy: p.ApprovedBy, UpdatedAt: p.UpdatedAt}
	if p.State == store.PullLive && p.Env != "" {
		doc.URL = s.cfg.EnvURL(app, p.Env)
	}
	return doc
}

func (s *Server) forgeFailed(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *forge.InvalidError
	var moved *forge.CommitChangedError
	switch {
	case errors.As(err, &invalid):
		problem(w, http.StatusUnprocessableEntity, "invalid", invalid.Msg)
	case errors.As(err, &moved):
		problem(w, http.StatusConflict, "moved_on", moved.Error())
	case errors.Is(err, forge.ErrNoConnection), errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", "There is no such thing: "+err.Error()+".")
	default:
		s.internal(w, err)
	}
}

func (s *Server) getForge(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.ForgeOf(r.Context(), r.PathValue("app"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = forge.ErrNoConnection
		}
		s.forgeFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.forgeDoc(c))
}

func (s *Server) connectForge(w http.ResponseWriter, r *http.Request) {
	var in apitypes.ForgeConnect
	if !decode(w, r, &in) {
		return
	}
	app := r.PathValue("app")
	appID, err := s.db.AppID(r.Context(), app)
	if err != nil {
		s.forgeFailed(w, r, err)
		return
	}
	c, secret, err := s.forges.Connect(r.Context(), appID, app, forge.Connection{URL: in.URL, Kind: in.Kind, APIURL: in.APIURL, Token: in.Token, DefaultBranch: in.DefaultBranch, NoPreviews: in.NoPreviews, Poll: in.Poll}, who(r).Name)
	if err != nil {
		s.forgeFailed(w, r, err)
		return
	}
	doc := s.forgeDoc(c)
	doc.Secret = secret
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) disconnectForge(w http.ResponseWriter, r *http.Request) {
	if err := s.forges.Disconnect(r.Context(), r.PathValue("app")); err != nil {
		s.forgeFailed(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) forgePulls(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	c, err := s.db.ForgeOf(r.Context(), app)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = forge.ErrNoConnection
		}
		s.forgeFailed(w, r, err)
		return
	}
	pulls, err := s.db.ForgePulls(r.Context(), c.AppID)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]apitypes.ForgePull, 0, len(pulls))
	for _, p := range pulls {
		out = append(out, s.pullDoc(app, p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) approveForgePull(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		problem(w, http.StatusNotFound, "not_found", "There is no such pull request.")
		return
	}
	var in apitypes.ForgeApproval
	if !decode(w, r, &in) {
		return
	}
	app := r.PathValue("app")
	pull, err := s.forges.Approve(r.Context(), app, number, in.Commit, who(r).Name)
	if err != nil {
		s.forgeFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.pullDoc(app, pull))
}

func (s *Server) syncForge(w http.ResponseWriter, r *http.Request) {
	if err := s.forges.Sync(r.Context(), r.PathValue("app")); err != nil {
		s.forgeFailed(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// forgeHook takes what a forge sends. No one signs in for this: what it sends is signed with the hook's secret, and what
// it can ask for is only that the server looks at the repository.
func (s *Server) forgeHook(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(s.forges.Hook(r.Context(), r.PathValue("hook"), r.Header, r.Body))
}
