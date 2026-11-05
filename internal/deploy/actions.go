package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/archive"
	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// What the API and the console both do to an app that is running: the same work, with
// the same result, whichever asks.

// MaxUpload bounds the size of an upload's request body.
const MaxUpload = 1 << 30

// sourceArchive is the copy of an upload kept with its build, for a redeploy.
const sourceArchive = "source.tar.gz"

// appDir is where a build's source goes, and where it builds.
func appDir(buildDir string) string { return filepath.Join(buildDir, "app") }

// fillKeeping extracts an uploaded archive into the app directory, keeping a copy
// beside it: the build changes what it extracts, and a redeploy starts from the source.
func fillKeeping(upload io.Reader) func(dir string) (int, error) {
	return func(dir string) (int, error) {
		kept, err := os.OpenFile(filepath.Join(filepath.Dir(dir), sourceArchive), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			return 0, err
		}
		defer kept.Close()
		tee := io.TeeReader(upload, kept)
		files, _, err := archive.Extract(tee, dir, archive.DefaultLimits)
		if err != nil {
			return files, err
		}
		// The extraction stops at the end of the archive, short of what closes it.
		if _, err := io.Copy(io.Discard, tee); err != nil {
			return files, err
		}
		return files, kept.Close()
	}
}

// EngineStatus says whether this server can run containers, and why not.
func (d *Deployer) EngineStatus(ctx context.Context) container.Status {
	if d == nil || d.opts.Builder == nil {
		return container.Status{State: container.Absent}
	}
	return d.opts.Builder.Engines.Status(ctx)
}

// WantsEngine says whether anything here runs in a container.
func (d *Deployer) WantsEngine(ctx context.Context) bool {
	if d == nil {
		return false
	}
	routes, err := d.db.Routes(ctx)
	if err != nil {
		return false
	}
	for _, r := range routes {
		var plan manifest.Plan
		if json.Unmarshal(r.Build.Plan, &plan) == nil && plan.Container != nil {
			return true
		}
	}
	return false
}

// DeployImage queues the deployment of an image that comes without a manifest.
func (d *Deployer) DeployImage(ctx context.Context, app, env, by, image string) (store.Deployment, error) {
	var empty bytes.Buffer
	gz := gzip.NewWriter(&empty)
	if err := tar.NewWriter(gz).Close(); err != nil {
		return store.Deployment{}, err
	}
	if err := gz.Close(); err != nil {
		return store.Deployment{}, err
	}
	dep, _, err := d.SubmitImage(ctx, app, env, by, image, &empty)
	return dep, err
}

// CannotRollBackError says why an environment cannot go back to a release.
type CannotRollBackError struct{ Err error }

func (e *CannotRollBackError) Error() string { return e.Err.Error() }
func (e *CannotRollBackError) Unwrap() error { return e.Err }

// Rollback publishes an earlier release again, as a new release: version 0 is the one
// before the current one. With withConfig, it also restores the variables that release
// ran with.
func (d *Deployer) Rollback(ctx context.Context, app, env, by string, version int, withConfig bool) (store.Release, store.Deployment, error) {
	buildID, version, err := d.db.RollbackBuild(ctx, app, env, version)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Release{}, store.Deployment{}, err
		}
		return store.Release{}, store.Deployment{}, &CannotRollBackError{err}
	}
	b, err := d.db.Build(ctx, buildID)
	if err != nil {
		return store.Release{}, store.Deployment{}, err
	}
	var plan manifest.Plan
	if err := json.Unmarshal(b.Plan, &plan); err != nil {
		return store.Release{}, store.Deployment{}, err
	}
	var cfg []store.ConfigVar
	if withConfig {
		if cfg, err = d.db.ReleaseConfig(ctx, app, env, version); err != nil {
			return store.Release{}, store.Deployment{}, err
		}
		if cfg == nil {
			cfg = []store.ConfigVar{} // that release ran without server variables
		}
	}
	return d.Publish(ctx, PublishRequest{App: app, Env: env, By: by, Source: "rollback", Reason: "rollback", Build: b, Plan: plan, Config: cfg})
}

