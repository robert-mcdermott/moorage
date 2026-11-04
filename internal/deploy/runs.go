package deploy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/build"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/oneoff"
	"github.com/robert-mcdermott/moorage/internal/schedule"
	"github.com/robert-mcdermott/moorage/internal/store"
)

const (
	// KeepRuns is how many of a job's runs are kept, with their output.
	KeepRuns = 100
	// DefaultRunLimit and MaxRunLimit are how long a command someone asked for may take.
	DefaultRunLimit = time.Hour
	MaxRunLimit     = 24 * time.Hour
)

// NotRunnableError says why a command cannot run in an environment.
type NotRunnableError struct{ Msg string }

func (e *NotRunnableError) Error() string { return e.Msg }

// runBook is what is running now, and when each cron job next is.
type runBook struct {
	mu      sync.Mutex
	running map[string]*activeRun
	next    map[string]cronState
}

type activeRun struct {
	app, env, job string
	cancel        context.CancelFunc
	done          chan struct{}
}

// cronState is what the scheduler knows of a job.
type cronState struct {
	schedule, timezone string
	next               time.Time
}

func (b *runBook) add(id string, a *activeRun) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running == nil {
		b.running = map[string]*activeRun{}
	}
	b.running[id] = a
}

func (b *runBook) remove(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.running, id)
}

// ofJob returns the runs of a job going on now.
func (b *runBook) ofJob(app, env, job string) []*activeRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*activeRun
	for _, a := range b.running {
		if a.app == app && a.env == env && a.job == job {
			out = append(out, a)
		}
	}
	return out
}

// cancelRuns stops what is running in an environment, and waits for it, since what goes next
// is its files.
func (d *Deployer) cancelRuns(app, env string) {
	d.runs.mu.Lock()
	var going []*activeRun
	for _, a := range d.runs.running {
		if a.app == app && a.env == env {
			going = append(going, a)
		}
	}
	d.runs.mu.Unlock()
	for _, a := range going {
		a.cancel()
	}
	for _, a := range going {
		<-a.done
	}
}

// RunRequest asks for a command to run in an environment's current release.
type RunRequest struct {
	App, Env, By string
	// Job is the cron job it is for, if any. Trigger is schedule or manual.
	Job, Trigger string
	Command      string
	Timeout      time.Duration
}

// StartRun starts a command, as a one-off instance of an environment's current release, with its
// variables and data directory. It returns at once; FollowRun has what it says.
func (d *Deployer) StartRun(ctx context.Context, req RunRequest) (store.Run, error) {
	if strings.TrimSpace(req.Command) == "" {
		return store.Run{}, &NotRunnableError{"there is no command to run"}
	}
	if req.Timeout <= 0 {
		req.Timeout = DefaultRunLimit
	}
	if req.Timeout > MaxRunLimit {
		return store.Run{}, &NotRunnableError{"a command can take a day at the most"}
	}
	rel, b, err := d.db.CurrentRelease(ctx, req.App, req.Env)
	if errors.Is(err, store.ErrNotFound) {
		return store.Run{}, &NotRunnableError{req.Env + " of " + req.App + " has nothing deployed to run a command with"}
	}
	if err != nil {
		return store.Run{}, err
	}
	var plan manifest.Plan
	if err := json.Unmarshal(b.Plan, &plan); err != nil {
		return store.Run{}, err
	}
	if plan.Kind != manifest.KindProcess {
		return store.Run{}, &NotRunnableError{req.App + " is a static site, with nothing to run a command with"}
	}
	plan.Defaults()
	if req.Trigger == "" {
		req.Trigger = "manual"
	}
	run := store.Run{
		ID: ids.New(ids.Run), App: req.App, Env: req.Env, Job: req.Job, Command: req.Command, Trigger: req.Trigger, CreatedBy: req.By,
		Version: rel.Version, Status: store.RunRunning, StartedAt: time.Now().UTC(),
	}
	if err := d.db.CreateRun(ctx, run); err != nil {
		return run, err
	}
	runCtx, cancel := context.WithTimeout(d.ctx, req.Timeout+time.Minute)
	active := &activeRun{app: req.App, env: req.Env, job: req.Job, cancel: cancel, done: make(chan struct{})}
	d.runs.add(run.ID, active)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer cancel()
		defer close(active.done)
		defer d.runs.remove(run.ID)
		process := "run"
		if req.Job != "" {
			process = "cron-" + req.Job
		}
		logFile, err := logs.Open(LogPath(d.opts.DataDir, req.App, run.ID), 0, 0)
		if err == nil {
			defer logFile.Close()
			err = d.opts.Runtime.RunOnce(runCtx, req.App, req.Env, rel, b, plan, process, req.Command, req.Timeout, logFile)
		}
		d.finishRun(run, err, runCtx)
	}()
	return run, nil
}

