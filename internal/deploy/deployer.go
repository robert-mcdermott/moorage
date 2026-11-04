package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/build"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/gitserver"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Runtime runs the processes of releases.
type Runtime interface {
	Rollout(ctx context.Context, app, env string, rel store.Release, build store.Build, plan manifest.Plan) error
	Retire(app, env string)
	Forget(app string)
	ForgetEnv(app, env string)
	Restart(ctx context.Context, app, env string) error
	Scale(ctx context.Context, app, env string, change map[string]int) (map[string]int, error)
	Settle(ctx context.Context, app, env string) error
	RunOnce(ctx context.Context, app, env string, rel store.Release, build store.Build, plan manifest.Plan, command string, timeout time.Duration, log io.Writer) error
}

// Keyring seals and opens the values of secret variables.
type Keyring interface {
	Seal(ctx context.Context, appID, scope, name, value string) ([]byte, error)
	Open(ctx context.Context, appID, scope, name string, sealed []byte) (string, error)
}

// Options configure a Deployer.
type Options struct {
	DataDir string
	Builder *build.Builder
	Runtime Runtime
	// Reload makes the router pick up a new release.
	Reload func(context.Context) error
	// Secrets seals and opens secret variables.
	Secrets Keyring
	// Events hears of deployments and releases.
	Events *events.Bus
	// Repos holds what was pushed, to build it again.
	Repos *gitserver.Repos
	// ForgetApp has the router let go of an app's files, which are about to move or go.
	ForgetApp func(app string)
	// Config is the server's configuration: its hostnames.
	Config config.Config
	// Resolver looks up what a domain points to, unless the system's is to.
	Resolver DomainResolver
	// Busy says whether an environment has requests going on, such as a stream.
	Busy func(app, env string) bool
	// LastActive says when an environment last had a request, since the server started.
	LastActive func(app, env string) time.Time
	// URL returns an environment's address, for the log.
	URL func(app, env string) string
	// Builds bounds how many builds run at once (default: half the CPUs).
	Builds int
	// GCMinAge protects new builds from garbage collection (default: a
	// minute), so that one being published is never collected.
	GCMinAge time.Duration
	// FailedBuildAge is how long the directory of a failed build is kept
	// for inspection (default: a day).
	FailedBuildAge time.Duration
}

// Deployer runs deployments: it publishes uploaded builds at once, and
// builds and publishes source in the background, one deployment per
// environment at a time.
type Deployer struct {
	db   *store.DB
	opts Options
	log  *slog.Logger

	ctx    context.Context // ends when the server shuts down
	cancel context.CancelFunc
	wg     sync.WaitGroup
	slots  chan struct{} // build slots

	mu     sync.Mutex
	queues map[string]*queue      // by app/env
	locks  map[string]*sync.Mutex // the release phase of an environment

	started  time.Time
	asleep   sync.Map // of app/env, as long as a request should wait for it
	wakingMu sync.Mutex
	waking   map[string]chan struct{} // by app/env: closes when it has woken
}

type queue struct {
	running bool
	next    *store.Deployment
}

// New returns a deployer.
func New(db *store.DB, opts Options, log *slog.Logger) *Deployer {
	if opts.Builds <= 0 {
		opts.Builds = max(1, runtime.NumCPU()/2)
	}
	if opts.GCMinAge == 0 {
		opts.GCMinAge = time.Minute
	}
	if opts.FailedBuildAge == 0 {
		opts.FailedBuildAge = 24 * time.Hour
	}
	if opts.URL == nil {
		opts.URL = func(app, env string) string { return "" }
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Deployer{
		db: db, opts: opts, log: log, ctx: ctx, cancel: cancel,
		slots:   make(chan struct{}, opts.Builds),
		queues:  map[string]*queue{},
		locks:   map[string]*sync.Mutex{},
		started: time.Now(),
		waking:  map[string]chan struct{}{},
	}
	if err := d.loadAsleep(ctx); err != nil {
		log.Error("cannot read which environments are asleep", "err", err)
	}
	return d
}

// Shutdown stops the running deployments (they are recorded as failed)
// and waits for them to finish recording.
func (d *Deployer) Shutdown() {
	d.cancel()
	d.wg.Wait()
}

// LogPath is a deployment's log file.
func LogPath(dataDir, app, id string) string {
	return filepath.Join(dataDir, "logs", "deployments", app, id+".log")
}

func (d *Deployer) envLock(app, env string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := app + "/" + env
	l := d.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		d.locks[key] = l
	}
	return l
}

