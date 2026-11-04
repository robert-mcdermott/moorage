package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/shim"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Runtime is what these pages ask of what runs the apps.
type Runtime interface {
	Instances(app, env string) []reconcile.InstanceInfo
}

// status is how something stands, as a badge shows it: its kind is its look.
type status struct{ Label, Kind string }

// appView is the page of an app.
type appView struct {
	Name, URL string
	Status    status
	// LastFailed says that the newest deployment failed, and a release keeps serving.
	LastFailed string
	Release    *releaseView
	Static     bool
	Processes  []processView
	Recent     []deploymentRow
	Older      string
	Events     []eventRow
	// Deploying says that a deployment is under way.
	Deploying bool
	// Previous is the release a rollback goes back to, if there is one to serve.
	Previous int
	// Engine says whether this server can run containers.
	Engine bool
}

// releaseView is what an environment serves.
type releaseView struct {
	Version    int
	By, Source string
	At         time.Time
	Deployment string
}

// processView is a process of an app, and its instances.
type processView struct {
	Name string
	// Want is how many should run, and Manifest how many the manifest says.
	Want, Manifest int
	Instances      []instanceView
}

// Overridden says that scaling set another number than the manifest's.
func (p processView) Overridden() bool { return p.Want != p.Manifest }

// instanceView is a line of the instances table.
type instanceView struct {
	Name     string
	Status   status
	Version  int
	Port     int
	PID      int
	Restarts int
	Started  time.Time
	LastExit string
	// Replaced says that it belongs to another release than the current one: a
	// rollout is under way.
	Replaced bool
}

// inProgress is a status of a deployment under way.
func inProgress(d store.Deployment) bool { return !store.Final(d.Status) }

// appStatus says how an app stands, from its newest deployment and its instances.
func appStatus(hasRelease, static bool, want, healthy int, last *store.Deployment) status {
	switch {
	case last != nil && inProgress(*last):
		return status{"Deploying", "info pulse"}
	case !hasRelease && last != nil && last.Status == store.Failed:
		return status{"Deploy failed", "fail"}
	case !hasRelease:
		return status{"Not deployed", ""}
	case static:
		return status{"Live", "ok"}
	case want == 0:
		return status{"Scaled to zero", ""}
	case healthy >= want:
		return status{"Healthy", "ok"}
	case healthy == 0:
		return status{"Down", "fail"}
	}
	return status{fmt.Sprintf("%d of %d healthy", healthy, want), "warn"}
}

func instanceStatus(in reconcile.InstanceInfo) status {
	switch {
	case in.Healthy:
		return status{"Healthy", "ok"}
	case in.Status == shim.Running:
		return status{"Unhealthy", "warn"}
	case in.Status == "lost" || in.Status == "unknown":
		return status{sentence(in.Status), "fail"}
	}
	return status{sentence(in.Status), "warn"}
}

func exitText(in reconcile.InstanceInfo) string {
	if in.LastExit == nil {
		return ""
	}
	what := fmt.Sprintf("status %d", in.LastExit.Code)
	switch in.LastExit.Signal {
	case "":
	case "OOM":
		what = "out of memory"
	default:
		what = "signal " + in.LastExit.Signal
	}
	return what + ", " + ago(in.LastExit.At)
}

func readPlan(b store.Build) (manifest.Plan, error) {
	var plan manifest.Plan
	if err := json.Unmarshal(b.Plan, &plan); err != nil {
		return plan, err
	}
	plan.Defaults()
	return plan, nil
}

// wanted counts the instances a plan runs under a formation.
func wanted(plan manifest.Plan, formation map[string]int) int {
	n := 0
	for _, proc := range plan.WithFormation(formation).Processes {
		n += *proc.Instances
	}
	return n
}

func (c *Console) instances(app string) []reconcile.InstanceInfo {
	if c.runtime == nil {
		return nil
	}
	return c.runtime.Instances(app, names.Production)
}