// finishRun records how a run ended.
func (d *Deployer) finishRun(run store.Run, err error, runCtx context.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, failure := store.RunSucceeded, ""
	var exit *int
	var exited *oneoff.ExitError
	switch {
	case err == nil:
		zero := 0
		exit = &zero
	case runCtx.Err() != nil && errors.Is(runCtx.Err(), context.Canceled):
		status, failure = store.RunCanceled, "it was stopped"
	case errors.As(err, &exited):
		status, failure = store.RunFailed, fmt.Sprintf("it exited with status %d", exited.Code)
		exit = &exited.Code
	default:
		status, failure = store.RunFailed, err.Error()
	}
	if err := d.db.FinishRun(ctx, run.ID, status, exit, failure, time.Now().UTC()); err != nil {
		d.log.Error("record the end of a run", "run", run.ID, "err", err)
	}
	if run.Job != "" {
		if status == store.RunFailed {
			d.opts.Events.Publish(ctx, store.Event{Type: events.CronFailed, App: run.App, Env: run.Env, Data: map[string]any{"job": run.Job, "run": run.ID, "error": failure}})
		}
	}
	gone, err := d.db.PruneRuns(ctx, run.App, run.Env, run.Job, KeepRuns)
	if err != nil {
		d.log.Warn("let go of old runs", "app", run.App, "env", run.Env, "err", err)
	}
	for _, id := range gone {
		os.Remove(LogPath(d.opts.DataDir, run.App, id))
	}
}

// StopRun stops a run that is going on.
func (d *Deployer) StopRun(ctx context.Context, app, id string) error {
	if _, err := d.db.Run(ctx, app, id); err != nil {
		return err
	}
	d.runs.mu.Lock()
	active := d.runs.running[id]
	d.runs.mu.Unlock()
	if active != nil {
		active.cancel()
		<-active.done
	}
	return nil
}

// FollowRun passes a run's output to fn as it comes, until it ends or ctx does, and returns the
// run as it ended.
func (d *Deployer) FollowRun(ctx context.Context, app, id string, fn func(logs.Line) error) (store.Run, error) {
	run, err := d.db.Run(ctx, app, id)
	if err != nil {
		return run, err
	}
	f := logs.FollowFromStart(LogPath(d.opts.DataDir, app, id))
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		finished := run.Finished()
		lines, err := f.Read()
		if err != nil {
			return run, err
		}
		for _, l := range lines {
			if err := fn(l); err != nil {
				return run, err
			}
		}
		if finished {
			return run, nil
		}
		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case <-tick.C:
		}
		if run, err = d.db.Run(ctx, app, id); err != nil {
			return run, err
		}
	}
}

// CronJob is a cron job of an environment, as it stands.
type CronJob struct {
	manifest.CronPlan
	// Next is when it next runs, if it ever does.
	Next time.Time
	// Last is its newest run, if it has had one.
	Last *store.Run
}

