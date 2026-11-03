// Package api is Moorage's REST API under /api/v1 (docs/09-interfaces.md).
// Until accounts arrive (wave 2), every request needs the bootstrap admin
// token as a bearer token.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// MaxUpload bounds the size of an upload's request body.
const MaxUpload = 1 << 30

// Runtime runs process apps.
type Runtime interface {
	Rollout(ctx context.Context, app, env string, rel store.Release, build store.Build, plan manifest.Plan) error
	Restart(ctx context.Context, app, env string) error
	Retire(app, env string)
	Instances(app, env string) []reconcile.InstanceInfo
}

// Server serves the API.
type Server struct {
	db        *store.DB
	cfg       config.Config
	dataDir   string
	tokenHash [32]byte
	runtime   Runtime
	// reload makes the router pick up a new release.
	reload func(context.Context) error
	log    *slog.Logger
}

// New returns the API handler.
func New(db *store.DB, cfg config.Config, dataDir string, tokenHash [32]byte, runtime Runtime, reload func(context.Context) error, log *slog.Logger) http.Handler {
	s := &Server{db: db, cfg: cfg, dataDir: dataDir, tokenHash: tokenHash, runtime: runtime, reload: reload, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/apps", s.listApps)
	mux.HandleFunc("POST /api/v1/apps/{app}/envs/{env}/deployments", s.deploy)
	mux.HandleFunc("GET /api/v1/apps/{app}/envs/{env}/releases", s.releases)
	mux.HandleFunc("POST /api/v1/apps/{app}/envs/{env}/rollback", s.rollback)
	mux.HandleFunc("POST /api/v1/apps/{app}/envs/{env}/restart", s.restart)
	mux.HandleFunc("GET /api/v1/apps/{app}/envs/{env}/instances", s.instances)
	mux.HandleFunc("GET /api/v1/apps/{app}/envs/{env}/logs", s.logs)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		problem(w, http.StatusNotFound, "not_found", "There is no such API endpoint.")
	})
	return s.authenticate(mux)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !auth.Matches(strings.TrimSpace(token), s.tokenHash) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="moorage"`)
			problem(w, http.StatusUnauthorized, "unauthorized", "A valid token is required (Authorization: Bearer <token>).")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.db.Apps(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]apitypes.App, 0, len(apps))
	for _, a := range apps {
		app := apitypes.App{Name: a.Name, URL: s.appURL(a.Name), CreatedAt: a.CreatedAt}
		if a.Production != nil {
			rel := release(*a.Production)
			app.Production = &rel
		}
		out = append(out, app)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "multipart/form-data" {
		problem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Send the deploy as multipart/form-data with a plan part and an archive part.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		problem(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var plan *manifest.Plan
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			problem(w, http.StatusBadRequest, "missing_archive", "The upload has no archive part.")
			return
		}
		if err != nil {
			problem(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		switch part.FormName() {
		case apitypes.PlanPart:
			plan = &manifest.Plan{}
			if err := json.NewDecoder(io.LimitReader(part, 1<<20)).Decode(plan); err != nil {
				problem(w, http.StatusBadRequest, "bad_request", "The plan part is not valid JSON: "+err.Error())
				return
			}
			if err := plan.Validate(); err != nil {
				problem(w, http.StatusUnprocessableEntity, "invalid_plan", err.Error())
				return
			}
		case apitypes.ArchivePart:
			if plan == nil {
				problem(w, http.StatusBadRequest, "missing_plan", "Send the plan part before the archive.")
				return
			}
			build, created, err := deploy.Upload(r.Context(), s.db, s.dataDir, app, *plan, part)
			var invalid *deploy.InvalidUploadError
			switch {
			case errors.As(err, &invalid):
				problem(w, http.StatusUnprocessableEntity, "invalid_upload", err.Error())
				return
			case err != nil:
				s.internal(w, err)
				return
			}
			rel, err := s.publish(r.Context(), app, env, build, *plan, "deploy")
			if s.publishFailed(w, err) {
				return
			}
			s.log.Info("deployed", "app", app, "env", env, "release", rel.Version, "build", build.ID, "kind", plan.Kind)
			writeJSON(w, http.StatusCreated, apitypes.Deployment{
				App: app, Env: env, URL: s.appURL(app), AppCreated: created, Release: release(rel),
			})
			return
		default:
			problem(w, http.StatusBadRequest, "bad_request", "Unexpected form field "+part.FormName()+".")
			return
		}
	}
}

// publish makes build the environment's current release. A process app's
// release becomes current only once its instances are running and healthy;
// if they are not, the release is removed and the previous one keeps
// serving.
func (s *Server) publish(ctx context.Context, app, env string, build store.Build, plan manifest.Plan, reason string) (store.Release, error) {
	rel, err := s.db.CreateRelease(ctx, app, env, build.ID, reason, "admin")
	if err != nil {
		return rel, err
	}
	if plan.Kind == manifest.KindProcess {
		if err := s.runtime.Rollout(ctx, app, env, rel, build, plan); err != nil {
			s.db.DeleteRelease(context.WithoutCancel(ctx), rel.ID)
			return rel, err
		}
	}
	if err := s.db.MakeCurrent(ctx, rel.ID); err != nil {
		return rel, err
	}
	rel.Current = true
	if err := s.reload(ctx); err != nil {
		return rel, err
	}
	if plan.Kind == manifest.KindStatic {
		s.runtime.Retire(app, env) // processes of an earlier release are no longer needed
	}
	return rel, nil
}

// publishFailed answers a failed publish, reporting false if it did not
// fail.
func (s *Server) publishFailed(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var rollout *reconcile.RolloutError
	if errors.As(err, &rollout) {
		w.Header().Set("Content-Type", apitypes.ProblemType)
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(apitypes.Problem{
			Title: "Deployment failed", Status: http.StatusUnprocessableEntity, Code: "rollout_failed",
			Detail: rollout.Message + "; the previous release keeps serving", Output: rollout.Output,
		})
		return true
	}
	s.internal(w, err)
	return true
}

func (s *Server) releases(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	rels, err := s.db.Releases(r.Context(), app, env)
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]apitypes.Release, 0, len(rels))
	for _, rel := range rels {
		out = append(out, release(rel))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	var req apitypes.RollbackRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			problem(w, http.StatusBadRequest, "bad_request", "The body is not valid JSON: "+err.Error())
			return
		}
	}
	buildID, _, err := s.db.RollbackBuild(r.Context(), app, env, req.Version)
	switch {
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", err.Error())
		return
	case err != nil:
		problem(w, http.StatusConflict, "cannot_roll_back", err.Error())
		return
	}
	build, err := s.db.Build(r.Context(), buildID)
	if err != nil {
		s.internal(w, err)
		return
	}
	var plan manifest.Plan
	if err := json.Unmarshal(build.Plan, &plan); err != nil {
		s.internal(w, err)
		return
	}
	rel, err := s.publish(r.Context(), app, env, build, plan, "rollback")
	if s.publishFailed(w, err) {
		return
	}
	s.log.Info("rolled back", "app", app, "env", env, "release", rel.Version, "build", rel.BuildID)
	writeJSON(w, http.StatusOK, apitypes.Deployment{App: app, Env: env, URL: s.appURL(app), Release: release(rel)})
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	err := s.runtime.Restart(r.Context(), app, env)
	switch {
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", err.Error())
		return
	case err != nil:
		var rollout *reconcile.RolloutError
		if errors.As(err, &rollout) {
			s.publishFailed(w, err)
			return
		}
		problem(w, http.StatusConflict, "cannot_restart", err.Error())
		return
	}
	s.log.Info("restarted", "app", app, "env", env)
	writeJSON(w, http.StatusOK, s.instanceList(app, env))
}

func (s *Server) instances(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.instanceList(app, env))
}

func (s *Server) instanceList(app, env string) []apitypes.Instance {
	out := []apitypes.Instance{}
	for _, in := range s.runtime.Instances(app, env) {
		ai := apitypes.Instance{
			ID: in.ID, Process: in.Process, Index: in.Index, Release: in.Version, Port: in.Port,
			Status: in.Status, Healthy: in.Healthy, PID: in.PID, Restarts: in.Restarts, StartedAt: in.StartedAt,
		}
		if in.LastExit != nil {
			ai.LastExit = &apitypes.Exit{Code: in.LastExit.Code, Signal: in.LastExit.Signal, At: in.LastExit.At}
		}
		out = append(out, ai)
	}
	return out
}

// target reads and checks the {app} and {env} path values.
func (s *Server) target(w http.ResponseWriter, r *http.Request) (app, env string, ok bool) {
	app, env = r.PathValue("app"), r.PathValue("env")
	if err := names.ValidateApp(app); err != nil {
		problem(w, http.StatusUnprocessableEntity, "invalid_app_name", err.Error())
		return "", "", false
	}
	if env != names.Production {
		problem(w, http.StatusUnprocessableEntity, "unsupported_environment",
			"Only the production environment exists in this version; named environments and previews arrive later.")
		return "", "", false
	}
	return app, env, true
}

func (s *Server) appURL(app string) string {
	return s.cfg.PublicURL(app + "." + s.cfg.Domain)
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.log.Error("API request failed", "err", err)
	problem(w, http.StatusInternalServerError, "internal", "The server could not complete the request; its log has the details.")
}

func release(r store.Release) apitypes.Release {
	return apitypes.Release{
		Version: r.Version, Build: r.BuildID, Reason: r.Reason, Files: r.Files, Size: r.SizeBytes,
		CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, Current: r.Current,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", apitypes.ProblemType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apitypes.Problem{Title: http.StatusText(status), Status: status, Detail: detail, Code: code})
}
