package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// vars lists an environment's variables: those set on the server and the
// manifest's [env] of the current release, each marked overridden when a
// later source sets the same name. Secrets' values are never returned.
func (s *Server) vars(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	out, err := deploy.ListVars(r.Context(), s.db, app, env)
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// changeVars sets and unsets variables, and releases the change unless
// asked not to.
func (s *Server) changeVars(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	var req apitypes.VarsChange
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "The body is not a valid change of variables: "+err.Error())
		return
	}
	ch := deploy.ConfigChange{Unset: req.Unset, AllEnvs: req.AllEnvs, Restart: !req.NoRestart, By: who(r).Name}
	for _, v := range req.Set {
		ch.Set = append(ch.Set, deploy.VarValue{Name: v.Name, Value: v.Value, Secret: v.Secret, Build: v.Build})
	}
	res, err := s.deployer.Configure(r.Context(), app, env, ch)
	var invalid *deploy.InvalidConfigError
	var rollout *reconcile.RolloutError
	switch {
	case errors.As(err, &invalid):
		problem(w, http.StatusUnprocessableEntity, "invalid_variables", err.Error())
		return
	case errors.As(err, &rollout):
		s.publishFailed(w, err)
		return
	case err != nil:
		s.internal(w, err)
		return
	}
	out := apitypes.VarsResult{App: app, Env: env, AppCreated: res.AppCreated, NotSet: res.NotSet}
	if res.Release != nil {
		d := s.deploymentDoc(res.Deployment)
		d.Release = s.releaseDoc(*res.Release)
		out.Deployment = &d
	}
	writeJSON(w, http.StatusOK, out)
}