// CronJobs lists the jobs of an environment's current release.
func (d *Deployer) CronJobs(ctx context.Context, app, env string) ([]CronJob, error) {
	e, err := d.db.Environment(ctx, app, env)
	if err != nil {
		return nil, err
	}
	_, b, err := d.db.CurrentRelease(ctx, app, env)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var plan manifest.Plan
	if err := json.Unmarshal(b.Plan, &plan); err != nil {
		return nil, err
	}
	var out []CronJob
	now := time.Now()
	for _, c := range plan.Cron {
		if !c.RunsIn(env, e.Kind) {
			continue
		}
		job := CronJob{CronPlan: c}
		d.runs.mu.Lock()
		st, known := d.runs.next[cronKey(app, env, c.Name)]
		d.runs.mu.Unlock()
		if known && st.schedule == c.Schedule && st.timezone == c.Timezone {
			job.Next = st.next
		} else if s, err := schedule.Parse(c.Schedule); err == nil {
			job.Next = s.Next(now, cronLocation(c.Timezone))
		}
		runs, err := d.db.Runs(ctx, app, env, c.Name, 1)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			job.Last = &runs[0]
		}
		out = append(out, job)
	}
	return out, nil
}

// RunCronJob runs a job now, whatever its schedule says. What is going on stays going on.
func (d *Deployer) RunCronJob(ctx context.Context, app, env, name, by string) (store.Run, error) {
	jobs, err := d.CronJobs(ctx, app, env)
	if err != nil {
		return store.Run{}, err
	}
	for _, j := range jobs {
		if j.Name == name {
			return d.StartRun(ctx, RunRequest{App: app, Env: env, By: by, Job: name, Trigger: "manual", Command: j.Command, Timeout: j.Limit()})
		}
	}
	return store.Run{}, fmt.Errorf("no cron job %q in %s of %s: %w", name, env, app, store.ErrNotFound)
}

func cronKey(app, env, job string) string { return app + "/" + env + "/" + job }

func cronLocation(zone string) *time.Location {
	if zone != "" {
		if loc, err := time.LoadLocation(zone); err == nil {
			return loc
		}
	}
	return time.Local
}

// RunCron starts cron jobs when they are due, until ctx ends. A job that was due while the
// server was down waits for its next time: what ran late might be worse than what did not run.
func (d *Deployer) RunCron(ctx context.Context, every time.Duration) {
	if err := d.db.EndInterruptedRuns(ctx, time.Now().UTC()); err != nil {
		d.log.Warn("record the runs that the server's stopping ended", "err", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		d.cronTick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *Deployer) cronTick(ctx context.Context, now time.Time) {
	routes, err := d.db.Routes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("look at the cron jobs", "err", err)
		}
		return
	}
	seen := map[string]bool{}
	for _, r := range routes {
		var plan manifest.Plan
		if json.Unmarshal(r.Build.Plan, &plan) != nil || plan.Kind != manifest.KindProcess {
			continue
		}
		for _, c := range plan.Cron {
			if !c.RunsIn(r.Env, r.Kind) {
				continue
			}
			key := cronKey(r.App, r.Env, c.Name)
			seen[key] = true
			if d.cronDue(key, c, now) {
				d.fireCron(ctx, r.App, r.Env, c, now)
			}
		}
	}
	d.runs.mu.Lock()
	defer d.runs.mu.Unlock()
	for key := range d.runs.next {
		if !seen[key] {
			delete(d.runs.next, key)
		}
	}
}

// cronDue says whether a job is due, and if so moves it on to its next time.
func (d *Deployer) cronDue(key string, c manifest.CronPlan, now time.Time) bool {
	s, err := schedule.Parse(c.Schedule)
	if err != nil {
		return false
	}
	loc := cronLocation(c.Timezone)
	d.runs.mu.Lock()
	defer d.runs.mu.Unlock()
	if d.runs.next == nil {
		d.runs.next = map[string]cronState{}
	}
	st, known := d.runs.next[key]
	if !known || st.schedule != c.Schedule || st.timezone != c.Timezone {
		d.runs.next[key] = cronState{c.Schedule, c.Timezone, s.Next(now, loc)}
		return false
	}
	if st.next.IsZero() || now.Before(st.next) {
		return false
	}
	st.next = s.Next(now, loc)
	d.runs.next[key] = st
	return true
}