// appRows draws the apps list: each with how it stands.
func (c *Console) appRows(ctx context.Context, apps []store.AppSummary) ([]appRow, error) {
	routes, err := c.db.Routes(ctx)
	if err != nil {
		return nil, err
	}
	plans := map[string]manifest.Plan{}
	for _, r := range routes {
		if r.Env != names.Production {
			continue
		}
		if plan, err := readPlan(r.Build); err == nil {
			plans[r.App] = plan
		}
	}
	latest, err := c.db.LatestDeployments(ctx, names.Production)
	if err != nil {
		return nil, err
	}
	rows := make([]appRow, 0, len(apps))
	for _, a := range apps {
		row := appRow{Name: a.Name, URL: c.appURL(a.Name), Project: a.Project}
		plan, known := plans[a.Name]
		static := known && plan.Kind != manifest.KindProcess
		want, healthy := 0, 0
		if known && !static {
			formation, err := c.db.Formation(ctx, a.Name, names.Production)
			if err != nil {
				return nil, err
			}
			want = wanted(plan, formation)
			for _, in := range c.instances(a.Name) {
				if in.Healthy {
					healthy++
				}
			}
			row.Instances = fmt.Sprintf("%d of %d", min(healthy, want), want)
		}
		if a.Production != nil {
			row.Version, row.ChangedAt, row.ChangedBy = a.Production.Version, a.Production.CreatedAt, a.Production.CreatedBy
		}
		var last *store.Deployment
		if d, ok := latest[a.Name]; ok {
			last = &d
			row.Last, row.ChangedAt, row.ChangedBy = sourceText(d), d.CreatedAt, d.CreatedBy
			row.LastFailed = d.Status == store.Failed && a.Production != nil
			row.Deploying = inProgress(d)
		}
		row.Status = appStatus(a.Production != nil, static, want, healthy, last)
		rows = append(rows, row)
	}
	return rows, nil
}

func (c *Console) appsPage(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	d, apps, err := c.shellData(r, p, "apps", "Apps")
	if err != nil {
		c.internal(w, err)
		return
	}
	if d.Apps, err = c.appRows(r.Context(), apps); err != nil {
		c.internal(w, err)
		return
	}
	d.RefreshOn = "*"
	for _, row := range d.Apps {
		d.Poll = d.Poll || row.Deploying
	}
	c.render(w, http.StatusOK, "apps", d)
}

// lookupApp reads the {app} path value, and answers if there is no such app that the
// viewer can see.
func (c *Console) lookupApp(w http.ResponseWriter, r *http.Request, p auth.Principal) (string, bool) {
	app := r.PathValue("app")
	if names.ValidateApp(app) != nil {
		c.noSuchApp(w, app)
		return "", false
	}
	if _, err := c.db.EnvID(r.Context(), app, names.Production); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.noSuchApp(w, app)
		} else {
			c.internal(w, err)
		}
		return "", false
	}
	access, err := c.auth.AppAccess(r.Context(), p, app)
	if err != nil {
		c.internal(w, err)
		return "", false
	}
	if !access.See {
		c.noSuchApp(w, app)
		return "", false
	}
	return app, true
}

func (c *Console) noSuchApp(w http.ResponseWriter, app string) {
	c.message(w, http.StatusNotFound, data{Title: "Not found", Heading: "There is no such app", Message: "It may have another name, or it has not been deployed yet.", Link: "/new", LinkText: "Start a new app"})
}

// notices are what an action may say on the page it returns to. The address names
// one: nothing of a request's own is shown.
var notices = map[string]string{
	"restarted": "Its instances were replaced.",
	"scaled":    "It now runs as many instances as you asked.",
	"unchanged": "It already runs that many instances.",
}

func (c *Console) appPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	c.showApp(w, r, p, app, http.StatusOK, nil)
}

// failure is why an action did not work, on the page it was asked from.
type failure struct {
	Message string
	Output  []string
}

