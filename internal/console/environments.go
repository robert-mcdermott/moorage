package console

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// envOf says which environment of an app a request is about: production, unless its
// address says otherwise.
func envOf(r *http.Request) string {
	if env := r.URL.Query().Get("env"); env != "" {
		return env
	}
	return names.Production
}

// envQuery is what an address adds to name an environment: nothing for production.
func envQuery(env string) template.URL {
	if env == "" || env == names.Production {
		return ""
	}
	return template.URL("?env=" + url.QueryEscape(env))
}

// appAddress is the address of a page of an app, in an environment, with more to say.
func appAddress(app, env, page string, more url.Values) string {
	address := "/apps/" + url.PathEscape(app) + page
	values := url.Values{}
	for name, list := range more {
		values[name] = list
	}
	if env != "" && env != names.Production {
		values.Set("env", env)
	}
	if len(values) > 0 {
		address += "?" + values.Encode()
	}
	return address
}

// untilText says how long until a time.
func untilText(t time.Time) string {
	d := time.Until(t)
	count := func(n int, unit string) string {
		if n == 1 {
			return "in 1 " + unit
		}
		return fmt.Sprintf("in %d %ss", n, unit)
	}
	switch {
	case d <= time.Minute:
		return "any moment"
	case d < time.Hour:
		return count(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return count(int(d.Hours()), "hour")
	}
	return count(int(d.Hours()/24+0.5), "day")
}

// bareURL is an address without its scheme, where there is little room.
func bareURL(address string) string {
	_, rest, ok := strings.Cut(address, "://")
	if !ok {
		return address
	}
	return rest
}

// environmentRow is a line of a list of environments.
type environmentRow struct {
	App, Name, Kind, Branch, URL string
	Version                      int
	ReleasedAt                   time.Time
	Pinned                       bool
	LockedBy                     string
	Active, Expires              time.Time
	Status                       status
	// Asleep says that its instances are stopped until a request wakes it.
	Asleep bool
	// CanAct and CanRemove say what the viewer may do with it.
	CanAct, CanRemove bool
}

func (c *Console) environmentRow(r *http.Request, e store.Environment, access auth.Access) environmentRow {
	row := environmentRow{
		App: e.App, Name: e.Name, Kind: e.Kind, Branch: e.Branch, URL: c.cfg.EnvURL(e.App, e.Name), Version: e.Version, ReleasedAt: e.ReleasedAt,
		Pinned: e.Pinned, LockedBy: e.LockedBy, CanAct: access.Deploy, CanRemove: access.Manage || access.Deploy && e.Kind == store.KindPreview,
		Status: status{"Not deployed", ""},
	}
	if e.Version > 0 {
		row.Status = status{"Live", "ok"}
	}
	if c.deployer != nil && e.Kind == store.KindPreview {
		row.Active = c.deployer.EnvironmentActive(r.Context(), e)
		if !e.Pinned {
			row.Expires = row.Active.Add(c.deployer.PreviewTTL(r.Context(), e.App, e.Name))
		}
	}
	row.Asleep = !e.AsleepAt.IsZero()
	if deps, err := c.db.Deployments(r.Context(), e.App, e.Name, 1); err == nil && len(deps) > 0 {
		switch {
		case inProgress(deps[0]):
			row.Status = status{"Deploying", "info pulse"}
		case deps[0].Status == store.Failed && e.Version == 0:
			row.Status = status{"Deploy failed", "fail"}
		}
	}
	if row.Asleep && row.Status.Label == "Live" {
		row.Status = status{"Sleeping", "info"}
	}
	if e.Version > 0 && row.Status.Label == "Live" {
		if _, b, err := c.db.CurrentRelease(r.Context(), e.App, e.Name); err == nil {
			if plan, err := readPlan(b); err == nil && plan.Kind == manifest.KindProcess {
				row.Status = instancesStatus(c.runtimeInstances(e.App, e.Name))
			}
		}
	}
	return row
}

// instancesStatus says how an environment's instances stand.
func instancesStatus(instances []reconcile.InstanceInfo) status {
	healthy := 0
	for _, in := range instances {
		if in.Healthy {
			healthy++
		}
	}
	switch {
	case len(instances) == 0:
		return status{"Stopped", ""}
	case healthy == len(instances):
		return status{"Healthy", "ok"}
	case healthy == 0:
		return status{"Down", "fail"}
	}
	return status{fmt.Sprintf("%d of %d healthy", healthy, len(instances)), "warn"}
}

func (c *Console) runtimeInstances(app, env string) []reconcile.InstanceInfo {
	if c.runtime == nil {
		return nil
	}
	return c.runtime.Instances(app, env)
}

func (c *Console) environmentsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	c.showEnvironments(w, r, p, app, http.StatusOK, nil)
}