// CannotRedeployError says why the current release cannot be built again.
type CannotRedeployError struct{ Msg string }

func (e *CannotRedeployError) Error() string { return e.Msg }

// Redeploy builds what the current release was built from again, and rolls it out: a
// toolchain may have moved, an image's tag may point elsewhere, and build variables
// reach an app only through a build.
func (d *Deployer) Redeploy(ctx context.Context, app, env, by string) (store.Deployment, error) {
	_, b, err := d.db.CurrentRelease(ctx, app, env)
	if err != nil {
		return store.Deployment{}, err
	}
	src, err := d.sourceOf(ctx, app, b)
	if err != nil {
		return store.Deployment{}, err
	}
	dep, _, err := d.SubmitSource(ctx, app, env, by, src)
	return dep, err
}

// sourceOf finds what a build was made from.
func (d *Deployer) sourceOf(ctx context.Context, app string, b store.Build) (Source, error) {
	switch b.SourceType {
	case SourcePush:
		made, err := d.db.BuildDeployment(ctx, b.ID)
		if err != nil {
			return Source{}, err
		}
		if d.opts.Repos == nil || !d.opts.Repos.Exists(app) || made.SourceCommit == "" {
			return Source{}, &CannotRedeployError{fmt.Sprintf("the commit of this build is no longer in the repository of %s: push it again", app)}
		}
		return Source{
			Type: SourcePush, Ref: made.SourceRef, Commit: made.SourceCommit, Message: made.SourceMessage,
			Fill: func(dir string) (int, error) {
				ex, err := d.opts.Repos.Export(app, made.SourceCommit, dir)
				return ex.Files, err
			},
		}, nil
	case SourceUpload, SourceImage:
		made, err := d.db.BuildDeployment(ctx, b.ID)
		if err != nil {
			return Source{}, err
		}
		kept, err := os.Open(filepath.Join(d.opts.DataDir, "builds", app, b.ID, sourceArchive))
		if errors.Is(err, os.ErrNotExist) {
			// Builds from before sources were kept.
			return Source{}, &CannotRedeployError{"the source of this build was not kept: deploy it again from its directory"}
		}
		if err != nil {
			return Source{}, err
		}
		return Source{
			Type: b.SourceType, Ref: made.SourceRef,
			Fill: func(dir string) (int, error) {
				defer kept.Close()
				return fillKeeping(kept)(dir)
			},
		}, nil
	}
	return Source{}, &CannotRedeployError{"this release was uploaded ready to run, so there is nothing to build: deploy it again from its directory, or restart it"}
}

// Restart replaces an environment's instances without downtime.
func (d *Deployer) Restart(ctx context.Context, app, env, process, by string) error {
	if err := d.Wake(ctx, app, env, by); err != nil {
		return err
	}
	if err := d.opts.Runtime.Restart(ctx, app, env, process); err != nil {
		return err
	}
	data := map[string]any{}
	if process != "" {
		data["process"] = process
	}
	d.opts.Events.Publish(ctx, store.Event{Type: events.AppRestarted, App: app, Env: env, Actor: by, Data: data})
	return nil
}

// Scale changes how many instances of the named processes an environment runs, and
// returns how many of each it now runs.
func (d *Deployer) Scale(ctx context.Context, app, env, by string, change map[string]int) (map[string]int, error) {
	if err := d.Wake(ctx, app, env, by); err != nil {
		return nil, err
	}
	running, err := d.opts.Runtime.Scale(ctx, app, env, change)
	if err != nil {
		return nil, err
	}
	d.opts.Events.Publish(ctx, store.Event{
		Type: events.ScaleChanged, App: app, Env: env, Actor: by,
		Data: map[string]any{"formation": FormationText(running), "processes": running},
	})
	return running, nil
}

// FormationText says how many instances of each process run.
func FormationText(formation map[string]int) string {
	names := make([]string, 0, len(formation))
	for name := range formation {
		names = append(names, name)
	}
	sort.Strings(names)
	sort.SliceStable(names, func(i, j int) bool { return names[i] == manifest.Web && names[j] != manifest.Web })
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s ×%d", name, formation[name]))
	}
	return strings.Join(parts, " · ")
}
