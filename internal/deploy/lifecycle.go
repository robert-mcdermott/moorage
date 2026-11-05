package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// AppUsage says how much room an app takes.
type AppUsage struct {
	Builds, Logs, Repository, Data int64
}

// Total is all of it.
func (u AppUsage) Total() int64 { return u.Builds + u.Logs + u.Repository + u.Data }

// appDirs are the places an app has files, under the data directory.
type appDirs struct {
	builds, repository, data string
	logs                     []string
}

func (d *Deployer) dirsOf(app string) appDirs {
	root := d.opts.DataDir
	dirs := appDirs{
		builds: filepath.Join(root, "builds", app),
		data:   filepath.Join(root, "data", app),
		logs: []string{
			filepath.Join(root, "logs", "apps", app),
			filepath.Dir(LogPath(root, app, "x")),
			filepath.Join(root, "logs", "router", app),
		},
		repository: filepath.Join(root, "repos", app+".git"),
	}
	if d.opts.Repos != nil {
		dirs.repository = d.opts.Repos.Path(app)
	}
	return dirs
}

func (a appDirs) all() []string {
	return append([]string{a.builds, a.data, a.repository}, a.logs...)
}

func sizeOf(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // what cannot be read counts for nothing
		}
		if info, err := entry.Info(); err == nil && !entry.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Usage says how much room an app takes.
func (d *Deployer) Usage(app string) AppUsage {
	dirs := d.dirsOf(app)
	usage := AppUsage{Builds: sizeOf(dirs.builds), Repository: sizeOf(dirs.repository), Data: sizeOf(dirs.data)}
	for _, dir := range dirs.logs {
		usage.Logs += sizeOf(dir)
	}
	return usage
}

// AppBusyError says that a deployment of an app is under way.
type AppBusyError struct{ App string }

func (e *AppBusyError) Error() string {
	return "a deployment of " + e.App + " is under way: wait for it to end"
}

// NameTakenError says that an app already has that name.
type NameTakenError struct{ Name string }

func (e *NameTakenError) Error() string { return "there is already an app named " + e.Name }

// InvalidNameError says why an app cannot be called that.
type InvalidNameError struct{ Err error }

func (e *InvalidNameError) Error() string { return e.Err.Error() }

// quiesce takes an app's environments out of service, once no deployment is under way, and
// returns with their release locks held.
func (d *Deployer) quiesce(ctx context.Context, app string) (release func(), err error) {
	envs, err := d.db.Environments(ctx, app)
	if err != nil {
		return nil, err
	}
	if len(envs) == 0 {
		return nil, fmt.Errorf("no app %q: %w", app, store.ErrNotFound)
	}
	var held []func()
	release = func() {
		for _, unlock := range held {
			unlock()
		}
	}
	for _, e := range envs {
		d.cancelRuns(app, e.Name)
		lock := d.envLock(app, e.Name)
		lock.Lock()
		held = append(held, lock.Unlock)
		deps, err := d.db.Deployments(ctx, app, e.Name, 1)
		if err == nil && len(deps) > 0 && !store.Final(deps[0].Status) {
			err = &AppBusyError{app}
		}
		if err != nil {
			release()
			return nil, err
		}
	}
	if d.opts.Runtime != nil {
		for _, e := range envs {
			d.opts.Runtime.Retire(app, e.Name)
		}
		d.opts.Runtime.Forget(app)
	}
	if d.opts.ForgetApp != nil {
		d.opts.ForgetApp(app)
	}
	return release, nil
}

// DeleteApp removes an app: its instances, variables, releases, builds, logs and
// repository, and unless keepData says otherwise, its data directory.
func (d *Deployer) DeleteApp(ctx context.Context, app, by string, keepData bool) error {
	done, err := d.quiesce(ctx, app)
	if err != nil {
		return err
	}
	defer done()
	builds, err := d.db.AppImages(ctx, app)
	if err != nil {
		return err
	}
	if err := d.db.DeleteApp(ctx, app); err != nil {
		return err
	}
	if d.opts.Reload != nil {
		if err := d.opts.Reload(ctx); err != nil {
			d.log.Warn("reload the routes after deleting an app", "app", app, "err", err)
		}
	}
	if b := d.opts.Builder; b != nil && b.Engines.Engine() != nil {
		for _, image := range builds {
			if err := b.Engines.Engine().RemoveImage(ctx, image); err != nil {
				d.log.Warn("remove an image of a deleted app", "app", app, "image", image, "err", err)
			}
		}
	}
	dirs := d.dirsOf(app)
	for _, dir := range dirs.all() {
		if keepData && dir == dirs.data {
			continue
		}
		if err := removeTree(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			d.log.Warn("remove the files of a deleted app", "app", app, "dir", dir, "err", err)
		}
	}
	d.reloadAsleep(ctx, app)
	d.opts.Events.Publish(ctx, store.Event{Type: events.AppDeleted, App: app, Actor: by, Data: map[string]any{"kept_data": keepData}})
	d.log.Info("deleted an app", "app", app, "by", by, "kept_data", keepData)
	return nil
}

// RenameApp gives an app another name, which is another address and another git remote.
// Its files move, and its paths are in what its instances run with, so they stop and
// start again under the new name.
func (d *Deployer) RenameApp(ctx context.Context, app, name, by string) error {
	if err := names.ValidateApp(name); err != nil {
		return &InvalidNameError{err}
	}
	if name == app {
		return nil
	}
	if _, err := d.db.EnvID(ctx, name, names.Production); err == nil {
		return &NameTakenError{name}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	done, err := d.quiesce(ctx, app)
	if err != nil {
		return err
	}
	defer done()
	from, to := d.dirsOf(app), d.dirsOf(name)
	moved := [][2]string{}
	undo := func() {
		for i := len(moved) - 1; i >= 0; i-- {
			if err := platform.RenameDir(moved[i][1], moved[i][0]); err != nil {
				d.log.Error("put an app's files back", "from", moved[i][1], "to", moved[i][0], "err", err)
			}
		}
	}
	targets := to.all()
	for i, dir := range from.all() {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(targets[i]), 0o755); err != nil {
			undo()
			return err
		}
		if err := platform.RenameDir(dir, targets[i]); err != nil {
			undo()
			return fmt.Errorf("move %s: %w", dir, err)
		}
		moved = append(moved, [2]string{dir, targets[i]})
	}
	if err := d.db.RenameApp(ctx, app, name); err != nil {
		undo()
		return err
	}
	if d.opts.Reload != nil {
		if err := d.opts.Reload(ctx); err != nil {
			d.log.Warn("reload the routes after renaming an app", "app", name, "err", err)
		}
	}
	d.reloadAsleep(ctx, app)
	d.opts.Events.Publish(ctx, store.Event{Type: events.AppRenamed, App: name, Actor: by, Data: map[string]any{"from": app}})
	d.log.Info("renamed an app", "from", app, "to", name, "by", by)
	if d.opts.Runtime != nil {
		envs, err := d.db.Environments(ctx, name)
		if err != nil {
			return err
		}
		var failed error
		for _, e := range envs {
			// A static site has no instances to start, nor an environment with no release.
			_, b, err := d.db.CurrentRelease(ctx, name, e.Name)
			if err != nil {
				continue
			}
			var plan manifest.Plan
			if json.Unmarshal(b.Plan, &plan) == nil && plan.Kind == manifest.KindProcess {
				if err := d.opts.Runtime.Restart(ctx, name, e.Name, ""); err != nil && failed == nil {
					failed = err
				}
			}
		}
		return failed
	}
	return nil
}
