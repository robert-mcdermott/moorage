package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// MaxPreviews is how many previews an app may have at once.
const MaxPreviews = 10

// DefaultPreviewTTL is how long a preview lasts without a deployment or a request.
const DefaultPreviewTTL = 7 * 24 * time.Hour

// LockedError says that an environment takes no deployments for now.
type LockedError struct{ Env, By string }

func (e *LockedError) Error() string {
	return e.Env + " is locked by " + e.By + ": unlock it to deploy to it"
}

// checkLock refuses a deployment to a locked environment.
func (d *Deployer) checkLock(ctx context.Context, app, env string) error {
	e, err := d.db.Environment(ctx, app, env)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if e.LockedBy != "" {
		return &LockedError{env, e.LockedBy}
	}
	return nil
}

// Lock stops deployments to an environment, until Unlock: so that a rollback, say, is not
// overwritten by the next push. Rollbacks, promotions and variables go on working.
func (d *Deployer) Lock(ctx context.Context, app, env, by string) error {
	if err := d.db.SetLocked(ctx, app, env, by); err != nil {
		return err
	}
	d.opts.Events.Publish(ctx, store.Event{Type: events.EnvironmentLocked, App: app, Env: env, Actor: by})
	return nil
}

// Unlock lets deployments to an environment go on.
func (d *Deployer) Unlock(ctx context.Context, app, env, by string) error {
	if err := d.db.SetLocked(ctx, app, env, ""); err != nil {
		return err
	}
	d.opts.Events.Publish(ctx, store.Event{Type: events.EnvironmentUnlocked, App: app, Env: env, Actor: by})
	return nil
}

// Promote releases the exact build that one environment runs to another, which is made if
// it does not exist. It needs no building, so it takes seconds, and a release command runs.
func (d *Deployer) Promote(ctx context.Context, app, from, to, by string) (store.Release, store.Deployment, error) {
	if err := d.checkLock(ctx, app, to); err != nil {
		return store.Release{}, store.Deployment{}, err
	}
	_, b, err := d.db.CurrentRelease(ctx, app, from)
	if err != nil {
		return store.Release{}, store.Deployment{}, err
	}
	var plan manifest.Plan
	if err := json.Unmarshal(b.Plan, &plan); err != nil {
		return store.Release{}, store.Deployment{}, err
	}
	if _, err := d.db.CreateEnvironment(ctx, app, to, store.KindNamed, ""); err != nil {
		return store.Release{}, store.Deployment{}, err
	}
	return d.Publish(ctx, PublishRequest{App: app, Env: to, By: by, Source: "promote", Reason: "promote", Build: b, Plan: plan, Warnings: d.PromotionWarnings(ctx, app, from, to)})
}

// PromotionWarnings says what a promotion may surprise someone with: that the build was
// made with build variables other than those of the environment it goes to.
func (d *Deployer) PromotionWarnings(ctx context.Context, app, from, to string) []string {
	built, err := d.db.Config(ctx, app, from)
	if err != nil {
		return nil
	}
	going, err := d.db.Config(ctx, app, to)
	if err != nil {
		return nil
	}
	buildVars := func(list []store.ConfigVar) map[string]store.ConfigVar {
		out := map[string]store.ConfigVar{}
		for _, v := range list {
			if v.Build {
				out[v.Name] = v
			}
		}
		return out
	}
	a, b := buildVars(built), buildVars(going)
	var differing []string
	for name, v := range a {
		if w, ok := b[name]; !ok || w.Value != v.Value || w.Scope != v.Scope && (v.Secret || w.Secret) {
			differing = append(differing, name)
		}
	}
	for name := range b {
		if _, ok := a[name]; !ok {
			differing = append(differing, name)
		}
	}
	if len(differing) == 0 {
		return nil
	}
	sort.Strings(differing)
	return []string{fmt.Sprintf("this build was made with other values of %s than %s has; what it was built with stays in it, and moor redeploy would build it with these", strings.Join(differing, ", "), to)}
}

// NotPreviewableError says why there can be no preview of a branch.
type NotPreviewableError struct{ Msg string }

func (e *NotPreviewableError) Error() string { return e.Msg }

