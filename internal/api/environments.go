package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/services"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// describeEnv fills in what the store says of an environment.
func (s *Server) describeEnv(ctx context.Context, doc *apitypes.Environment, e store.Environment) {
	doc.Kind, doc.Branch, doc.Pinned, doc.LockedBy, doc.LastActive = e.Kind, e.Branch, e.Pinned, e.LockedBy, e.LastActive
	doc.Asleep = !e.AsleepAt.IsZero()
	doc.StandsAlone = standsAlone(e)
	if e.Kind == store.KindPreview && !e.Pinned {
		doc.ExpiresAt = s.deployer.EnvironmentActive(ctx, e).Add(s.deployer.PreviewTTL(ctx, e.App, e.Name))
	}
}

func (s *Server) environmentSummary(ctx context.Context, e store.Environment) apitypes.Environment {
	doc := apitypes.Environment{App: e.App, Name: e.Name, URL: s.envURL(e.App, e.Name), Instances: []apitypes.Instance{}}
	s.describeEnv(ctx, &doc, e)
	if e.Version > 0 {
		doc.Release = &apitypes.Release{Version: e.Version, CreatedAt: e.ReleasedAt, CreatedBy: e.ReleasedBy, Current: true}
	}
	return doc
}

func (s *Server) environments(w http.ResponseWriter, r *http.Request) {
	list, err := s.db.Environments(r.Context(), r.PathValue("app"))
	if err != nil {
		s.internal(w, err)
		return
	}
	if len(list) == 0 {
		problem(w, http.StatusNotFound, "not_found", "There is no app "+r.PathValue("app")+".")
		return
	}
	docs := make([]apitypes.Environment, 0, len(list))
	for _, e := range list {
		if who(r).Envs != "" && !standsAlone(e) {
			continue
		}
		docs = append(docs, s.environmentSummary(r.Context(), e))
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) previews(w http.ResponseWriter, r *http.Request) {
	list, err := s.db.Previews(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	visible, err := s.auth.Visible(r.Context(), who(r))
	if err != nil {
		s.internal(w, err)
		return
	}
	docs := []apitypes.Environment{}
	for _, e := range list {
		if visible(e.App) {
			docs = append(docs, s.environmentSummary(r.Context(), e))
		}
	}
	writeJSON(w, http.StatusOK, docs)
}

// environmentFailed answers what went wrong with an environment.
func (s *Server) environmentFailed(w http.ResponseWriter, err error) bool {
	var (
		production *deploy.CannotDeleteProductionError
		busy       *deploy.AppBusyError
		locked     *deploy.LockedError
		preview    *deploy.NotPreviewableError
		invalid    *store.InvalidNameError
		redeploy   *deploy.CannotRedeployError
		sleepless  *deploy.NotSleepableError
	)
	switch {
	case err == nil:
		return false
	case errors.As(err, &production), errors.As(err, &busy), errors.As(err, &locked), errors.As(err, &preview), errors.As(err, &redeploy), errors.As(err, &sleepless):
		problem(w, http.StatusConflict, "conflict", sentenceOf(err))
	case errors.Is(err, container.ErrNotAnswering), errors.Is(err, container.ErrNoEngine):
		problem(w, http.StatusConflict, "no_container_engine", sentenceOf(err))
	case errors.As(err, &invalid):
		problem(w, http.StatusUnprocessableEntity, "invalid_environment_name", err.Error())
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", sentenceOf(err))
	default:
		return s.publishFailed(w, err)
	}
	return true
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var req apitypes.NewEnvironment
	if !decode(w, r, &req) {
		return
	}
	created, err := s.db.CreateEnvironment(r.Context(), app, req.Name, store.KindNamed, req.Branch)
	if s.environmentFailed(w, err) {
		return
	}
	if !created {
		problem(w, http.StatusConflict, "environment_exists", "There is already an environment named "+req.Name+".")
		return
	}
	doc, err := s.environmentDoc(r.Context(), app, req.Name)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, doc)
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	// Deleting a branch destroys its preview, so a deployer may do that here too; the
	// environments that stay are a maintainer's to remove.
	kind, err := s.db.EnvKind(r.Context(), app, env)
	if s.environmentFailed(w, err) {
		return
	}
	if kind != store.KindPreview {
		rt := route{perm: canManage}
		why := refused(rt, who(r), app)
		if why == "" {
			access, err := s.auth.AppAccess(r.Context(), who(r), app)
			if err != nil {
				s.internal(w, err)
				return
			}
			why = roleRefused(rt, access)
		}
		if why != "" {
			problem(w, http.StatusForbidden, "forbidden", why)
			return
		}
	}
	if s.environmentFailed(w, s.deployer.DeleteEnvironment(lasting(r), app, env, who(r).Name)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) promote(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	var req apitypes.Promotion
	if !decode(w, r, &req) {
		return
	}
	warnings := s.deployer.PromotionWarnings(r.Context(), app, req.From, env)
	rel, dep, err := s.deployer.Promote(r.Context(), app, req.From, env, who(r).Name)
	if s.environmentFailed(w, err) {
		return
	}
	d := s.deploymentDoc(dep)
	d.Release = s.releaseDoc(rel)
	d.Warnings = warnings
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) lock(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok || s.environmentFailed(w, s.deployer.Lock(r.Context(), app, env, who(r).Name)) {
		return
	}
	s.environment(w, r)
}

func (s *Server) unlock(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok || s.environmentFailed(w, s.deployer.Unlock(r.Context(), app, env, who(r).Name)) {
		return
	}
	s.environment(w, r)
}

func (s *Server) sleep(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok || s.environmentFailed(w, s.deployer.Sleep(lasting(r), app, env, who(r).Name)) {
		return
	}
	s.environment(w, r)
}

func (s *Server) wake(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok || s.environmentFailed(w, s.deployer.Wake(lasting(r), app, env, who(r).Name)) {
		return
	}
	s.environment(w, r)
}

func (s *Server) createPreview(w http.ResponseWriter, r *http.Request) {
	var req apitypes.NewPreview
	if !decode(w, r, &req) {
		return
	}
	dep, err := s.deployer.PreviewBranch(r.Context(), r.PathValue("app"), req.Branch, who(r).Name)
	if s.environmentFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, s.deploymentDoc(dep))
}

func (s *Server) services(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	rows, err := s.deployer.ServiceList(r.Context(), app, env)
	if s.environmentFailed(w, err) {
		return
	}
	docs := make([]apitypes.Service, 0, len(rows))
	for _, row := range rows {
		docs = append(docs, apitypes.Service{
			Name: row.Name, Type: row.Type, Version: row.Version, Image: row.Image, Variable: row.EnvVar, Port: row.Port, State: row.State,
			Running: row.Running, Unknown: row.Unknown, MemoryBytes: row.Memory, Unused: row.Unused, OldVolume: row.OldVolume, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) serviceLogs(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	tail := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("tail")); err == nil && n > 0 {
		tail = min(n, 5000)
	}
	text, err := s.deployer.ServiceLogs(r.Context(), app, env, r.PathValue("name"), tail)
	if s.environmentFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, apitypes.ServiceLog{Log: text})
}

func (s *Server) removeService(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	if s.environmentFailed(w, s.deployer.RemoveService(lasting(r), app, env, r.PathValue("name"), who(r).Name)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateService(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	var log bytes.Buffer
	digest, err := s.deployer.UpdateService(lasting(r), app, env, r.PathValue("name"), &log)
	if s.serviceFailed(w, err) {
		return
	}
	said := "It was up to date.\n"
	if digest != "" {
		said = "It now runs " + digest + ".\n"
	}
	writeJSON(w, http.StatusOK, apitypes.ServiceChange{Log: said})
}

func (s *Server) upgradeService(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	var req apitypes.ServiceUpgrade
	if !decode(w, r, &req) {
		return
	}
	var log bytes.Buffer
	if s.serviceFailed(w, s.deployer.UpgradeService(lasting(r), app, env, r.PathValue("name"), req.Version, req.Image, &log)) {
		return
	}
	writeJSON(w, http.StatusOK, apitypes.ServiceChange{Log: log.String()})
}

func (s *Server) backService(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	if s.serviceFailed(w, s.deployer.BackService(lasting(r), app, env, r.PathValue("name"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) finishServiceUpgrade(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	if s.serviceFailed(w, s.deployer.FinishServiceUpgrade(lasting(r), app, env, r.PathValue("name"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) restoreService(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	if s.serviceFailed(w, s.deployer.RestoreService(lasting(r), app, env, r.PathValue("name"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serviceFailed(w http.ResponseWriter, err error) bool {
	var major *services.MajorChangeError
	var locked *deploy.LockedError
	var busy *deploy.AppBusyError
	var rollout *reconcile.RolloutError
	switch {
	case errors.As(err, &major):
		problem(w, http.StatusConflict, "conflict", sentenceOf(err))
		return true
	case errors.As(err, &rollout):
		problem(w, http.StatusUnprocessableEntity, "app_did_not_start", "The app did not start again after that: "+sentenceOf(err))
		return true
	case err == nil, errors.As(err, &locked), errors.As(err, &busy), errors.Is(err, context.Canceled), errors.Is(err, store.ErrNotFound),
		errors.Is(err, container.ErrNotAnswering), errors.Is(err, container.ErrNoEngine):
		return s.environmentFailed(w, err)
	}
	// Who may ask for these may know why one went wrong, in the words of the engine or of PostgreSQL, which they would otherwise have to find in the server's log.
	s.log.Error("a service could not do what was asked", "err", err)
	problem(w, http.StatusInternalServerError, "service_failed", sentenceOf(err))
	return true
}
