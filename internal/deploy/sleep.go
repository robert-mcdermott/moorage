package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// NotSleepableError says why an environment cannot sleep.
type NotSleepableError struct{ Msg string }

func (e *NotSleepableError) Error() string { return e.Msg }

func sleepKey(app, env string) string { return app + "/" + env }

// loadAsleep learns which environments are asleep, when the server starts.
func (d *Deployer) loadAsleep(ctx context.Context) error {
	list, err := d.db.AllEnvironments(ctx)
	if err != nil {
		return err
	}
	for _, e := range list {
		if !e.AsleepAt.IsZero() {
			d.asleep.Store(sleepKey(e.App, e.Name), true)
		}
	}
	return nil
}

// reloadAsleep learns again which environments of an app are asleep, as its name or its
// environments change.
func (d *Deployer) reloadAsleep(ctx context.Context, app string) {
	prefix := app + "/"
	d.asleep.Range(func(key, _ any) bool {
		if strings.HasPrefix(key.(string), prefix) {
			d.asleep.Delete(key)
		}
		return true
	})
	if err := d.loadAsleep(ctx); err != nil {
		d.log.Warn("read which environments are asleep", "err", err)
	}
}

// Asleep says whether an environment is asleep. A request to one wakes it. It goes on saying
// so while one wakes, so that requests wait for it rather than finding nothing to answer them.
func (d *Deployer) Asleep(app, env string) bool {
	_, asleep := d.asleep.Load(sleepKey(app, env))
	return asleep
}

// Sleep stops an environment's instances, except processes with sleep = false, until
// something wakes it. by is who asked; nobody did if it was left idle.
func (d *Deployer) Sleep(ctx context.Context, app, env, by string) error {
	_, b, err := d.db.CurrentRelease(ctx, app, env)
	if errors.Is(err, store.ErrNotFound) {
		return &NotSleepableError{env + " of " + app + " has nothing deployed to stop"}
	}
	if err != nil {
		return err
	}
	var plan manifest.Plan
	if err := json.Unmarshal(b.Plan, &plan); err != nil {
		return err
	}
	if plan.Kind != manifest.KindProcess {
		return &NotSleepableError{"a static site has nothing to stop: it never sleeps"}
	}
	lock := d.envLock(app, env)
	lock.Lock()
	defer lock.Unlock()
	if d.Asleep(app, env) {
		return nil
	}
	if err := d.db.SetAsleep(ctx, app, env, true); err != nil {
		return err
	}
	d.asleep.Store(sleepKey(app, env), true)
	if err := d.opts.Runtime.Settle(ctx, app, env); err != nil {
		// Nothing has stopped, and it should not stop by itself an hour from now, when the engine is back and no one remembers asking.
		if errors.Is(err, container.ErrNotAnswering) || errors.Is(err, container.ErrNoEngine) {
			d.asleep.Delete(sleepKey(app, env))
			if undo := d.db.SetAsleep(context.WithoutCancel(ctx), app, env, false); undo != nil {
				d.log.Error("wake an environment that could not go to sleep", "app", app, "env", env, "err", undo)
			}
		}
		return err
	}
	// Its services stop with it, and keep what they hold, unless something goes on running that needs them.
	if !needsServicesAsleep(plan) {
		if err := d.opts.Services.Sleep(ctx, app, env); err != nil {
			d.log.Warn("stop the services of an environment going to sleep", "app", app, "env", env, "err", err)
		}
	}
	d.opts.Events.Publish(ctx, store.Event{Type: events.EnvironmentSlept, App: app, Env: env, Actor: by})
	d.log.Info("an environment went to sleep", "app", app, "env", env, "by", by)
	return nil
}