// PreviewName finds the environment of a branch's preview, making it if needs be: its name
// is a slug of the branch's, with a short hash if another branch has that slug.
func (d *Deployer) PreviewName(ctx context.Context, app, branch string) (string, error) {
	if e, err := d.db.EnvironmentOfBranch(ctx, app, branch); err == nil {
		return e.Name, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	envs, err := d.db.Environments(ctx, app)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	previews := 0
	taken := map[string]bool{}
	for _, e := range envs {
		taken[e.Name] = true
		if e.Kind == store.KindPreview {
			previews++
		}
	}
	if previews >= MaxPreviews {
		return "", &NotPreviewableError{fmt.Sprintf("%s has %d previews, which is as many as an app may have: destroy one, or delete its branch", app, previews)}
	}
	name := names.Slug(branch)
	if taken[name] {
		name = names.WithHash(name, branch)
	}
	return name, nil
}

// PreviewBranch deploys a preview of a branch that was pushed, from its newest commit.
func (d *Deployer) PreviewBranch(ctx context.Context, app, branch, by string) (store.Deployment, error) {
	if d.opts.Repos == nil || !d.opts.Repos.Exists(app) {
		return store.Deployment{}, fmt.Errorf("%s has no repository to find %s in: push it: %w", app, branch, store.ErrNotFound)
	}
	commit, message, err := d.opts.Repos.BranchHead(app, branch)
	if err != nil {
		return store.Deployment{}, fmt.Errorf("%s has no branch %s: %w", app, branch, store.ErrNotFound)
	}
	name, err := d.PreviewName(ctx, app, branch)
	if err != nil {
		return store.Deployment{}, err
	}
	dep, _, err := d.SubmitSource(ctx, app, name, by, Source{
		Type: SourcePush, Ref: "refs/heads/" + branch, Commit: commit, Message: firstLine(message), EnvKind: store.KindPreview, Branch: branch,
		Fill: func(dir string) (int, error) {
			ex, err := d.opts.Repos.Export(app, commit, dir)
			return ex.Files, err
		},
	})
	return dep, err
}

// branchTarget says where a push of a branch deploys, from what its commit's manifest
// says: to a named environment, to a preview, or nowhere, and why.
func branchTarget(plan *manifest.Manifest, branch string) (env, kind, skip string) {
	if plan != nil {
		for name, e := range plan.Environments {
			if e.Branch == branch && name != "preview" && name != names.Production {
				return name, store.KindNamed, ""
			}
		}
		if p := plan.Previews; p != nil {
			if p.Enabled != nil && !*p.Enabled {
				return "", "", "previews are turned off in its moorage.toml"
			}
			if !branchMatches(p.Branches, branch) {
				return "", "", "its moorage.toml's [previews] branches leave it out"
			}
		}
	}
	return "", store.KindPreview, ""
}

// branchMatches applies [previews] branches: globs, where ! leaves some out and * crosses
// slashes, as branches have them. With none, or only some left out, the others all match.
func branchMatches(globs []string, branch string) bool {
	matched, anyIncluded := false, false
	for _, glob := range globs {
		rest, excluded := strings.CutPrefix(glob, "!")
		if !excluded {
			anyIncluded = true
		}
		if globMatch(rest, branch) {
			if excluded {
				return false
			}
			matched = true
		}
	}
	return matched || !anyIncluded
}

// globMatch says whether a name matches a glob: * is any characters and ? any one.
func globMatch(glob, name string) bool {
	star, mark := -1, 0
	g, n := 0, 0
	for n < len(name) {
		switch {
		case g < len(glob) && (glob[g] == '?' || glob[g] == name[n]) && glob[g] != '*':
			g++
			n++
		case g < len(glob) && glob[g] == '*':
			star, mark = g, n
			g++
		case star >= 0:
			mark++
			g, n = star+1, mark
		default:
			return false
		}
	}
	for g < len(glob) && glob[g] == '*' {
		g++
	}
	return g == len(glob)
}

// DeleteEnvironment removes an environment that is not production: its instances, variables,
// releases, deployments, data and logs.
func (d *Deployer) DeleteEnvironment(ctx context.Context, app, env, by string) error {
	e, err := d.db.Environment(ctx, app, env)
	if err != nil {
		return err
	}
	if e.Kind == store.KindProduction {
		return &CannotDeleteProductionError{}
	}
	lock := d.envLock(app, env)
	lock.Lock()
	defer lock.Unlock()
	deps, err := d.db.Deployments(ctx, app, env, 1)
	if err == nil && len(deps) > 0 && !store.Final(deps[0].Status) {
		return &AppBusyError{app + " " + env}
	}
	if err != nil {
		return err
	}
	if d.opts.Runtime != nil {
		d.opts.Runtime.Retire(app, env)
		d.opts.Runtime.ForgetEnv(app, env)
	}
	if d.opts.ForgetApp != nil {
		d.opts.ForgetApp(app)
	}
	ids, err := d.db.EnvironmentDeployments(ctx, app, env)
	if err != nil {
		return err
	}
	if err := d.db.DeleteEnvironment(ctx, app, env); err != nil {
		return err
	}
	if d.opts.Reload != nil {
		if err := d.opts.Reload(ctx); err != nil {
			d.log.Warn("reload the routes after deleting an environment", "app", app, "env", env, "err", err)
		}
	}
	root := d.opts.DataDir
	for _, dir := range []string{filepath.Join(root, "data", app, env), filepath.Join(root, "logs", "apps", app, env)} {
		if err := removeTree(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			d.log.Warn("remove the files of a deleted environment", "app", app, "env", env, "err", err)
		}
	}
	os.Remove(filepath.Join(root, "logs", "router", app, env+".log"))
	for _, id := range ids {
		os.Remove(LogPath(root, app, id))
	}
	d.asleep.Delete(sleepKey(app, env))
	d.opts.Events.Publish(ctx, store.Event{Type: events.EnvironmentDeleted, App: app, Env: env, Actor: by})
	d.log.Info("deleted an environment", "app", app, "env", env, "by", by)
	return nil
}

// CannotDeleteProductionError says that an app has one environment it cannot do without.
type CannotDeleteProductionError struct{}

func (*CannotDeleteProductionError) Error() string {
	return "production cannot be deleted: delete the app"
}

// PreviewTTL says how long a preview lasts, from what its current build's manifest says.
func (d *Deployer) PreviewTTL(ctx context.Context, app, env string) time.Duration {
	if _, b, err := d.db.CurrentRelease(ctx, app, env); err == nil {
		var plan manifest.Plan
		if json.Unmarshal(b.Plan, &plan) == nil && plan.Previews != nil && plan.Previews.TTL > 0 {
			return plan.Previews.TTL.Std()
		}
	}
	return DefaultPreviewTTL
}

// EnvironmentActive says when an environment last did something: a deployment or a request.
func (d *Deployer) EnvironmentActive(ctx context.Context, e store.Environment) time.Time {
	active := e.CreatedAt
	if e.LastActive.After(active) {
		active = e.LastActive
	}
	if e.ReleasedAt.After(active) {
		active = e.ReleasedAt
	}
	if deps, err := d.db.Deployments(ctx, e.App, e.Name, 1); err == nil && len(deps) > 0 {
		for _, at := range []time.Time{deps[0].CreatedAt, deps[0].FinishedAt} {
			if at.After(active) {
				active = at
			}
		}
	}
	return active
}

// SweepEnvironments notes how recently each environment had a request, destroys the previews
// that have outlasted their time, unless they are pinned, and puts to sleep those left idle.
func (d *Deployer) SweepEnvironments(ctx context.Context, now time.Time) {
	list, err := d.db.AllEnvironments(ctx)
	if err != nil {
		d.log.Warn("look at the environments", "err", err)
		return
	}
	for _, e := range list {
		if e.Kind != store.KindPreview && d.sleepAfter(ctx, e) == 0 {
			continue
		}
		if d.opts.LastActive != nil {
			if at := d.opts.LastActive(e.App, e.Name); at.After(e.LastActive) {
				if err := d.db.SetLastActive(ctx, e.App, e.Name, at); err == nil {
					e.LastActive = at
				}
			}
		}
		if e.Kind == store.KindPreview && !e.Pinned && now.Sub(d.EnvironmentActive(ctx, e)) >= d.PreviewTTL(ctx, e.App, e.Name) {
			d.log.Info("a preview has outlasted its time", "app", e.App, "env", e.Name)
			if err := d.DeleteEnvironment(ctx, e.App, e.Name, "moorage"); err != nil {
				d.log.Warn("destroy a preview", "app", e.App, "env", e.Name, "err", err)
			}
			continue
		}
		d.sweepSleep(ctx, e, now)
	}
}

// Sweep looks at the environments every so often, until ctx ends.
func (d *Deployer) Sweep(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.SweepEnvironments(ctx, time.Now())
		}
	}
}
