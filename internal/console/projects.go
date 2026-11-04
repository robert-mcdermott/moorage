package console

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/notify"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// projectRow is a line of the projects list.
type projectRow struct {
	Name, Role, Everyone string
	Apps                 int
}

// projectView is the page of a project.
type projectView struct {
	Project store.Project
	Role    string
	Manage  bool
	Apps    []string
	Members []store.Member
	// Candidates are who could be added, for those who may see everyone.
	Candidates []store.User
	// What the form said, when it was refused.
	FormEmail, FormName string
	// Where its notifications go, for those who may change that.
	Rules    []ruleRow
	Channels []string
	Events   []notify.RuleEvent
}

// Roles lists what someone can be in a project, for the pages' forms.
func (projectView) Roles() []string {
	return []string{store.RoleViewer, store.RoleDeployer, store.RoleMaintainer}
}

func (c *Console) projectsPage(w http.ResponseWriter, r *http.Request) {
	c.showProjects(w, r, http.StatusOK, "", nil)
}

func (c *Console) showProjects(w http.ResponseWriter, r *http.Request, code int, formName string, failed *failure) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	d, _, err := c.shellData(r, p, "projects", "Projects")
	if err != nil {
		c.internal(w, err)
		return
	}
	all, err := c.db.Projects(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	for _, project := range all {
		access, err := c.auth.ProjectAccess(r.Context(), p, project)
		if err != nil {
			c.internal(w, err)
			return
		}
		if access.See {
			d.Projects = append(d.Projects, projectRow{project.Name, access.Role, project.EveryoneRole, project.Apps})
		}
	}
	d.Name = formName
	d.Failure = failed
	c.render(w, code, "projects", d)
}

func (c *Console) createProject(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	project, err := c.db.CreateProject(r.Context(), name)
	var invalid *store.InvalidNameError
	switch {
	case errors.As(err, &invalid):
		c.showProjects(w, r, http.StatusUnprocessableEntity, name, &failure{Message: sentence(err.Error())})
	case errors.Is(err, store.ErrProjectExists):
		c.showProjects(w, r, http.StatusConflict, name, &failure{Message: sentence(err.Error())})
	case err != nil:
		c.internal(w, err)
	default:
		http.Redirect(w, r, "/projects/"+url.PathEscape(project.Name), http.StatusSeeOther)
	}
}

// showProject draws a project, for someone with a role in it.
func (c *Console) showProject(w http.ResponseWriter, r *http.Request, code int, view projectView, failed *failure) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	project, err := c.db.ProjectByName(r.Context(), r.PathValue("project"))
	notFound := func() {
		c.message(w, http.StatusNotFound, data{Title: "Not found", Heading: "There is no such project", Link: "/projects", LinkText: "Back to the projects"})
	}
	if errors.Is(err, store.ErrNotFound) {
		notFound()
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	access, err := c.auth.ProjectAccess(r.Context(), p, project)
	if err != nil {
		c.internal(w, err)
		return
	}
	if !access.See {
		notFound()
		return
	}
	d, apps, err := c.shellData(r, p, "projects", "Project "+project.Name)
	if err != nil {
		c.internal(w, err)
		return
	}
	d.Crumbs = []crumb{{"Projects", "/projects"}}
	view.Project, view.Role, view.Manage = project, access.Role, access.Manage
	for _, a := range apps {
		if a.Project == project.Name {
			view.Apps = append(view.Apps, a.Name)
		}
	}
	if view.Members, err = c.db.Members(r.Context(), project.Name); err != nil {
		c.internal(w, err)
		return
	}
	if view.Manage {
		if view.Rules, view.Channels, err = c.projectRules(r, project.Name, view.Apps); err != nil {
			c.internal(w, err)
			return
		}
		view.Events = notify.RuleEvents
	}
	d.Project = view
	d.Notice = projectNotices[r.URL.Query().Get("done")]
	d.Failure = failed
	c.render(w, code, "project", d)
}

var projectNotices = map[string]string{
	"member":       "They have that role now.",
	"removed":      "They are no longer a member.",
	"changed":      "The project is changed.",
	"rule":         "The rule is added.",
	"rule-removed": "The rule is removed.",
}