// Wake starts an environment that is asleep, and returns once it is ready.
func (d *Deployer) Wake(ctx context.Context, app, env, by string) error {
	lock := d.envLock(app, env)
	lock.Lock()
	defer lock.Unlock()
	if !d.Asleep(app, env) {
		return nil
	}
	defer d.asleep.Delete(sleepKey(app, env))
	if err := d.db.SetAsleep(ctx, app, env, false); err != nil {
		return err
	}
	// Whatever woke it, its time starts again now.
	if err := d.db.SetLastActive(ctx, app, env, time.Now()); err != nil {
		return err
	}
	// What it connects to comes first.
	err := d.opts.Services.Wake(ctx, app, env)
	if err == nil {
		err = d.opts.Runtime.Settle(ctx, app, env)
	}
	d.opts.Events.Publish(ctx, store.Event{Type: events.EnvironmentWoke, App: app, Env: env, Actor: by})
	d.log.Info("an environment woke", "app", app, "env", env, "by", by, "err", err)
	return err
}

// WakeFor wakes an environment for a request, once however many are waiting, and returns what
// closes when it is over. It goes on when those who asked have gone.
func (d *Deployer) WakeFor(app, env string) <-chan struct{} {
	d.wakingMu.Lock()
	defer d.wakingMu.Unlock()
	key := sleepKey(app, env)
	if done, ok := d.waking[key]; ok {
		return done
	}
	done := make(chan struct{})
	d.waking[key] = done
	go func() {
		defer func() {
			d.wakingMu.Lock()
			delete(d.waking, key)
			d.wakingMu.Unlock()
			close(done)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := d.Wake(ctx, app, env, ""); err != nil {
			d.log.Warn("waking an environment", "app", app, "env", env, "err", err)
		}
	}()
	return done
}

// release marks an environment that is about to get a release as awake: the release wakes it.
// It says it is asleep until the rollout ends, with the lock of the environment held, so that a
// request meanwhile waits for the rollout. The function returned ends that.
func (d *Deployer) release(ctx context.Context, app, env string) func() {
	if !d.Asleep(app, env) {
		return func() {}
	}
	if err := d.db.SetAsleep(ctx, app, env, false); err != nil {
		d.log.Warn("waking an environment for a release", "app", app, "env", env, "err", err)
	}
	if err := d.db.SetLastActive(ctx, app, env, time.Now()); err != nil {
		d.log.Warn("waking an environment for a release", "app", app, "env", env, "err", err)
	}
	return func() {
		d.asleep.Delete(sleepKey(app, env))
		d.opts.Events.Publish(context.WithoutCancel(ctx), store.Event{Type: events.EnvironmentWoke, App: app, Env: env, Data: map[string]any{"reason": "a release"}})
	}
}

// sleepAfter says how long an environment may sit idle, from its current build's manifest: 0
// is for ever, and for what has no processes to stop.
func (d *Deployer) sleepAfter(ctx context.Context, e store.Environment) time.Duration {
	_, b, err := d.db.CurrentRelease(ctx, e.App, e.Name)
	if err != nil {
		return 0
	}
	var plan manifest.Plan
	if json.Unmarshal(b.Plan, &plan) != nil || plan.Kind != manifest.KindProcess {
		return 0
	}
	return plan.SleepAfter(e.Name, e.Kind)
}

// sweepSleep puts an environment to sleep if it has been idle long enough, with nothing still
// connected to it, and nothing on its way.
func (d *Deployer) sweepSleep(ctx context.Context, e store.Environment, now time.Time) {
	after := d.sleepAfter(ctx, e)
	if after == 0 || d.Asleep(e.App, e.Name) {
		return
	}
	active := d.EnvironmentActive(ctx, e)
	if d.started.After(active) {
		active = d.started // what it did while the server was down is not known
	}
	if now.Sub(active) < after {
		return
	}
	if d.opts.Busy != nil && d.opts.Busy(e.App, e.Name) {
		return
	}
	if deps, err := d.db.Deployments(ctx, e.App, e.Name, 1); err != nil || len(deps) > 0 && !store.Final(deps[0].Status) {
		return
	}
	if err := d.Sleep(ctx, e.App, e.Name, ""); err != nil {
		d.log.Warn("put an idle environment to sleep", "app", e.App, "env", e.Name, "err", fmt.Sprint(err))
	}
}