// fireCron starts a job that is due, doing as it says of a run still going on before it.
func (d *Deployer) fireCron(ctx context.Context, app, env string, c manifest.CronPlan, now time.Time) {
	going := d.runs.ofJob(app, env, c.Name)
	if len(going) > 0 {
		switch c.Overlap {
		case "allow":
		case "replace":
			for _, a := range going {
				a.cancel()
			}
			for _, a := range going {
				<-a.done
			}
		default:
			skipped := store.Run{ID: ids.New(ids.Run), App: app, Env: env, Job: c.Name, Command: c.Command, Trigger: "schedule", Status: store.RunSkipped,
				Error: "the run before it was still going", StartedAt: now.UTC(), FinishedAt: now.UTC()}
			if err := d.db.CreateRun(ctx, skipped); err != nil {
				d.log.Warn("record a skipped run", "job", c.Name, "err", err)
			}
			if err := d.db.FinishRun(ctx, skipped.ID, store.RunSkipped, nil, skipped.Error, now.UTC()); err != nil {
				d.log.Warn("record a skipped run", "job", c.Name, "err", err)
			}
			return
		}
	}
	if _, err := d.StartRun(ctx, RunRequest{App: app, Env: env, Job: c.Name, Trigger: "schedule", Command: c.Command, Timeout: c.Limit()}); err != nil && ctx.Err() == nil {
		d.log.Warn("start a cron job", "app", app, "env", env, "job", c.Name, "err", err)
	}
}

// prepareVars sees to the variables an app expects, before a deployment releases it: it makes
// those it should generate, once for each environment, and fails if a required one is not there,
// so that the app does not start up only to fall over.
func (d *Deployer) prepareVars(ctx context.Context, app, env string, plan manifest.Plan, log io.Writer) error {
	if len(plan.Vars) == 0 {
		return nil
	}
	kind, err := d.db.EnvKind(ctx, app, env)
	if err != nil {
		return err
	}
	own, err := d.db.Vars(ctx, app, env)
	if err != nil {
		return err
	}
	hasOwn := map[string]bool{}
	for _, v := range own {
		hasOwn[v.Name] = true
	}
	declared := make([]string, 0, len(plan.Vars))
	for name := range plan.Vars {
		declared = append(declared, name)
	}
	sort.Strings(declared)
	var generate []VarValue
	for _, name := range declared {
		v := plan.Vars[name]
		if v.Generate == "" || hasOwn[name] {
			continue
		}
		value, err := generateValue(v.Generate)
		if err != nil {
			return err
		}
		generate = append(generate, VarValue{Name: name, Value: value, Secret: true})
		build.Marker(log, "moorage  generated %s for %s", name, env)
	}
	if len(generate) > 0 {
		if _, err := d.Configure(ctx, app, env, ConfigChange{Set: generate, By: "moorage"}); err != nil {
			return err
		}
	}
	have, err := d.db.Config(ctx, app, env)
	if err != nil {
		return err
	}
	set := map[string]store.ConfigVar{}
	for _, v := range have {
		set[v.Name] = v
	}
	inManifest := plan.ForEnv(env, kind).Env
	var missing, exposed []string
	for _, name := range declared {
		v := plan.Vars[name]
		cfg, onServer := set[name]
		_, inRepo := inManifest[name]
		switch {
		case v.Required && !onServer && !inRepo:
			missing = append(missing, name)
		case v.Secret && (onServer && !cfg.Secret || !onServer && inRepo):
			exposed = append(exposed, name)
		}
	}
	var problems []string
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("it needs %s, which %s not set: moor env set --env %s", strings.Join(missing, ", "), isOrAre(missing), env))
	}
	if len(exposed) > 0 {
		problems = append(problems, fmt.Sprintf("%s must be kept as %s: moor env set --secret", strings.Join(exposed, ", "), aSecretOrSecrets(exposed)))
	}
	if len(problems) > 0 {
		return &releaseError{strings.Join(problems, "; ")}
	}
	return nil
}

func isOrAre(list []string) string {
	if len(list) == 1 {
		return "is"
	}
	return "are"
}

func aSecretOrSecrets(list []string) string {
	if len(list) == 1 {
		return "a secret"
	}
	return "secrets"
}

// generateValue makes a value for a variable: secret or uuid.
func generateValue(kind string) (string, error) {
	raw := make([]byte, 32)
	if kind == "uuid" {
		raw = raw[:16]
	}
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	if kind == "uuid" {
		raw[6] = raw[6]&0x0f | 0x40
		raw[8] = raw[8]&0x3f | 0x80
		return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:]), nil
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
