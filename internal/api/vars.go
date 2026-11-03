package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/manifest"
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
	set, err := s.db.Vars(r.Context(), app, env)
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	out := []apitypes.Var{}
	envNames := map[string]bool{}
	serverNames := map[string]bool{}
	for _, v := range set {
		if !v.AllEnvs {
			envNames[v.Name] = true
		}
		serverNames[v.Name] = true
	}
	for _, v := range set {
		doc := apitypes.Var{Name: v.Name, Secret: v.Secret, Build: v.Build, Source: "env", UpdatedBy: v.UpdatedBy, UpdatedAt: v.UpdatedAt}
		if !v.Secret {
			doc.Value = v.Value
		}
		if v.AllEnvs {
			doc.Source = "app"
			doc.Overridden = envNames[v.Name]
		}
		out = append(out, doc)
	}
	if _, build, err := s.db.CurrentRelease(r.Context(), app, env); err == nil {
		var plan manifest.Plan
		if json.Unmarshal(build.Plan, &plan) == nil {
			for name, value := range plan.Env {
				out = append(out, apitypes.Var{Name: name, Value: value, Source: "manifest", Overridden: serverNames[name]})
			}
		}
	}
	order := map[string]int{"env": 0, "app": 1, "manifest": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return order[out[i].Source] < order[out[j].Source]
	})
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
	ch := deploy.ConfigChange{Unset: req.Unset, AllEnvs: req.AllEnvs, Restart: !req.NoRestart, By: "admin"}
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