// Submit stores uploaded source (a gzip-compressed tar stream) as a new
// build directory of app and queues its deployment. The app is created if
// it does not exist, once the upload is known to be good. A newer
// submission supersedes one still queued for the same environment.
func (d *Deployer) Submit(ctx context.Context, app, env, by string, upload io.Reader) (store.Deployment, bool, error) {
	return d.SubmitSource(ctx, app, env, by, Source{Type: SourceUpload, Fill: fillKeeping(upload)})
}

// SubmitImage queues the deployment of an image. upload holds no more than the
// app's manifest, if it has one.
func (d *Deployer) SubmitImage(ctx context.Context, app, env, by, image string, upload io.Reader) (store.Deployment, bool, error) {
	return d.SubmitSource(ctx, app, env, by, Source{Type: SourceImage, Ref: image, Fill: fillKeeping(upload)})
}

// Source is a deployment's source code. Fill writes it into the build
// directory and returns how many files it wrote.
type Source struct {
	// Type is "source-upload", "git-push" or "image".
	Type string
	// Ref, Commit and Message describe a pushed commit; for an image, Ref is
	// its name.
	Ref, Commit, Message string
	Fill                 func(dir string) (files int, err error)
	// EnvKind and Branch say what kind of environment to make if it does not exist yet,
	// and which branch deploys to it: a named one, unless said otherwise.
	EnvKind, Branch string
}

// The types of source that a deployment builds. SourceImage is a deployment of an
// image: its source holds no more than the app's manifest, if that.
const (
	SourceUpload = "source-upload"
	SourcePush   = "git-push"
	SourceImage  = "image"
)

// SubmitSource is Submit for any source.
func (d *Deployer) SubmitSource(ctx context.Context, app, env, by string, src Source) (store.Deployment, bool, error) {
	if err := names.ValidateApp(app); err != nil {
		return store.Deployment{}, false, err
	}
	if err := d.checkLock(ctx, app, env); err != nil {
		return store.Deployment{}, false, err
	}
	buildID := ids.New(ids.Build)
	dir := filepath.Join(d.opts.DataDir, "builds", app, buildID)
	if err := os.MkdirAll(appDir(dir), 0o755); err != nil {
		return store.Deployment{}, false, err
	}
	files, err := src.Fill(appDir(dir))
	if err == nil && files == 0 && src.Type != SourceImage {
		err = errors.New("the source contains no files")
	}
	if err != nil {
		os.RemoveAll(dir)
		return store.Deployment{}, false, &InvalidUploadError{err}
	}
	_, created, err := d.db.EnsureApp(ctx, app)
	if err != nil {
		os.RemoveAll(dir)
		return store.Deployment{}, false, err
	}
	if err := d.ensureEnv(ctx, app, env, src.EnvKind, src.Branch); err != nil {
		os.RemoveAll(dir)
		return store.Deployment{}, false, err
	}
	dep := store.Deployment{
		ID: ids.New(ids.Deployment), App: app, Env: env, SourceType: src.Type,
		SourceRef: src.Ref, SourceCommit: src.Commit, SourceMessage: src.Message, Status: store.Queued,
		BuildID: buildID, CreatedBy: by, CreatedAt: time.Now().UTC(),
	}
	if err := d.db.CreateDeployment(ctx, dep); err != nil {
		os.RemoveAll(dir)
		return store.Deployment{}, false, err
	}
	d.emit(ctx, events.DeploymentQueued, dep, nil)
	d.enqueue(dep)
	return dep, created, nil
}

// ensureEnv makes an environment that a deployment names, if it does not exist yet.
func (d *Deployer) ensureEnv(ctx context.Context, app, env, kind, branch string) error {
	if env == names.Production {
		return nil
	}
	if kind == "" {
		kind = store.KindNamed
	}
	created, err := d.db.CreateEnvironment(ctx, app, env, kind, branch)
	if err != nil {
		return err
	}
	if created {
		d.opts.Events.Publish(ctx, store.Event{Type: events.EnvironmentCreated, App: app, Env: env, Data: map[string]any{"kind": kind, "branch": branch}})
	}
	return nil
}

