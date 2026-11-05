package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"fmt"
	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func (s *Server) projectDoc(r *http.Request, project store.Project) (apitypes.Project, auth.Access, error) {
	access, err := s.auth.ProjectAccess(r.Context(), who(r), project)
	if err != nil {
		return apitypes.Project{}, access, err
	}
	return apitypes.Project{
		Name: project.Name, Role: access.Role, Everyone: project.EveryoneRole, ViewersSeeLogs: project.ViewersSeeLogs, CreatedAt: project.CreatedAt,
	}, access, nil
}

// projects lists the projects the caller has a role in.
func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	all, err := s.db.Projects(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	docs := []apitypes.Project{}
	for _, project := range all {
		doc, access, err := s.projectDoc(r, project)
		if err != nil {
			s.internal(w, err)
			return
		}
		if access.See {
			docs = append(docs, doc)
		}
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req apitypes.NewProject
	if !decode(w, r, &req) {
		return
	}
	project, err := s.db.CreateProject(r.Context(), strings.TrimSpace(req.Name))
	if !s.projectFailed(w, err) {
		doc, _, err := s.projectDoc(r, project)
		if err != nil {
			s.internal(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, doc)
	}
}

// projectFailed answers what went wrong with a project, and says whether anything did.
// noProject is what a project someone may not see looks like: the same as one that is not there.
func noProject(name string) error {
	return fmt.Errorf("no project %q: %w", name, store.ErrNotFound)
}

func (s *Server) projectFailed(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrProjectExists):
		problem(w, http.StatusConflict, "project_exists", "There is already a project of that name.")
	case errors.Is(err, store.ErrProjectNotEmpty), errors.Is(err, store.ErrDefaultProject):
		problem(w, http.StatusConflict, "cannot_delete", sentenceOf(err))
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", sentenceOf(err))
	default:
		var invalid *store.InvalidNameError
		if errors.As(err, &invalid) {
			problem(w, http.StatusUnprocessableEntity, "invalid_name", sentenceOf(err))
			return true
		}
		s.internal(w, err)
	}
	return true
}

func sentenceOf(err error) string {
	text := err.Error()
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:] + "."
}

// lookupProject reads the {project} path value, and answers if the caller has no role in it.
func (s *Server) lookupProject(w http.ResponseWriter, r *http.Request) (store.Project, auth.Access, bool) {
	project, err := s.db.ProjectByName(r.Context(), r.PathValue("project"))
	if s.projectFailed(w, err) {
		return project, auth.Access{}, false
	}
	access, err := s.auth.ProjectAccess(r.Context(), who(r), project)
	if err != nil {
		s.internal(w, err)
		return project, access, false
	}
	if !access.See {
		s.projectFailed(w, noProject(project.Name))
		return project, access, false
	}
	return project, access, true
}

