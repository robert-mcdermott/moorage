package console

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// deploymentsPage is how many deployments a page of the history shows.
const deploymentsPage = 25

// maxLogLines is how many lines of a deployment's log a page holds.
const maxLogLines = 2000

// deploymentRow is a line of the deployments history.
type deploymentRow struct {
	ID      string
	Status  status
	What    string
	By      string
	At      time.Time
	Took    string
	Version int
	// Current says that its release is the one being served.
	Current bool
	// CanRollBack says that its release can be served again.
	CanRollBack bool
}

// deploymentView is the page of a deployment.
type deploymentView struct {
	deploymentRow
	App, URL, Env string
	Running       bool
	Error         string
	Output        []string
	Stages        []stageRow
	Log           []logRow
	// LogCount is how many lines the log has so far, and Omitted how many of the
	// oldest this page leaves out.
	LogCount, Omitted int
	Manifest          string
	Diff              *apitypes.ReleaseDiff
	DiffChanged       bool
	// Redeployable says that this deployment made what is being served.
	Redeployable bool
	// OfferLock says that this rollback is being served, and the next push would replace it.
	OfferLock bool
}

type stageRow struct {
	Name, Status, Took string
}

type logRow struct {
	Stream, Text string
	At           time.Time
}

// deploymentStatus is how a deployment's status looks.
func deploymentStatus(s string) status {
	switch s {
	case store.Queued:
		return status{"Queued", "info pulse"}
	case store.Building:
		return status{"Building", "info pulse"}
	case store.Releasing:
		return status{"Releasing", "info pulse"}
	case store.RollingOut:
		return status{"Rolling out", "info pulse"}
	case store.Live:
		return status{"Live", "ok"}
	case store.Failed:
		return status{"Failed", "fail"}
	case store.Superseded:
		return status{"Superseded", ""}
	case store.Canceled:
		return status{"Canceled", ""}
	}
	return status{s, ""}
}

// sourceText says what a deployment deployed.
func sourceText(d store.Deployment) string {
	switch d.SourceType {
	case deploy.SourcePush:
		commit := d.SourceCommit
		if len(commit) > 7 {
			commit = commit[:7]
		}
		text := strings.TrimPrefix(d.SourceRef, "refs/heads/") + "@" + commit
		if d.SourceMessage != "" {
			text += ": " + d.SourceMessage
		}
		return text
	case deploy.SourceUpload:
		return "Uploaded source"
	case deploy.SourceImage:
		return "Image " + d.SourceRef
	case "rollback":
		return "Roll back"
	case "promote":
		return "Promoted from another environment"
	case "config":
		return "A change of variables"
	case "static-upload":
		return "Uploaded site"
	case "process-upload":
		return "Uploaded build"
	}
	return d.SourceType
}