// Follow passes a deployment's log lines to fn as they are written, until
// the deployment ends or ctx is done, and returns the deployment as it
// ended.
func (d *Deployer) Follow(ctx context.Context, id string, fn func(logs.Line) error) (store.Deployment, error) {
	dep, err := d.db.Deployment(ctx, id)
	if err != nil {
		return dep, err
	}
	f := logs.FollowFromStart(LogPath(d.opts.DataDir, dep.App, dep.ID))
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		// The status is read before the log: a deployment writes its last
		// line before it records its end.
		finished := store.Final(dep.Status)
		lines, err := f.Read()
		if err != nil {
			return dep, err
		}
		for _, l := range lines {
			if err := fn(l); err != nil {
				return dep, err
			}
		}
		if finished {
			return dep, nil
		}
		select {
		case <-ctx.Done():
			return dep, ctx.Err()
		case <-tick.C:
		}
		if dep, err = d.db.Deployment(ctx, id); err != nil {
			return dep, err
		}
	}
}

func (d *Deployer) enqueue(dep store.Deployment) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := dep.App + "/" + dep.Env
	q := d.queues[key]
	if q == nil {
		q = &queue{}
		d.queues[key] = q
	}
	if !q.running {
		q.running = true
		d.start(dep)
		return
	}
	if q.next != nil {
		d.supersede(*q.next, dep.ID)
	}
	q.next = &dep
}

// start runs a deployment in the background; called with d.mu held.
func (d *Deployer) start(dep store.Deployment) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.run(dep)
		d.mu.Lock()
		defer d.mu.Unlock()
		q := d.queues[dep.App+"/"+dep.Env]
		if q.next != nil && d.ctx.Err() == nil {
			next := *q.next
			q.next = nil
			d.start(next)
			return
		}
		if q.next != nil {
			d.finish(*q.next, store.Canceled, "the server stopped before the deployment started", nil)
			q.next = nil
		}
		q.running = false
	}()
}

func (d *Deployer) supersede(dep store.Deployment, by string) {
	d.finish(dep, store.Superseded, "superseded by "+by, nil)
	os.RemoveAll(filepath.Join(d.opts.DataDir, "builds", dep.App, dep.BuildID))
}

func (d *Deployer) finish(dep store.Deployment, status, msg string, output []string) {
	if err := d.db.FinishDeployment(context.Background(), dep.ID, status, msg, output); err != nil {
		d.log.Error("record the end of a deployment", "deployment", dep.ID, "err", err)
	}
	switch status {
	case store.Failed:
		d.emit(context.Background(), events.DeploymentFailed, dep, map[string]any{"error": msg})
	case store.Superseded:
		d.emit(context.Background(), events.DeploymentSuperseded, dep, nil)
	case store.Canceled:
		d.emit(context.Background(), events.DeploymentCanceled, dep, nil)
	}
}

// emit says what happened to a deployment.
func (d *Deployer) emit(ctx context.Context, kind string, dep store.Deployment, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["deployment"] = dep.ID
	if s := describeSource(dep); s != "" {
		data["source"] = s
	}
	d.opts.Events.Publish(ctx, store.Event{Type: kind, App: dep.App, Env: dep.Env, Actor: dep.CreatedBy, Data: data})
}

// describeSource says where a deployment came from, in a few words.
func describeSource(dep store.Deployment) string {
	switch {
	case dep.SourceType == SourceImage:
		return "image " + dep.SourceRef
	case dep.SourceCommit != "":
		commit := dep.SourceCommit
		if len(commit) > 7 {
			commit = commit[:7]
		}
		return "push " + strings.TrimPrefix(dep.SourceRef, "refs/heads/") + "@" + commit
	}
	return ""
}