func (c *Console) projectPage(w http.ResponseWriter, r *http.Request) {
	c.showProject(w, r, http.StatusOK, projectView{}, nil)
}

// projectAction guards a form that changes a project: its maintainers, and administrators.
func (c *Console) projectAction(next func(http.ResponseWriter, *http.Request, auth.Principal, store.Project)) http.HandlerFunc {
	return c.accountAction(func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		project, err := c.db.ProjectByName(r.Context(), r.PathValue("project"))
		if err != nil {
			c.message(w, http.StatusNotFound, data{Title: "Not found", Heading: "There is no such project", Link: "/projects", LinkText: "Back to the projects"})
			return
		}
		access, err := c.auth.ProjectAccess(r.Context(), p, project)
		if err != nil {
			c.internal(w, err)
			return
		}
		if !access.See {
			c.message(w, http.StatusNotFound, data{Title: "Not found", Heading: "There is no such project", Link: "/projects", LinkText: "Back to the projects"})
			return
		}
		if !access.Manage {
			c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "You may not do that", Message: "It takes being a maintainer of this project.", Link: "/projects/" + url.PathEscape(project.Name), LinkText: "Back to the project"})
			return
		}
		next(w, r, p, project)
	})
}

func (c *Console) changeProjectSettings(w http.ResponseWriter, r *http.Request, p auth.Principal, project store.Project) {
	everyone := r.PostFormValue("everyone")
	if everyone == "none" {
		everyone = ""
	}
	if everyone != "" && !store.ValidRole(everyone) {
		c.showProject(w, r, http.StatusUnprocessableEntity, projectView{}, &failure{Message: "A role is viewer, deployer or maintainer, or none."})
		return
	}
	if err := c.db.SetProject(r.Context(), project.Name, everyone, r.PostFormValue("viewers_see_logs") != ""); err != nil {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/projects/"+url.PathEscape(project.Name)+"?done=changed", http.StatusSeeOther)
}

func (c *Console) setProjectMember(w http.ResponseWriter, r *http.Request, p auth.Principal, project store.Project) {
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	role := r.PostFormValue("role")
	if !store.ValidRole(role) {
		c.showProject(w, r, http.StatusUnprocessableEntity, projectView{FormEmail: email}, &failure{Message: "A role is viewer, deployer or maintainer."})
		return
	}
	u, err := c.db.UserByEmail(r.Context(), email)
	if errors.Is(err, store.ErrNotFound) {
		c.showProject(w, r, http.StatusUnprocessableEntity, projectView{FormEmail: email}, &failure{Message: "There is no account with that address. An administrator can add them under Settings, Users."})
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	if err := c.db.SetMember(r.Context(), project.Name, u.ID, role); err != nil {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/projects/"+url.PathEscape(project.Name)+"?done=member", http.StatusSeeOther)
}

func (c *Console) removeProjectMember(w http.ResponseWriter, r *http.Request, p auth.Principal, project store.Project) {
	if err := c.db.RemoveMember(r.Context(), project.Name, r.PostFormValue("user")); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/projects/"+url.PathEscape(project.Name)+"?done=removed", http.StatusSeeOther)
}

func (c *Console) deleteProject(w http.ResponseWriter, r *http.Request, p auth.Principal, project store.Project) {
	if !p.Admin {
		c.showProject(w, r, http.StatusForbidden, projectView{}, &failure{Message: "Deleting a project takes an administrator."})
		return
	}
	err := c.db.DeleteProject(r.Context(), project.Name)
	if errors.Is(err, store.ErrProjectNotEmpty) || errors.Is(err, store.ErrDefaultProject) {
		c.showProject(w, r, http.StatusConflict, projectView{}, &failure{Message: sentence(err.Error())})
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// auditPage is who did what, for administrators.
func (c *Console) auditPage(w http.ResponseWriter, r *http.Request) {
	_, d, ok := c.adminData(w, r, "audit", "Audit log")
	if !ok {
		return
	}
	q := r.URL.Query()
	const pageSize = 50
	filter := store.AuditFilter{Actor: strings.TrimSpace(q.Get("actor")), Action: strings.TrimSpace(q.Get("action")), Target: strings.TrimSpace(q.Get("target")), Before: q.Get("before"), Limit: pageSize + 1}
	list, err := c.db.Audit(r.Context(), filter)
	if err != nil {
		c.internal(w, err)
		return
	}
	params := url.Values{}
	for name, value := range map[string]string{"actor": filter.Actor, "action": filter.Action, "target": filter.Target} {
		if value != "" {
			params.Set(name, value)
		}
	}
	view := settingsView{FormActor: filter.Actor, FormAction: filter.Action, FormTarget: filter.Target}
	view.AuditExport = "/api/v1/audit?" + params.Encode()
	if strings.HasSuffix(view.AuditExport, "?") {
		view.AuditExport += "format=ndjson"
	} else {
		view.AuditExport += "&format=ndjson"
	}
	if len(list) > pageSize {
		list = list[:pageSize]
		params.Set("before", list[pageSize-1].ID)
		d.Older = "/settings/audit?" + params.Encode()
	}
	view.Audit = list
	d.Settings = view
	c.render(w, http.StatusOK, "settings-audit", d)
}

// recorder notes how a form ended, for the audit log.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

// audited keeps a record of a form that changes something, whether or not it worked.
// Nothing of what the form holds goes in the record.
func (c *Console) audited(w http.ResponseWriter, r *http.Request, p auth.Principal, next func(http.ResponseWriter)) {
	rec := &recorder{ResponseWriter: w}
	next(rec)
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	result := "ok"
	switch {
	case status == http.StatusForbidden || status == http.StatusUnauthorized || status == http.StatusNotFound:
		result = "refused"
	case status >= 400:
		result = "failed"
	}
	c.auth.Audit(r.Context(), store.AuditEntry{
		Actor: p.Name, IP: c.auth.ClientIP(r), Action: r.Pattern, Target: r.URL.Path, Result: result,
		Data: map[string]any{"via": "console", "status": status},
	})
}

// newAppPage asks for a name and a project.
func (c *Console) newAppPage(w http.ResponseWriter, r *http.Request) {
	c.showNewApp(w, r, http.StatusOK, "", "", nil)
}

func (c *Console) showNewApp(w http.ResponseWriter, r *http.Request, code int, name, project string, failed *failure) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	d, _, err := c.shellData(r, p, "apps", "New app")
	if err != nil {
		c.internal(w, err)
		return
	}
	d.Crumbs = []crumb{{"Apps", "/"}}
	all, err := c.db.Projects(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	for _, candidate := range all {
		access, err := c.auth.ProjectAccess(r.Context(), p, candidate)
		if err != nil {
			c.internal(w, err)
			return
		}
		if access.Deploy {
			d.Projects = append(d.Projects, projectRow{Name: candidate.Name, Role: access.Role, Apps: candidate.Apps})
		}
	}
	d.Name, d.Image = name, project
	d.Failure = failed
	c.render(w, code, "new-app", d)
}

// createApp makes an app, in a project its maker may deploy to.
func (c *Console) createApp(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	projectName := r.PostFormValue("project")
	if projectName == "" {
		projectName = store.DefaultProject
	}
	refuse := func(code int, message string) {
		c.showNewApp(w, r, code, name, projectName, &failure{Message: message})
	}
	if err := names.ValidateApp(name); err != nil {
		refuse(http.StatusUnprocessableEntity, sentence(err.Error()))
		return
	}
	project, err := c.db.ProjectByName(r.Context(), projectName)
	if err != nil {
		refuse(http.StatusUnprocessableEntity, "There is no such project.")
		return
	}
	access, err := c.auth.ProjectAccess(r.Context(), p, project)
	if err != nil {
		c.internal(w, err)
		return
	}
	if !access.Deploy {
		refuse(http.StatusForbidden, "You may not make apps in that project: it takes being a deployer of it.")
		return
	}
	_, created, err := c.db.CreateApp(r.Context(), name, projectName)
	if err != nil {
		c.internal(w, err)
		return
	}
	if !created {
		refuse(http.StatusConflict, "There is already an app of that name.")
		return
	}
	http.Redirect(w, r, "/apps/"+url.PathEscape(name)+"/deploy", http.StatusSeeOther)
}