func (c *Console) showApp(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, code int, failed *failure) {
	d, err := c.appData(r, p, app, "apps", app)
	if err != nil {
		c.internal(w, err)
		return
	}
	d.Crumbs = []crumb{{"Apps", "/"}}
	d.Tab = "overview"
	view, err := c.appView(r.Context(), app)
	if err != nil {
		c.internal(w, err)
		return
	}
	d.App = view
	d.Poll = view.Deploying
	d.RefreshOn = app
	d.Notice = notices[r.URL.Query().Get("done")]
	d.Failure = failed
	c.render(w, code, "app", d)
}

// recentDeployments is how many the app's page shows.
const recentDeployments = 5

func (c *Console) appView(ctx context.Context, app string) (appView, error) {
	view := appView{Name: app, URL: c.appURL(app), Engine: c.hasEngine()}
	deps, err := c.db.Deployments(ctx, app, names.Production, recentDeployments+1)
	if err != nil {
		return view, err
	}
	var last *store.Deployment
	if len(deps) > 0 {
		last = &deps[0]
		view.Deploying = inProgress(deps[0])
	}
	rel, b, err := c.db.CurrentRelease(ctx, app, names.Production)
	hasRelease := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return view, err
	}
	current := 0
	want, healthy := 0, 0
	if hasRelease {
		current = rel.Version
		plan, err := readPlan(b)
		if err != nil {
			return view, err
		}
		view.Static = plan.Kind != manifest.KindProcess
		view.Release = &releaseView{Version: rel.Version, By: rel.CreatedBy, At: rel.CreatedAt}
		if made, err := c.db.BuildDeployment(ctx, b.ID); err == nil {
			view.Release.Source = sourceText(made)
		}
		for _, d := range deps {
			if d.Version == rel.Version {
				view.Release.Deployment = d.ID
			}
		}
		if !view.Static {
			formation, err := c.db.Formation(ctx, app, names.Production)
			if err != nil {
				return view, err
			}
			want = wanted(plan, formation)
			view.Processes = c.processViews(app, rel.Version, plan, formation)
			for _, proc := range view.Processes {
				for _, in := range proc.Instances {
					if in.Status.Kind == "ok" && !in.Replaced {
						healthy++
					}
				}
			}
		}
		if rel.Version > 1 {
			if _, prev, err := c.db.ReleaseVersion(ctx, app, names.Production, rel.Version-1); err == nil && !prev.Collected {
				view.Previous = rel.Version - 1
			}
		}
	}
	view.Status = appStatus(hasRelease, view.Static, want, healthy, last)
	if hasRelease && last != nil && last.Status == store.Failed {
		view.LastFailed = last.ID
	}
	if len(deps) > recentDeployments {
		deps = deps[:recentDeployments]
		view.Older = "/apps/" + url.PathEscape(app) + "/deployments"
	}
	for _, dep := range deps {
		view.Recent = append(view.Recent, deploymentRowOf(dep, current))
	}
	list, err := c.db.Events(ctx, store.EventFilter{App: app, Limit: 8})
	if err != nil {
		return view, err
	}
	for _, e := range list {
		view.Events = append(view.Events, eventRowOf(e))
	}
	return view, nil
}

func (c *Console) processViews(app string, version int, plan manifest.Plan, formation map[string]int) []processView {
	byName := map[string][]instanceView{}
	for _, in := range c.instances(app) {
		byName[in.Process] = append(byName[in.Process], instanceView{
			Name: fmt.Sprintf("%s.%d", in.Process, in.Index), Status: instanceStatus(in), Version: in.Version,
			Port: in.Port, PID: in.PID, Restarts: in.Restarts, Started: in.StartedAt, LastExit: exitText(in),
			Replaced: in.Version != version,
		})
	}
	scaled := plan.WithFormation(formation)
	var out []processView
	for _, name := range manifest.SortedProcesses(plan.Processes) {
		list := byName[name]
		sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		out = append(out, processView{
			Name: name, Want: *scaled.Processes[name].Instances, Manifest: *plan.Processes[name].Instances, Instances: list,
		})
	}
	return out
}