// run builds and publishes a queued deployment.
func (d *Deployer) run(dep store.Deployment) {
	ctx := d.ctx
	logFile, err := logs.Open(LogPath(d.opts.DataDir, dep.App, dep.ID), 0, 0)
	if err != nil {
		d.finish(dep, store.Failed, "cannot write the deployment log: "+err.Error(), nil)
		return
	}
	defer logFile.Close()
	fail := func(msg string, output []string) {
		build.Marker(logFile, "✘ failed     %s", msg)
		if output == nil {
			output = lastOutput(logFile.Path, 20)
		}
		logFile.Close()
		d.finish(dep, store.Failed, msg, output)
		d.log.Warn("deployment failed", "app", dep.App, "env", dep.Env, "deployment", dep.ID, "err", msg)
	}
	build.Marker(logFile, "moorage  %s · %s · %s", dep.App, dep.Env, dep.ID)
	if dep.SourceCommit != "" {
		build.Marker(logFile, "▸ source     %s @ %.7s  %s", strings.TrimPrefix(dep.SourceRef, "refs/heads/"), dep.SourceCommit, dep.SourceMessage)
	}

	select {
	case d.slots <- struct{}{}:
	case <-ctx.Done():
		fail("interrupted: the server stopped", nil)
		return
	}
	d.db.SetDeploymentStatus(ctx, dep.ID, store.Building)
	d.emit(ctx, events.DeploymentStarted, dep, nil)
	start := time.Now()
	dir := filepath.Join(d.opts.DataDir, "builds", dep.App, dep.BuildID)
	vars, redact, err := d.buildVars(ctx, dep.App, dep.Env)
	if err != nil {
		<-d.slots
		fail(err.Error(), nil)
		return
	}
	job := build.Job{App: dep.App, Env: dep.Env, BuildID: dep.BuildID, Dir: dir, Log: logFile, Vars: vars, Redact: redact}
	if dep.SourceType == SourceImage {
		job.Image = dep.SourceRef
	}
	res, err := d.opts.Builder.Build(ctx, job)
	<-d.slots
	os.RemoveAll(filepath.Join(d.opts.DataDir, "tmp", "build-"+dep.BuildID))
	if err != nil {
		var be *build.Error
		switch {
		case ctx.Err() != nil:
			fail("interrupted: the server stopped", nil)
		case errors.As(err, &be):
			fail(be.Msg, nil)
		default:
			fail("the build failed: "+err.Error(), nil)
		}
		return
	}
	took := time.Since(start).Round(100 * time.Millisecond)
	kind, ref := "dir", filepath.ToSlash(filepath.Join("builds", dep.App, dep.BuildID))
	if res.Image != "" {
		kind, ref = "image", res.Image
		build.Marker(logFile, "▸ image      %s, %s in %s", res.Image, humanBytes(res.Size), took)
	} else {
		build.Marker(logFile, "▸ built      %d files, %s in %s", res.Files, humanBytes(res.Size), took)
	}

	planJSON, err := json.Marshal(res.Plan)
	if err != nil {
		fail(err.Error(), nil)
		return
	}
	appID, _, err := d.db.EnsureApp(ctx, dep.App)
	if err != nil {
		fail(err.Error(), nil)
		return
	}
	b := store.Build{
		ID: dep.BuildID, AppID: appID, SourceType: dep.SourceType, ArtifactKind: kind,
		ArtifactRef: ref, Plan: planJSON, Files: res.Files, SizeBytes: res.Size, CreatedAt: time.Now(),
	}
	if err := d.db.CreateBuild(ctx, b); err != nil {
		fail(err.Error(), nil)
		return
	}
	if res.Image != "" {
		removeTree(appDir(dir)) // the image holds what the source was for
	}
	rel, err := d.publish(ctx, dep, b, res.Plan, "deploy", nil, logFile)
	if err != nil {
		var rollout *reconcile.RolloutError
		switch {
		case errors.As(err, &rollout):
			fail(rollout.Message+"; the previous release keeps serving", rollout.Output)
		case ctx.Err() != nil:
			fail("interrupted: the server stopped", nil)
		default:
			fail(err.Error(), nil)
		}
		return
	}
	build.Marker(logFile, "✔ live       v%d  %s  (%s)", rel.Version, d.opts.URL(dep.App, dep.Env), time.Since(dep.CreatedAt).Round(100*time.Millisecond))
	logFile.Close()
	d.finish(dep, store.Live, "", nil)
	d.emit(ctx, events.DeploymentLive, dep, map[string]any{"version": rel.Version, "url": d.opts.URL(dep.App, dep.Env)})
	d.log.Info("deployed", "app", dep.App, "env", dep.Env, "release", rel.Version, "build", b.ID, "deployment", dep.ID)
	d.collect(dep.App, res.Plan.Deploy.KeepReleases)
}