// writeProject answers with a project, its apps and its members.
func (s *Server) writeProject(w http.ResponseWriter, r *http.Request, name string, status int) {
	project, err := s.db.ProjectByName(r.Context(), name)
	if s.projectFailed(w, err) {
		return
	}
	doc, _, err := s.projectDoc(r, project)
	if err != nil {
		s.internal(w, err)
		return
	}
	members, err := s.db.Members(r.Context(), name)
	if err != nil {
		s.internal(w, err)
		return
	}
	doc.Members = []apitypes.Member{}
	for _, m := range members {
		doc.Members = append(doc.Members, apitypes.Member{Email: m.Email, Name: m.Name, Role: m.Role})
	}
	apps, err := s.db.Apps(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	for _, a := range apps {
		if a.Project == name {
			doc.Apps = append(doc.Apps, a.Name)
		}
	}
	writeJSON(w, status, doc)
}

func (s *Server) project(w http.ResponseWriter, r *http.Request) {
	if project, _, ok := s.lookupProject(w, r); ok {
		s.writeProject(w, r, project.Name, http.StatusOK)
	}
}

// manageProject reads a project whose members the caller may change.
func (s *Server) manageProject(w http.ResponseWriter, r *http.Request) (store.Project, bool) {
	project, access, ok := s.lookupProject(w, r)
	if ok && !access.Manage {
		problem(w, http.StatusForbidden, "forbidden", "A "+access.Role+" of the project "+project.Name+" may not do this: it takes a maintainer.")
		return project, false
	}
	return project, ok
}

func (s *Server) changeProject(w http.ResponseWriter, r *http.Request) {
	project, ok := s.manageProject(w, r)
	if !ok {
		return
	}
	var req apitypes.ProjectChange
	if !decode(w, r, &req) {
		return
	}
	everyone, logs := project.EveryoneRole, project.ViewersSeeLogs
	if req.Everyone != nil {
		everyone = *req.Everyone
		if everyone == "none" {
			everyone = ""
		}
		if everyone != "" && !store.ValidRole(everyone) {
			problem(w, http.StatusUnprocessableEntity, "invalid_role", "A role is viewer, deployer or maintainer, or none.")
			return
		}
	}
	if req.ViewersSeeLogs != nil {
		logs = *req.ViewersSeeLogs
	}
	if s.projectFailed(w, s.db.SetProject(r.Context(), project.Name, everyone, logs)) {
		return
	}
	s.writeProject(w, r, project.Name, http.StatusOK)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	if s.projectFailed(w, s.db.DeleteProject(r.Context(), r.PathValue("project"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setMember(w http.ResponseWriter, r *http.Request) {
	project, ok := s.manageProject(w, r)
	if !ok {
		return
	}
	u, ok := s.lookupUser(w, r)
	if !ok {
		return
	}
	var req apitypes.MemberChange
	if !decode(w, r, &req) {
		return
	}
	if !store.ValidRole(req.Role) {
		problem(w, http.StatusUnprocessableEntity, "invalid_role", "A role is viewer, deployer or maintainer.")
		return
	}
	if s.projectFailed(w, s.db.SetMember(r.Context(), project.Name, u.ID, req.Role)) {
		return
	}
	s.writeProject(w, r, project.Name, http.StatusOK)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	project, ok := s.manageProject(w, r)
	if !ok {
		return
	}
	u, ok := s.lookupUser(w, r)
	if !ok {
		return
	}
	if s.projectFailed(w, s.db.RemoveMember(r.Context(), project.Name, u.ID)) {
		return
	}
	s.writeProject(w, r, project.Name, http.StatusOK)
}

// createApp makes an app in a project the caller may deploy to.
func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	var req apitypes.NewApp
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if err := names.ValidateApp(name); err != nil {
		problem(w, http.StatusUnprocessableEntity, "invalid_app_name", err.Error())
		return
	}
	projectName := req.Project
	if projectName == "" {
		projectName = store.DefaultProject
	}
	project, err := s.db.ProjectByName(r.Context(), projectName)
	if s.projectFailed(w, err) {
		return
	}
	access, err := s.auth.ProjectAccess(r.Context(), who(r), project)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !access.See {
		s.projectFailed(w, noProject(projectName))
		return
	}
	if !access.Deploy {
		problem(w, http.StatusForbidden, "forbidden", "A "+access.Role+" of the project "+projectName+" may not create apps: it takes a deployer.")
		return
	}
	_, created, err := s.db.CreateApp(r.Context(), name, projectName)
	if s.projectFailed(w, err) {
		return
	}
	if !created {
		problem(w, http.StatusConflict, "app_exists", "There is already an app named "+name+".")
		return
	}
	writeJSON(w, http.StatusCreated, apitypes.App{Name: name, URL: s.appURL(name), Project: projectName})
}

// appInfo says how an app stands in the project, and how much room it takes.
func (s *Server) appInfo(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	project, err := s.db.AppProject(r.Context(), app)
	if s.projectFailed(w, err) {
		return
	}
	// What production holds is not for a token limited to previews, even how much of it there is.
	if who(r).Envs != "" {
		writeJSON(w, http.StatusOK, apitypes.AppInfo{App: apitypes.App{Name: app, URL: s.appURL(app), Project: project.Name}})
		return
	}
	usage := s.deployer.Usage(app)
	writeJSON(w, http.StatusOK, apitypes.AppInfo{
		App:    apitypes.App{Name: app, URL: s.appURL(app), Project: project.Name},
		Builds: usage.Builds, Logs: usage.Logs, Repository: usage.Repository, Data: usage.Data,
	})
}

// changeApp moves an app to another project, which takes managing both, or renames it.
func (s *Server) changeApp(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var req apitypes.AppChange
	if !decode(w, r, &req) {
		return
	}
	if req.Project != "" {
		project, err := s.db.ProjectByName(r.Context(), req.Project)
		if s.projectFailed(w, err) {
			return
		}
		access, err := s.auth.ProjectAccess(r.Context(), who(r), project)
		if err != nil {
			s.internal(w, err)
			return
		}
		if !access.See {
			s.projectFailed(w, noProject(req.Project))
			return
		}
		if !access.Manage {
			problem(w, http.StatusForbidden, "forbidden", "Moving an app to "+req.Project+" takes being a maintainer of it.")
			return
		}
		if s.projectFailed(w, s.db.MoveApp(r.Context(), app, req.Project)) {
			return
		}
	}
	if req.Name != "" {
		if s.lifecycleFailed(w, s.deployer.RenameApp(lasting(r), app, req.Name, who(r).Name)) {
			return
		}
		app = req.Name
	}
	project, err := s.db.AppProject(r.Context(), app)
	if s.projectFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, apitypes.App{Name: app, URL: s.appURL(app), Project: project.Name})
}

// lasting is the context of what must not stop halfway if whoever asked goes away.
func lasting(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

// lifecycleFailed answers what went wrong with renaming or deleting an app.
func (s *Server) lifecycleFailed(w http.ResponseWriter, err error) bool {
	var (
		busy    *deploy.AppBusyError
		taken   *deploy.NameTakenError
		invalid *deploy.InvalidNameError
		rollout *reconcile.RolloutError
	)
	switch {
	case err == nil:
		return false
	case errors.As(err, &busy):
		problem(w, http.StatusConflict, "app_busy", sentenceOf(err))
	case errors.As(err, &taken):
		problem(w, http.StatusConflict, "name_taken", sentenceOf(err))
	case errors.As(err, &invalid):
		problem(w, http.StatusUnprocessableEntity, "invalid_app_name", err.Error())
	case errors.As(err, &rollout):
		writeProblem(w, apitypes.Problem{
			Title: "Starting it again failed", Status: http.StatusUnprocessableEntity, Code: "restart_failed",
			Detail: "It has its new name, and it could not start under it: " + rollout.Message, Output: rollout.Output,
		})
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", sentenceOf(err))
	default:
		s.internal(w, err)
	}
	return true
}

// deleteApp removes an app, and everything of it unless ?keep_data=true.
func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	keep := r.URL.Query().Get("keep_data") == "true"
	if s.lifecycleFailed(w, s.deployer.DeleteApp(lasting(r), r.PathValue("app"), who(r).Name, keep)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revealVar reads a secret's value, and keeps a record of who did.
func (s *Server) revealVar(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	value, err := s.deployer.RevealVar(r.Context(), app, env, name)
	result := "ok"
	if err != nil {
		result = "failed"
	}
	s.auth.Audit(r.Context(), store.AuditEntry{Actor: who(r).Name, IP: s.auth.ClientIP(r), Action: "secret.reveal", Target: app + "/" + env + "/" + name, Result: result})
	var notSecret *deploy.NotSecretError
	switch {
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", "There is no secret "+name+".")
	case errors.As(err, &notSecret):
		problem(w, http.StatusConflict, "not_secret", notSecret.Error())
	case err != nil:
		s.internal(w, err)
	default:
		writeJSON(w, http.StatusOK, apitypes.RevealedVar{Name: name, Value: value})
	}
}

func auditDoc(e store.AuditEntry) apitypes.AuditEntry {
	return apitypes.AuditEntry{ID: e.ID, At: e.At, Actor: e.Actor, IP: e.IP, Action: e.Action, Target: e.Target, Result: e.Result, Data: e.Data}
}

// auditLog lists who did what, or, with ?format=ndjson, all of it to keep.
func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := pageParams(w, r, 50)
	if !ok {
		return
	}
	q := r.URL.Query()
	filter := store.AuditFilter{Actor: q.Get("actor"), Action: q.Get("action"), Target: q.Get("target"), Before: cursor, Limit: limit + 1}
	if q.Get("format") == "ndjson" {
		s.exportAudit(w, r, filter)
		return
	}
	list, err := s.db.Audit(r.Context(), filter)
	if err != nil {
		s.internal(w, err)
		return
	}
	page := apitypes.Page[apitypes.AuditEntry]{Items: []apitypes.AuditEntry{}}
	for i, e := range list {
		if i == limit {
			page.NextCursor = encodeCursor(list[i-1].ID)
			break
		}
		page.Items = append(page.Items, auditDoc(e))
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) exportAudit(w http.ResponseWriter, r *http.Request, filter store.AuditFilter) {
	w.Header().Set("Content-Type", apitypes.NDJSONType)
	w.Header().Set("Content-Disposition", `attachment; filename="moorage-audit.ndjson"`)
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	filter.Limit = 500
	for {
		list, err := s.db.Audit(r.Context(), filter)
		if err != nil {
			s.log.Error("export the audit log", "err", err)
			return
		}
		for _, e := range list {
			if enc.Encode(auditDoc(e)) != nil {
				return
			}
		}
		if len(list) < filter.Limit {
			return
		}
		filter.Before = list[len(list)-1].ID
	}
}
