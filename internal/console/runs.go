package console

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// How much of a run a page shows, and how many runs the list has.
const (
	runLogLines = 2000
	runsShown   = 30
)

type cronView struct {
	Jobs []cronRow
	Runs []runRow
}

type cronRow struct {
	Name, Schedule, Command, Timezone string
	Next                              time.Time
	Last                              *runRow
}

type runRow struct {
	ID, Job, Command, By, Took string
	Status                     status
	At                         time.Time
}

// runView is a run's page.
type runView struct {
	runRow
	Version int
	Exit    string
	Error   string
	Running bool
	Log     []logRow
}

func runStatus(s string) status {
	switch s {
	case store.RunRunning:
		return status{"Running", "info pulse"}
	case store.RunSucceeded:
		return status{"Succeeded", "ok"}
	case store.RunFailed:
		return status{"Failed", "fail"}
	case store.RunCanceled:
		return status{"Stopped", "warn"}
	case store.RunSkipped:
		return status{"Skipped", ""}
	}
	return status{sentence(s), ""}
}

func runRowOf(r store.Run) runRow {
	row := runRow{ID: r.ID, Job: r.Job, Command: r.Command, By: r.CreatedBy, Status: runStatus(r.Status), At: r.StartedAt}
	if !r.FinishedAt.IsZero() && r.Status != store.RunSkipped {
		row.Took = r.FinishedAt.Sub(r.StartedAt).Round(100 * time.Millisecond).String()
	}
	return row
}

func (c *Console) cronPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	c.showCron(w, r, p, app, http.StatusOK, nil)
}

func (c *Console) showCron(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, code int, failed *failure) {
	d, err := c.appData(r, p, app, "apps", "Cron of "+app)
	if err != nil {
		c.internal(w, err)
		return
	}
	env := envOf(r)
	d.Crumbs = []crumb{{"Apps", "/"}, {app, appAddress(app, env, "", nil)}}
	d.Tab = "cron"
	d.App = appView{Name: app, URL: c.cfg.EnvURL(app, env)}
	view := &cronView{}
	if c.deployer != nil {
		jobs, err := c.deployer.CronJobs(r.Context(), app, env)
		if err != nil {
			c.internal(w, err)
			return
		}
		for _, j := range jobs {
			row := cronRow{Name: j.Name, Schedule: j.Schedule, Command: j.Command, Timezone: j.Timezone, Next: j.Next}
			if j.Last != nil {
				last := runRowOf(*j.Last)
				row.Last = &last
			}
			view.Jobs = append(view.Jobs, row)
		}
	}
	runs, err := c.db.Runs(r.Context(), app, env, "", runsShown)
	if err != nil {
		c.internal(w, err)
		return
	}
	for _, run := range runs {
		view.Runs = append(view.Runs, runRowOf(run))
		d.Poll = d.Poll || !run.Finished()
	}
	d.Cron = view
	d.RefreshOn = app
	d.Failure = failed
	c.render(w, code, "app-cron", d)
}

func (c *Console) runPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	run, err := c.db.Run(r.Context(), app, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		c.message(w, http.StatusNotFound, data{Title: "Not found", Heading: "There is no such run", Message: "Only the newest of a job's runs are kept.", Link: "/apps/" + url.PathEscape(app) + "/cron", LinkText: "The app's runs"})
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	d, err := c.appData(r, p, app, "apps", "A run of "+app)
	if err != nil {
		c.internal(w, err)
		return
	}
	d.Env, d.EnvNames = envValue(run.Env), nil
	d.Crumbs = []crumb{{"Apps", "/"}, {app, appAddress(app, run.Env, "", nil)}, {"Cron", appAddress(app, run.Env, "/cron", nil)}}
	d.Tab = "cron"
	d.App = appView{Name: app, URL: c.cfg.EnvURL(app, run.Env)}
	view := &runView{runRow: runRowOf(run), Version: run.Version, Error: run.Error, Running: !run.Finished()}
	if run.HasExit {
		view.Exit = fmt.Sprintf("exit status %d", run.ExitCode)
	}
	if d.Access.Logs {
		lines, err := logs.Tail(deploy.LogPath(c.cfg.DataDir, app, run.ID), runLogLines)
		if err != nil {
			c.log.Warn("read a run's output", "run", run.ID, "err", err)
		}
		for _, l := range lines {
			view.Log = append(view.Log, logRow{Stream: l.Stream, Text: l.Text, At: l.Time})
		}
	}
	d.Run = view
	d.Poll = view.Running
	c.render(w, http.StatusOK, "app-run", d)
}

func (c *Console) runFailed(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, err error) {
	var refused *deploy.NotRunnableError
	switch {
	case errors.As(err, &refused):
		c.showCron(w, r, p, app, http.StatusConflict, &failure{Message: sentence(refused.Error())})
	case errors.Is(err, store.ErrNotFound):
		c.showCron(w, r, p, app, http.StatusConflict, &failure{Message: "That is no longer there."})
	default:
		c.internal(w, err)
	}
}

func (c *Console) startRun(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	run, err := c.deployer.StartRun(lasting(r), deploy.RunRequest{App: app, Env: envOf(r), By: p.Name, Command: strings.TrimSpace(r.PostFormValue("command"))})
	if err != nil {
		c.runFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, "/apps/"+url.PathEscape(app)+"/runs/"+run.ID, http.StatusSeeOther)
}

func (c *Console) runCronJob(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	run, err := c.deployer.RunCronJob(lasting(r), app, envOf(r), r.PathValue("job"), p.Name)
	if err != nil {
		c.runFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, "/apps/"+url.PathEscape(app)+"/runs/"+run.ID, http.StatusSeeOther)
}

func (c *Console) stopRun(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	if err := c.deployer.StopRun(lasting(r), app, r.PathValue("id")); err != nil {
		c.runFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, "/apps/"+url.PathEscape(app)+"/runs/"+url.PathEscape(r.PathValue("id")), http.StatusSeeOther)
}