// releaseError reports a failed release command.
type releaseError struct{ msg string }

func (e *releaseError) Error() string { return e.msg }

// buildVars returns the environment's variables flagged for builds, with
// secrets decrypted, and the secrets' values (to remove from the log).
func (d *Deployer) buildVars(ctx context.Context, app, env string) (map[string]string, []string, error) {
	cfg, err := d.db.Config(ctx, app, env)
	if err != nil {
		return nil, nil, err
	}
	vars := map[string]string{}
	var redact []string
	for _, v := range cfg {
		if !v.Build {
			continue
		}
		if !v.Secret {
			vars[v.Name] = v.Value
			continue
		}
		appID, _, err := d.db.Scope(ctx, app, env)
		if err != nil {
			return nil, nil, err
		}
		value, err := d.opts.Secrets.Open(ctx, appID, v.Scope, v.Name, v.Sealed)
		if err != nil {
			return nil, nil, err
		}
		vars[v.Name] = value
		redact = append(redact, value)
	}
	return vars, redact, nil
}

// publish makes build the environment's current release: it runs the
// release command, rolls the release out (for process apps) and switches
// the router to it. A process app's release becomes current only once its
// instances are healthy; if anything fails, the release is removed and the
// previous one keeps serving. The release runs with cfg, or with the
// environment's variables when cfg is nil. log, if set, receives progress.
func (d *Deployer) publish(ctx context.Context, dep store.Deployment, b store.Build, plan manifest.Plan, reason string, cfg []store.ConfigVar, log io.Writer) (store.Release, error) {
	if log == nil {
		log = io.Discard
	}
	plan.Defaults()
	lock := d.envLock(dep.App, dep.Env)
	lock.Lock()
	defer lock.Unlock()
	defer d.release(ctx, dep.App, dep.Env)()

	_, _, prevErr := d.db.CurrentRelease(ctx, dep.App, dep.Env)
	firstRelease := errors.Is(prevErr, store.ErrNotFound)
	rel, err := d.db.CreateReleaseWith(ctx, dep.App, dep.Env, b.ID, reason, dep.CreatedBy, cfg)
	if err != nil {
		return rel, err
	}
	d.db.SetDeploymentRelease(ctx, dep.ID, rel.ID)
	abandon := func(err error) (store.Release, error) {
		d.db.DeleteRelease(context.WithoutCancel(ctx), rel.ID)
		return rel, err
	}
	if plan.Release != nil && plan.Release.Command != "" {
		d.db.SetDeploymentStatus(ctx, dep.ID, store.Releasing)
		build.Marker(log, "▸ release    v%d: %s", rel.Version, plan.Release.Command)
		if err := d.runOnce(ctx, dep, rel, b, plan, plan.Release.Command, log); err != nil {
			return abandon(&releaseError{"the release command failed: " + err.Error()})
		}
	}
	d.db.SetDeploymentStatus(ctx, dep.ID, store.RollingOut)
	if plan.Kind == manifest.KindProcess {
		running, err := d.db.Formation(ctx, dep.App, dep.Env)
		if err != nil {
			return abandon(err)
		}
		build.Marker(log, "▸ rollout    %s", formationText(plan.WithFormation(running)))
		if err := d.opts.Runtime.Rollout(ctx, dep.App, dep.Env, rel, b, plan); err != nil {
			return abandon(err)
		}
	}
	if err := d.db.MakeCurrent(ctx, rel.ID); err != nil {
		return rel, err
	}
	rel.Current = true
	if err := d.opts.Reload(ctx); err != nil {
		return rel, err
	}
	if plan.Kind == manifest.KindStatic {
		d.opts.Runtime.Retire(dep.App, dep.Env) // processes of an earlier release are no longer needed
	}
	if dep.Env == names.Production {
		d.declareEnvironments(ctx, dep.App, plan, log)
	}
	if firstRelease && plan.Release != nil && plan.Release.OnCreate != "" {
		build.Marker(log, "▸ on_create  %s", plan.Release.OnCreate)
		if err := d.runOnce(ctx, dep, rel, b, plan, plan.Release.OnCreate, log); err != nil {
			build.Marker(log, "             on_create failed (%v); the release is live", err)
		}
	}
	return rel, nil
}