// action guards a form that changes an app: it must come from the console's own pages,
// with the session's token, from someone who may deploy this app. Until roles arrive
// (wave 4) that is everyone signed in; the API's routes ask the same of a principal.
func (c *Console) action(next func(http.ResponseWriter, *http.Request, auth.Principal, string)) http.HandlerFunc {
	return c.actionWith(auth.ScopeDeploy, maxForm, next)
}

// actionWith is action, for a form that needs another scope or may be longer.
func (c *Console) actionWith(scope string, limit int64, next func(http.ResponseWriter, *http.Request, auth.Principal, string)) http.HandlerFunc {
	return c.postUpTo(limit, func(w http.ResponseWriter, r *http.Request) {
		p, ok := c.formUser(w, r)
		if !ok {
			return
		}
		if !auth.CheckCSRF(p.CookieValue, r.PostFormValue("csrf")) {
			c.refused(w)
			return
		}
		app, ok := c.lookupApp(w, r, p)
		if !ok {
			return
		}
		access, err := c.auth.AppAccess(r.Context(), p, app)
		if err != nil {
			c.internal(w, err)
			return
		}
		if scope == auth.ScopeAdmin && !access.Manage || scope == auth.ScopeDeploy && !access.Deploy {
			c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "You may not do that", Message: "Your role in the project " + access.Project.Name + " does not allow it.", Link: "/apps/" + url.PathEscape(app), LinkText: "Back to the app"})
			return
		}
		c.audited(w, r, p, func(w http.ResponseWriter) { next(w, r, p, app) })
	})
}

// formUser returns who is signed in, or sends them to sign in and on to the app's
// page: a form's own address has no page to come back to.
func (c *Console) formUser(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := c.signedIn(r)
	if !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/apps/"+url.PathEscape(r.PathValue("app"))), http.StatusSeeOther)
	}
	return p, ok
}

func (c *Console) refused(w http.ResponseWriter) {
	c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "That request was refused", Message: "It did not come from this site's own pages.", Link: "/", LinkText: "Back to the start"})
}

// lasting is the context of an action: it goes on if whoever asked closes the page.
func lasting(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

// actionFailed shows why an action did not work, or that the server could not do it.
func (c *Console) actionFailed(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, err error) {
	var (
		rollout  *reconcile.RolloutError
		scale    *reconcile.ScaleError
		back     *deploy.CannotRollBackError
		redeploy *deploy.CannotRedeployError
	)
	switch {
	case errors.As(err, &rollout):
		c.showApp(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: sentence(rollout.Message) + " It keeps running as it was.", Output: rollout.Output})
	case errors.As(err, &scale):
		c.showApp(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: sentence(scale.Error())})
	case errors.As(err, &back):
		c.showApp(w, r, p, app, http.StatusConflict, &failure{Message: sentence(back.Error())})
	case errors.As(err, &redeploy):
		c.showApp(w, r, p, app, http.StatusConflict, &failure{Message: sentence(redeploy.Error())})
	case errors.Is(err, store.ErrNotFound):
		c.showApp(w, r, p, app, http.StatusConflict, &failure{Message: "Nothing is deployed yet."})
	default:
		c.internal(w, err)
	}
}

func (c *Console) restart(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	if err := c.deployer.Restart(lasting(r), app, names.Production, p.Name); err != nil {
		var rollout *reconcile.RolloutError
		if errors.As(err, &rollout) || errors.Is(err, store.ErrNotFound) {
			c.actionFailed(w, r, p, app, err)
			return
		}
		// What the API answers as it stands: a static site has nothing to restart.
		c.showApp(w, r, p, app, http.StatusConflict, &failure{Message: sentence(err.Error())})
		return
	}
	http.Redirect(w, r, "/apps/"+url.PathEscape(app)+"?done=restarted", http.StatusSeeOther)
}

func (c *Console) redeploy(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	dep, err := c.deployer.Redeploy(lasting(r), app, names.Production, p.Name)
	if err != nil {
		c.actionFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, "/deployments/"+dep.ID, http.StatusSeeOther)
}