func (c *Console) showEnvironments(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, code int, failed *failure) {
	d, err := c.appData(r, p, app, "apps", "Environments of "+app)
	if err != nil {
		c.internal(w, err)
		return
	}
	d.Crumbs = []crumb{{"Apps", "/"}, {app, "/apps/" + url.PathEscape(app)}}
	d.Tab = "environments"
	d.App = appView{Name: app, URL: c.cfg.EnvURL(app, names.Production)}
	envs, err := c.db.Environments(r.Context(), app)
	if err != nil {
		c.internal(w, err)
		return
	}
	for _, e := range envs {
		d.EnvRows = append(d.EnvRows, c.environmentRow(r, e, d.Access))
	}
	d.Notice = environmentNotices[r.URL.Query().Get("done")]
	d.Failure = failed
	c.render(w, code, "app-environments", d)
}

var environmentNotices = map[string]string{
	"created":  "The environment is made. A deployment to it will give it an address that answers.",
	"deleted":  "The environment is removed, with everything of it.",
	"locked":   "It takes no deployments now, until it is unlocked. Rollbacks and variables go on working.",
	"unlocked": "It takes deployments again.",
	"pinned":   "It will not expire now.",
	"unpinned": "It will expire if it stays unused.",
}

// environmentAction guards a form about one environment of an app: the {env} of its path.
func (c *Console) environmentAction(next func(http.ResponseWriter, *http.Request, auth.Principal, string, string)) http.HandlerFunc {
	return c.action(func(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
		next(w, r, p, app, r.PathValue("env"))
	})
}

func (c *Console) environmentFailed(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, err error) {
	var (
		production *deploy.CannotDeleteProductionError
		busy       *deploy.AppBusyError
		locked     *deploy.LockedError
		preview    *deploy.NotPreviewableError
		invalid    *store.InvalidNameError
		rollout    *reconcile.RolloutError
	)
	switch {
	case errors.As(err, &production), errors.As(err, &busy), errors.As(err, &locked), errors.As(err, &preview):
		c.showEnvironments(w, r, p, app, http.StatusConflict, &failure{Message: sentence(err.Error())})
	case errors.As(err, &invalid):
		c.showEnvironments(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: sentence(err.Error())})
	case errors.As(err, &rollout):
		c.showEnvironments(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: sentence(rollout.Message) + " It keeps running as it was.", Output: rollout.Output})
	case errors.Is(err, store.ErrNotFound):
		c.showEnvironments(w, r, p, app, http.StatusConflict, &failure{Message: sentence(strings.TrimSuffix(err.Error(), ": "+store.ErrNotFound.Error()))})
	default:
		c.internal(w, err)
	}
}

func (c *Console) createEnvironment(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	created, err := c.db.CreateEnvironment(r.Context(), app, name, store.KindNamed, strings.TrimSpace(r.PostFormValue("branch")))
	if err != nil {
		c.environmentFailed(w, r, p, app, err)
		return
	}
	if !created {
		c.showEnvironments(w, r, p, app, http.StatusConflict, &failure{Message: "There is already an environment of that name."})
		return
	}
	http.Redirect(w, r, appAddress(app, "", "/environments", url.Values{"done": {"created"}}), http.StatusSeeOther)
}