// declareEnvironments makes the environments production's manifest names, and keeps their
// branches as it says. One it no longer names stays: it may have data. A failure here does
// not undo a release that is live, and the log says so.
func (d *Deployer) declareEnvironments(ctx context.Context, app string, plan manifest.Plan, log io.Writer) {
	for name, e := range plan.Environments {
		if name == names.Production || name == "preview" {
			continue
		}
		err := d.ensureEnv(ctx, app, name, store.KindNamed, e.Branch)
		if err == nil && e.Branch != "" {
			err = d.db.SetEnvironmentBranch(ctx, app, name, e.Branch)
		}
		if err != nil {
			build.Marker(log, "             the environment %s could not be made: %v", name, err)
		}
	}
}

func (d *Deployer) runOnce(ctx context.Context, dep store.Deployment, rel store.Release, b store.Build, plan manifest.Plan, command string, log io.Writer) error {
	return d.opts.Runtime.RunOnce(ctx, dep.App, dep.Env, rel, b, plan, command, plan.Release.Timeout.Std(), log)
}

// formationText says how many instances of each process a plan runs.
func formationText(plan manifest.Plan) string {
	var parts []string
	for _, name := range manifest.SortedProcesses(plan.Processes) {
		p := plan.Processes[name]
		parts = append(parts, fmt.Sprintf("%s ×%d", name, *p.Instances))
	}
	return strings.Join(parts, " · ")
}

// PublishRequest asks for a build that needs no building to be released.
type PublishRequest struct {
	App, Env, By string
	// Source is the deployment's source type; Reason the release's.
	Source, Reason string
	Build          store.Build
	Plan           manifest.Plan
	// Config, if not nil, replaces the environment's variables (a rollback
	// that restores an earlier release's configuration).
	Config []store.ConfigVar
	// Warnings are written to the deployment's log.
	Warnings []string
}

// Publish publishes a build that needs no building (an upload, the build
// of an earlier release, or the current build with new variables) at once,
// recording it as a deployment.
func (d *Deployer) Publish(ctx context.Context, req PublishRequest) (store.Release, store.Deployment, error) {
	app, env, b, plan := req.App, req.Env, req.Build, req.Plan
	if req.Reason == "rollback" || req.Reason == "config" {
		plan.Release = nil // release commands run once, when a build is first deployed
	}
	if req.Reason == "deploy" {
		if err := d.checkLock(ctx, app, env); err != nil {
			return store.Release{}, store.Deployment{}, err
		}
		if err := d.ensureEnv(ctx, app, env, store.KindNamed, ""); err != nil {
			return store.Release{}, store.Deployment{}, err
		}
	}
	dep := store.Deployment{
		ID: ids.New(ids.Deployment), App: app, Env: env, SourceType: req.Source, Status: store.RollingOut,
		BuildID: b.ID, CreatedBy: req.By, CreatedAt: time.Now().UTC(), StartedAt: time.Now().UTC(),
	}
	if err := d.db.CreateDeployment(ctx, dep); err != nil {
		return store.Release{}, dep, err
	}
	logFile, err := logs.Open(LogPath(d.opts.DataDir, app, dep.ID), 0, 0)
	if err != nil {
		d.finish(dep, store.Failed, "cannot write the deployment log: "+err.Error(), nil)
		return store.Release{}, dep, err
	}
	defer logFile.Close()
	build.Marker(logFile, "moorage  %s · %s · %s", app, env, dep.ID)
	for _, warning := range req.Warnings {
		build.Marker(logFile, "moorage  warning: %s", warning)
	}
	rel, err := d.publish(ctx, dep, b, plan, req.Reason, req.Config, logFile)
	if err != nil {
		var rollout *reconcile.RolloutError
		if errors.As(err, &rollout) {
			build.Marker(logFile, "✘ failed     %s", rollout.Message)
			d.finish(dep, store.Failed, rollout.Message, rollout.Output)
		} else {
			build.Marker(logFile, "✘ failed     %s", err)
			d.finish(dep, store.Failed, err.Error(), nil)
		}
		return rel, dep, err
	}
	build.Marker(logFile, "✔ live       v%d  %s  (%s)", rel.Version, d.opts.URL(app, env), time.Since(dep.CreatedAt).Round(100*time.Millisecond))
	d.finish(dep, store.Live, "", nil)
	dep.Status, dep.ReleaseID, dep.Version = store.Live, rel.ID, rel.Version
	if req.Reason == "deploy" {
		d.emit(ctx, events.DeploymentLive, dep, map[string]any{"version": rel.Version, "url": d.opts.URL(app, env)})
	} else {
		d.emit(ctx, events.ReleaseCreated, dep, map[string]any{"version": rel.Version, "reason": req.Reason, "url": d.opts.URL(app, env)})
	}
	d.collect(app, plan.Deploy.KeepReleases)
	return rel, dep, nil
}

