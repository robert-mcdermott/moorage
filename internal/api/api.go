// Package api is Moorage's REST API under /api/v1 (spec/09-interfaces.md). Its
// routes are declared in routes.go, each with the permission it requires.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"sync"

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
	Restart(ctx context.Context, app, env string) error
	Instances(app, env string) []reconcile.InstanceInfo
}

// Server serves the API.
type Server struct {
	db       *store.DB
	cfg      config.Config
	dataDir  string
	auth     *auth.Service
	runtime  Runtime
	deployer *deploy.Deployer
	log      *slog.Logger
	// openapiDoc is the OpenAPI document, generated once.
	openapiDoc func() []byte
}

// New returns the API handler.
func New(db *store.DB, cfg config.Config, dataDir string, authn *auth.Service, runtime Runtime, deployer *deploy.Deployer, log *slog.Logger) http.Handler {
	s := &Server{db: db, cfg: cfg, dataDir: dataDir, auth: authn, runtime: runtime, deployer: deployer, log: log}
	s.openapiDoc = sync.OnceValue(func() []byte { return openAPIDocument(s.routes()) })
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.method+" "+basePath+rt.path, s.guard(rt))
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		problem(w, http.StatusNotFound, "not_found", "There is no such API endpoint.")
	})
	return mux
}

// basePath is where the API's routes are.
const basePath = "/api/v1"

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := pageParams(w, r, 100)
	if !ok {
		return
	}
	apps, err := s.db.AppsAfter(r.Context(), cursor, limit+1)
	if err != nil {
		s.internal(w, err)
		return
	}
	p := who(r)
	page := apitypes.Page[apitypes.App]{Items: []apitypes.App{}}
	for i, a := range apps {
		if i == limit {
			page.NextCursor = encodeCursor(apps[i-1].Name)
			break
		}
		if !p.CanApp(a.Name) {
			continue
		}
		app := apitypes.App{Name: a.Name, URL: s.appURL(a.Name), CreatedAt: a.CreatedAt}
		if a.Production != nil {
			rel := release(*a.Production)
			app.Production = &rel
		}
		page.Items = append(page.Items, app)
	}
	writeJSON(w, http.StatusOK, page)
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
		case apitypes.SourcePart:
			if plan != nil {
				problem(w, http.StatusBadRequest, "bad_request", "Send source alone, without a plan: the server works out how to build it.")
				return
			}
			dep, created, err := s.deployer.Submit(r.Context(), app, env, who(r).Name, part)
			var invalid *deploy.InvalidUploadError
			switch {
			case errors.As(err, &invalid):
				problem(w, http.StatusUnprocessableEntity, "invalid_upload", err.Error())
				return
			case err != nil:
				s.internal(w, err)
				return
			}
			s.log.Info("deployment queued", "app", app, "env", env, "deployment", dep.ID)
			d := s.deploymentDoc(dep)
			d.AppCreated = created
			w.Header().Set("Location", "/api/v1/deployments/"+dep.ID)
			writeJSON(w, http.StatusAccepted, d)
			return
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
			rel, dep, err := s.deployer.Publish(r.Context(), deploy.PublishRequest{
				App: app, Env: env, By: who(r).Name, Source: plan.Kind + "-upload", Reason: "deploy", Build: build, Plan: *plan,
			})
			if s.publishFailed(w, err) {
				return
			}
			s.log.Info("deployed", "app", app, "env", env, "release", rel.Version, "build", build.ID, "kind", plan.Kind)
			d := s.deploymentDoc(dep)
			d.AppCreated = created
			d.Release = s.releaseDoc(rel)
			writeJSON(w, http.StatusCreated, d)
			return
		default:
			problem(w, http.StatusBadRequest, "bad_request", "Unexpected form field "+part.FormName()+".")
			return
		}
	}
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
	limit, cursor, ok := pageParams(w, r, 50)
	if !ok {
		return
	}
	before := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 1 {
			problem(w, http.StatusBadRequest, "invalid_cursor", "That cursor is not one the API gave.")
			return
		}
		before = n
	}
	rels, err := s.db.ReleasesBefore(r.Context(), app, env, before, limit+1)
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	page := apitypes.Page[apitypes.Release]{Items: []apitypes.Release{}}
	for i, rel := range rels {
		if i == limit {
			page.NextCursor = encodeCursor(strconv.Itoa(rels[i-1].Version))
			break
		}
		page.Items = append(page.Items, release(rel))
	}
	writeJSON(w, http.StatusOK, page)
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
	buildID, version, err := s.db.RollbackBuild(r.Context(), app, env, req.Version)
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
	var cfg []store.ConfigVar
	if req.WithConfig {
		if cfg, err = s.db.ReleaseConfig(r.Context(), app, env, version); err != nil {
			s.internal(w, err)
			return
		}
		if cfg == nil {
			cfg = []store.ConfigVar{} // that release ran without server variables
		}
	}
	rel, dep, err := s.deployer.Publish(r.Context(), deploy.PublishRequest{
		App: app, Env: env, By: who(r).Name, Source: "rollback", Reason: "rollback", Build: build, Plan: plan, Config: cfg,
	})
	if s.publishFailed(w, err) {
		return
	}
	s.log.Info("rolled back", "app", app, "env", env, "release", rel.Version, "build", rel.BuildID)
	d := s.deploymentDoc(dep)
	d.Release = s.releaseDoc(rel)
	writeJSON(w, http.StatusOK, d)
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
	writeProblem(w, apitypes.Problem{Title: http.StatusText(status), Status: status, Detail: detail, Code: code})
}

func writeProblem(w http.ResponseWriter, p apitypes.Problem) {
	w.Header().Set("Content-Type", apitypes.ProblemType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