func (c *Console) rollback(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	version, err := strconv.Atoi(r.PostFormValue("version"))
	if err != nil || version < 0 {
		c.showApp(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: "That is not a release to go back to."})
		return
	}
	_, dep, err := c.deployer.Rollback(lasting(r), app, names.Production, p.Name, version, r.PostFormValue("with_config") != "")
	if err != nil {
		c.actionFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, "/deployments/"+dep.ID, http.StatusSeeOther)
}

// scalePrefix begins the form's field for a process's number.
const scalePrefix = "process."

// scale changes the numbers that differ from what runs: one left alone stays the
// manifest's to decide.
func (c *Console) scale(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	_, b, err := c.db.CurrentRelease(r.Context(), app, names.Production)
	if err != nil {
		c.actionFailed(w, r, p, app, err)
		return
	}
	plan, err := readPlan(b)
	if err != nil {
		c.internal(w, err)
		return
	}
	formation, err := c.db.Formation(r.Context(), app, names.Production)
	if err != nil {
		c.internal(w, err)
		return
	}
	running := plan.WithFormation(formation).Processes
	change := map[string]int{}
	for field, values := range r.PostForm {
		name, ok := strings.CutPrefix(field, scalePrefix)
		if !ok || len(values) == 0 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(values[0]))
		if err != nil {
			c.showApp(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: fmt.Sprintf("How many instances of %s? That is not a number.", name)})
			return
		}
		if proc, known := running[name]; !known || *proc.Instances != n {
			change[name] = n
		}
	}
	if len(change) == 0 {
		http.Redirect(w, r, "/apps/"+url.PathEscape(app)+"?done=unchanged", http.StatusSeeOther)
		return
	}
	if _, err := c.deployer.Scale(lasting(r), app, names.Production, p.Name, change); err != nil {
		c.actionFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, "/apps/"+url.PathEscape(app)+"?done=scaled", http.StatusSeeOther)
}

func (c *Console) hasEngine() bool { return c.deployer != nil && c.deployer.HasEngine() }

// deployPage says how to deploy an app, which need not exist yet: the first deployment
// creates it.
func (c *Console) deployPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app := r.PathValue("app")
	if names.ValidateApp(app) != nil {
		c.noSuchApp(w, app)
		return
	}
	access, err := c.auth.AppAccess(r.Context(), p, app)
	if err != nil {
		c.internal(w, err)
		return
	}
	if !access.See {
		c.noSuchApp(w, app)
		return
	}
	if !access.Deploy {
		c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "You may not do that", Message: "Your role in the project " + access.Project.Name + " does not allow deploying.", Link: "/apps/" + url.PathEscape(app), LinkText: "Back to the app"})
		return
	}
	c.showDeploy(w, r, p, app, http.StatusOK, "", "")
}

func (c *Console) showDeploy(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, code int, problem, image string) {
	d, err := c.appData(r, p, app, "apps", "Deploy "+app)
	if err != nil {
		c.internal(w, err)
		return
	}
	d.Crumbs = []crumb{{"Apps", "/"}}
	d.Tab = "deploy"
	exists := true
	if _, err := c.db.EnvID(r.Context(), app, names.Production); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			c.internal(w, err)
			return
		}
		exists = false
	}
	if exists {
		d.Crumbs = append(d.Crumbs, crumb{app, "/apps/" + url.PathEscape(app)})
	}
	d.App = appView{Name: app, URL: c.appURL(app), Engine: c.hasEngine()}
	d.Heading = "Deploy " + app
	d.Error = problem
	d.Image = image
	d.Commands = []string{"moor deploy --app " + app}
	d.GitCommands = []string{"moor keys add", "moor git remote --app " + app, "git push moorage main"}
	d.LoginCommand = "moor login " + c.cfg.PublicURL(c.cfg.ConsoleHost)
	d.MaxUpload = humanSize(deploy.MaxUpload)
	c.render(w, code, "deploy", d)
}