// collect removes the builds of app that are kept for no release (beyond
// keep per environment), and the directories of failed builds once they are
// old enough.
func (d *Deployer) collect(app string, keep int) {
	ctx := context.Background()
	if keep <= 0 {
		keep = 5
	}
	builds, err := d.db.CollectableBuilds(ctx, app, keep, d.opts.GCMinAge)
	if err != nil {
		d.log.Error("garbage collection", "app", app, "err", err)
		return
	}
	for _, b := range builds {
		if b.ArtifactKind == "image" {
			// Another app may use the same image: it is pulled again if so.
			if engine := d.opts.Builder.Engine; engine != nil {
				if err := engine.RemoveImage(ctx, b.ArtifactRef); err != nil {
					d.log.Warn("remove an old image", "app", app, "build", b.ID, "image", b.ArtifactRef, "err", err)
				}
			}
			removeTree(filepath.Join(d.opts.DataDir, "builds", app, b.ID)) // the source kept for a redeploy
		} else if err := removeTree(filepath.Join(d.opts.DataDir, filepath.FromSlash(b.ArtifactRef))); err != nil {
			d.log.Error("remove a build", "app", app, "build", b.ID, "err", err)
			continue
		}
		d.db.MarkCollected(ctx, b.ID)
		d.log.Info("removed an old build", "app", app, "build", b.ID)
	}
	known, err := d.db.BuildIDs(ctx, app)
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(filepath.Join(d.opts.DataDir, "builds", app))
	for _, e := range entries {
		id := e.Name()
		_, created, err := ids.Parse(id)
		if err != nil || known[id] || !e.IsDir() || time.Since(created) < d.opts.FailedBuildAge {
			continue
		}
		if active, _ := d.activeBuild(ctx, id); active {
			continue
		}
		if err := removeTree(filepath.Join(d.opts.DataDir, "builds", app, id)); err == nil {
			d.log.Info("removed a failed build", "app", app, "build", id)
		}
	}
}

func (d *Deployer) activeBuild(ctx context.Context, buildID string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, q := range d.queues {
		if q.next != nil && q.next.BuildID == buildID {
			return true, nil
		}
	}
	return false, nil
}

// removeTree removes a build directory, whose files are read-only.
func removeTree(dir string) error {
	if runtime.GOOS == "windows" {
		filepath.WalkDir(dir, func(p string, de fs.DirEntry, err error) error {
			if err == nil && !de.IsDir() {
				os.Chmod(p, 0o644)
			}
			return nil
		})
	}
	return os.RemoveAll(dir)
}

// lastOutput returns the last n lines of a log's output, for a failed
// deployment's record.
func lastOutput(path string, n int) []string {
	lines, _ := logs.Tail(path, 200)
	var out []string
	for _, l := range lines {
		if l.Stream == logs.Stdout || l.Stream == logs.Stderr {
			out = append(out, l.Text)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