func (c *Console) createPreview(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	dep, err := c.deployer.PreviewBranch(lasting(r), app, strings.TrimSpace(r.PostFormValue("branch")), p.Name)
	if err != nil {
		c.environmentFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, "/deployments/"+dep.ID, http.StatusSeeOther)
}

// backTo goes back to where a form came from, among the two pages that list environments.
func backTo(w http.ResponseWriter, r *http.Request, app, done string) {
	if r.PostFormValue("from") == "previews" {
		http.Redirect(w, r, "/previews?done="+url.QueryEscape(done), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, appAddress(app, "", "/environments", url.Values{"done": {done}}), http.StatusSeeOther)
}

func (c *Console) deleteEnvironment(w http.ResponseWriter, r *http.Request, p auth.Principal, app, env string) {
	// Deleting a branch destroys its preview, so a deployer may do that here too; the
	// environments that stay are a maintainer's to remove.
	kind, err := c.db.EnvKind(r.Context(), app, env)
	if err != nil {
		c.environmentFailed(w, r, p, app, err)
		return
	}
	access, err := c.auth.AppAccess(r.Context(), p, app)
	if err != nil {
		c.internal(w, err)
		return
	}
	if kind != store.KindPreview && !access.Manage {
		c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "You may not do that", Message: "Removing an environment takes a maintainer of the project " + access.Project.Name + ".", Link: "/apps/" + url.PathEscape(app) + "/environments", LinkText: "Back to its environments"})
		return
	}
	if err := c.deployer.DeleteEnvironment(lasting(r), app, env, p.Name); err != nil {
		c.environmentFailed(w, r, p, app, err)
		return
	}
	backTo(w, r, app, "deleted")
}

func (c *Console) lockEnvironment(w http.ResponseWriter, r *http.Request, p auth.Principal, app, env string) {
	if err := c.deployer.Lock(r.Context(), app, env, p.Name); err != nil {
		c.environmentFailed(w, r, p, app, err)
		return
	}
	backTo(w, r, app, "locked")
}

func (c *Console) unlockEnvironment(w http.ResponseWriter, r *http.Request, p auth.Principal, app, env string) {
	if err := c.deployer.Unlock(r.Context(), app, env, p.Name); err != nil {
		c.environmentFailed(w, r, p, app, err)
		return
	}
	backTo(w, r, app, "unlocked")
}

func (c *Console) pinEnvironment(w http.ResponseWriter, r *http.Request, p auth.Principal, app, env string) {
	pinned := r.PostFormValue("pinned") != ""
	if err := c.db.SetPinned(r.Context(), app, env, pinned); err != nil {
		c.environmentFailed(w, r, p, app, err)
		return
	}
	if pinned {
		backTo(w, r, app, "pinned")
	} else {
		backTo(w, r, app, "unpinned")
	}
}

func (c *Console) promoteEnvironment(w http.ResponseWriter, r *http.Request, p auth.Principal, app, env string) {
	to := strings.TrimSpace(r.PostFormValue("to"))
	if to == "" {
		to = names.Production
	}
	_, dep, err := c.deployer.Promote(lasting(r), app, env, to, p.Name)
	if err != nil {
		c.environmentFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, "/deployments/"+dep.ID, http.StatusSeeOther)
}

// previewsPage is every preview the viewer can see.
func (c *Console) previewsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	d, _, err := c.shellData(r, p, "previews", "Previews")
	if err != nil {
		c.internal(w, err)
		return
	}
	previews, err := c.db.Previews(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	for _, e := range previews {
		access, err := c.auth.AppAccess(r.Context(), p, e.App)
		if err != nil {
			c.internal(w, err)
			return
		}
		if access.See {
			d.EnvRows = append(d.EnvRows, c.environmentRow(r, e, access))
		}
	}
	d.RefreshOn = "*"
	d.Notice = environmentNotices[r.URL.Query().Get("done")]
	c.render(w, http.StatusOK, "previews", d)
}