func tookText(d time.Duration) string {
	switch {
	case d < 0:
		return ""
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + " ms"
	case d < time.Minute:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + " s"
	}
	d = d.Round(time.Second)
	return fmt.Sprintf("%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
}

func deploymentRowOf(d store.Deployment, current int) deploymentRow {
	row := deploymentRow{
		ID: d.ID, Status: deploymentStatus(d.Status), What: sourceText(d), By: d.CreatedBy, At: d.CreatedAt, Version: d.Version,
	}
	if d.Status == store.Live || d.Status == store.Failed {
		if !d.FinishedAt.IsZero() {
			row.Took = tookText(d.FinishedAt.Sub(d.CreatedAt))
		}
	}
	row.Current = d.Version > 0 && d.Version == current
	row.CanRollBack = d.Status == store.Live && d.Version > 0 && !row.Current && !d.BuildCollected
	return row
}

func (c *Console) deploymentsList(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	d, err := c.appData(r, p, app, "apps", "Deployments of "+app)
	if err != nil {
		c.internal(w, err)
		return
	}
	d.Crumbs = []crumb{{"Apps", "/"}, {app, "/apps/" + url.PathEscape(app)}}
	d.Tab = "deployments"
	env := envOf(r)
	d.App = appView{Name: app, URL: c.cfg.EnvURL(app, env)}
	current := 0
	if rel, _, err := c.db.CurrentRelease(r.Context(), app, env); err == nil {
		current = rel.Version
	} else if !errors.Is(err, store.ErrNotFound) {
		c.internal(w, err)
		return
	}
	var created time.Time
	before := r.URL.Query().Get("before")
	var beforeID string
	if at, id, ok := strings.Cut(before, "/"); ok {
		ms, err := strconv.ParseInt(at, 10, 64)
		if err != nil {
			c.message(w, http.StatusBadRequest, data{Title: "Not found", Heading: "Not found", Message: "There is nothing at this address."})
			return
		}
		created, beforeID = time.UnixMilli(ms), id
	}
	deps, err := c.db.DeploymentsBefore(r.Context(), app, env, created, beforeID, deploymentsPage+1)
	if err != nil {
		c.internal(w, err)
		return
	}
	if len(deps) > deploymentsPage {
		deps = deps[:deploymentsPage]
		last := deps[deploymentsPage-1]
		d.Older = appAddress(app, env, "/deployments", url.Values{"before": {strconv.FormatInt(last.CreatedAt.UnixMilli(), 10) + "/" + last.ID}})
	}
	for _, dep := range deps {
		d.Deployments = append(d.Deployments, deploymentRowOf(dep, current))
		d.Poll = d.Poll || inProgress(dep)
	}
	d.RefreshOn = app
	c.render(w, http.StatusOK, "deployments", d)
}

// ansi matches the terminal's colour codes that build tools write.
var ansi = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func (c *Console) deploymentPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	notFound := func() {
		c.message(w, http.StatusNotFound, data{Title: "Not found", Heading: "There is no such deployment", Link: "/", LinkText: "Back to the apps"})
	}
	if prefix, _, err := ids.Parse(id); err != nil || prefix != ids.Deployment {
		notFound()
		return
	}
	dep, err := c.db.Deployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		notFound()
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	d, err := c.appData(r, p, dep.App, "apps", "Deployment of "+dep.App)
	if err != nil {
		c.internal(w, err)
		return
	}
	if !d.Access.See {
		notFound()
		return
	}
	app := dep.App
	d.Crumbs = []crumb{{"Apps", "/"}, {app, "/apps/" + url.PathEscape(app)}, {"Deployments", "/apps/" + url.PathEscape(app) + "/deployments"}}
	d.App = appView{Name: app, URL: c.cfg.EnvURL(app, dep.Env)}
	// It is of one environment, whatever the address says, and has no others to go to.
	d.Env, d.EnvNames = envValue(dep.Env), nil
	view, err := c.deploymentView(r, dep)
	if err != nil {
		c.internal(w, err)
		return
	}
	if !d.Access.Logs {
		view.Log, view.LogCount, view.Omitted = nil, 0, 0
	}
	d.Deployment = view
	d.Poll = view.Running
	d.RefreshOn = app
	d.Noscript = view.Running // what a page without scripts falls back on
	c.render(w, http.StatusOK, "deployment", d)
}

func (c *Console) deploymentView(r *http.Request, dep store.Deployment) (deploymentView, error) {
	ctx := r.Context()
	current := 0
	if rel, _, err := c.db.CurrentRelease(ctx, dep.App, dep.Env); err == nil {
		current = rel.Version
	} else if !errors.Is(err, store.ErrNotFound) {
		return deploymentView{}, err
	}
	lines, err := logs.FollowFromStart(deploy.LogPath(c.cfg.DataDir, dep.App, dep.ID)).Read()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return deploymentView{}, err
	}
	view := deploymentView{
		deploymentRow: deploymentRowOf(dep, current), App: dep.App, Env: dep.Env, URL: c.cfg.EnvURL(dep.App, dep.Env),
		Running: inProgress(dep), Error: sentence(dep.Error), Output: dep.Output, LogCount: len(lines),
	}
	for _, s := range deploy.Stages(dep, lines, time.Now()) {
		row := stageRow{Name: s.Name, Status: s.Status}
		if s.Status == deploy.StageDone || s.Status == deploy.StageFailed || s.Status == deploy.StageRunning {
			row.Took = tookText(s.Took)
		}
		view.Stages = append(view.Stages, row)
	}
	if len(lines) > maxLogLines {
		view.Omitted = len(lines) - maxLogLines
		lines = lines[view.Omitted:]
	}
	for _, l := range lines {
		view.Log = append(view.Log, logRow{Stream: l.Stream, Text: ansi.ReplaceAllString(l.Text, ""), At: l.Time})
	}
	// What it built, once there is a build: a deployment that failed in its build
	// has none, and one superseded in the queue never will.
	if dep.BuildID != "" {
		b, err := c.db.Build(ctx, dep.BuildID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return view, err
		default:
			if view.Manifest, err = deploy.ResolvedManifest(b); err != nil {
				return view, err
			}
		}
	}
	if dep.Version > 1 {
		diff, err := deploy.DiffReleases(ctx, c.db, dep.App, dep.Env, dep.Version-1, dep.Version)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// The release it would compare with is gone.
		case err != nil:
			return view, err
		default:
			view.Diff = &diff
			for _, l := range diff.Manifest {
				view.DiffChanged = view.DiffChanged || l.Op != apitypes.DiffSame
			}
		}
	}
	view.Redeployable = view.deploymentRow.Current
	if dep.SourceType == "rollback" && view.deploymentRow.Current {
		if e, err := c.db.Environment(r.Context(), dep.App, dep.Env); err == nil && e.LockedBy == "" {
			view.OfferLock = true
		}
	}
	return view, nil
}