func humanSize(n int64) string {
	const gib = 1 << 30
	if n%gib == 0 {
		return strconv.FormatInt(n/gib, 10) + " GiB"
	}
	return strconv.FormatInt(n>>20, 10) + " MiB"
}

// deployImage deploys an image by its name.
func (c *Console) deployImage(w http.ResponseWriter, r *http.Request) {
	p, app, ok := c.deployForm(w, r)
	if !ok {
		return
	}
	image := strings.TrimSpace(r.PostFormValue("image"))
	if err := manifest.ValidateImage(image); err != nil {
		c.showDeploy(w, r, p, app, http.StatusUnprocessableEntity, sentence(err.Error()), image)
		return
	}
	c.audited(w, r, p, func(w http.ResponseWriter) {
		dep, err := c.deployer.DeployImage(lasting(r), app, names.Production, p.Name, image)
		if err != nil {
			c.internal(w, err)
			return
		}
		http.Redirect(w, r, "/deployments/"+dep.ID, http.StatusSeeOther)
	})
}

// deployForm guards the forms that may create an app: there may be none yet to look up.
func (c *Console) deployForm(w http.ResponseWriter, r *http.Request) (auth.Principal, string, bool) {
	if !auth.SameOrigin(r) {
		c.refused(w)
		return auth.Principal{}, "", false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		c.message(w, http.StatusBadRequest, data{Title: "Refused", Heading: "That request was refused", Message: "The form could not be read."})
		return auth.Principal{}, "", false
	}
	return c.deployGuard(w, r, r.PostFormValue("csrf"))
}

func (c *Console) deployGuard(w http.ResponseWriter, r *http.Request, token string) (auth.Principal, string, bool) {
	p, ok := c.formUser(w, r)
	if !ok {
		return p, "", false
	}
	if !auth.CheckCSRF(p.CookieValue, token) {
		c.refused(w)
		return p, "", false
	}
	app := r.PathValue("app")
	if names.ValidateApp(app) != nil {
		c.noSuchApp(w, app)
		return p, "", false
	}
	access, err := c.auth.AppAccess(r.Context(), p, app)
	if err != nil {
		c.internal(w, err)
		return p, "", false
	}
	if !access.See {
		c.noSuchApp(w, app)
		return p, "", false
	}
	if !access.Deploy {
		c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "You may not do that", Message: "Your role in the project " + access.Project.Name + " does not allow deploying.", Link: "/", LinkText: "Back to the start"})
		return p, "", false
	}
	return p, app, true
}

// deployArchive deploys an uploaded archive of source. The form's token comes first,
// so that nothing of the upload is read for a request that will be refused.
func (c *Console) deployArchive(w http.ResponseWriter, r *http.Request) {
	if !auth.SameOrigin(r) {
		c.refused(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, deploy.MaxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		c.message(w, http.StatusBadRequest, data{Title: "Refused", Heading: "That request was refused", Message: "The form could not be read."})
		return
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "csrf" {
		c.refused(w)
		return
	}
	token, err := io.ReadAll(io.LimitReader(part, 256))
	if err != nil {
		c.refused(w)
		return
	}
	p, app, ok := c.deployGuard(w, r, string(token))
	if !ok {
		return
	}
	part, err = mr.NextPart()
	if err != nil || part.FormName() != "archive" || part.FileName() == "" {
		c.showDeploy(w, r, p, app, http.StatusUnprocessableEntity, "Choose an archive to upload.", "")
		return
	}
	c.audited(w, r, p, func(w http.ResponseWriter) {
		dep, _, err := c.deployer.Submit(lasting(r), app, names.Production, p.Name, part)
		var invalid *deploy.InvalidUploadError
		switch {
		case errors.As(err, &invalid):
			c.showDeploy(w, r, p, app, http.StatusUnprocessableEntity, sentence(err.Error()), "")
			return
		case err != nil:
			c.internal(w, err)
			return
		}
		http.Redirect(w, r, "/deployments/"+dep.ID, http.StatusSeeOther)
	})
}
