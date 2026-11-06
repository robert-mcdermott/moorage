// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"time"

	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/services"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// ServiceRow is how a service stands, and whether what is running asks for it.
type ServiceRow struct {
	services.Status
	// Unused says that the current release's manifest does not name it. It stays until someone removes it, as nothing deletes data because a line went from a file.
	Unused bool
}

// ServiceList says how an environment's services stand.
func (d *Deployer) ServiceList(ctx context.Context, app, env string) ([]ServiceRow, error) {
	list, err := d.opts.Services.List(ctx, app, env)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	named := map[string]bool{}
	if _, b, err := d.db.CurrentRelease(ctx, app, env); err == nil {
		var plan manifest.Plan
		if json.Unmarshal(b.Plan, &plan) == nil {
			kind, err := d.db.EnvKind(ctx, app, env)
			if err != nil {
				return nil, err
			}
			for name, s := range plan.Services {
				// A preview that is told that it has no use for one is not given its address.
				named[name] = kind != store.KindPreview || s.Previews != "none"
			}
		}
	}
	rows := make([]ServiceRow, 0, len(list))
	for _, st := range list {
		rows = append(rows, ServiceRow{Status: st, Unused: !named[st.Name]})
	}
	return rows, nil
}

// ServiceTotals says what an app's services add up to, across its environments.
func (d *Deployer) ServiceTotals(ctx context.Context, app string) (total, running int, memory uint64, err error) {
	return d.opts.Services.Totals(ctx, app)
}

// RemoveService removes a service, with what it holds.
func (d *Deployer) RemoveService(ctx context.Context, app, env, name, by string) error {
	lock := d.envLock(app, env)
	lock.Lock()
	defer lock.Unlock()
	return d.opts.Services.Remove(ctx, app, env, name, by)
}

// ServiceLogs says the newest of what a service has said.
func (d *Deployer) ServiceLogs(ctx context.Context, app, env, name string, tail int) (string, error) {
	return d.opts.Services.Logs(ctx, app, env, name, tail)
}

// ServiceProblems says what is wrong with the services on this server, for the doctor. It asks the engine only if there are any.
func (d *Deployer) ServiceProblems(ctx context.Context) (count int, problems []string) {
	return d.opts.Services.Problems(ctx)
}

// UpdateService takes the newest image of a service's version. It says what it now runs, if that changed.
func (d *Deployer) UpdateService(ctx context.Context, app, env, name string, log io.Writer) (string, error) {
	lock := d.envLock(app, env)
	lock.Lock()
	defer lock.Unlock()
	return d.opts.Services.Update(ctx, app, env, name, log)
}

// UpgradeService moves a service to a newer major version. What can refuse it does so first, with nothing stopped. Then what connects to it stops, as what it wrote after the dump
// would be lost, including what runs by itself, and starts again however it went.
func (d *Deployer) UpgradeService(ctx context.Context, app, env, name, version, image string, log io.Writer) error {
	lock := d.envLock(app, env)
	lock.Lock()
	defer lock.Unlock()
	upgrade, err := d.opts.Services.PrepareUpgrade(ctx, app, env, name, version, image, log)
	if err != nil {
		return err
	}
	key := sleepKey(app, env)
	d.upgrading.Store(key, true)
	defer d.upgrading.Delete(key)
	d.cancelRuns(app, env)
	return d.opts.Runtime.Offline(ctx, app, env, func() error {
		return d.opts.Services.Upgrade(ctx, upgrade, log)
	})
}

// RestoreService puts a service back to its latest dump, with what connects to it stopped.
func (d *Deployer) RestoreService(ctx context.Context, app, env, name string) error {
	lock := d.envLock(app, env)
	lock.Lock()
	defer lock.Unlock()
	// What can refuse it does so with nothing stopped.
	if err := d.opts.Services.CheckRestore(ctx, app, env, name); err != nil {
		return err
	}
	key := sleepKey(app, env)
	d.upgrading.Store(key, true)
	defer d.upgrading.Delete(key)
	d.cancelRuns(app, env)
	return d.opts.Runtime.Offline(ctx, app, env, func() error {
		return d.opts.Services.Restore(ctx, app, env, name)
	})
}

// serviceUsers is how many commands are using an environment's services, whether it was asleep or awake when they began.
func (d *Deployer) serviceUsers(key string) *atomic.Int64 {
	counter, _ := d.usingServices.LoadOrStore(key, &atomic.Int64{})
	return counter.(*atomic.Int64)
}

// holdServicesForRun starts what a command needs, if its environment is asleep, and keeps it running until the command has ended, whatever else happens to the environment. It says
// what to do at the end. It counts itself before it looks, and Sleep marks an environment asleep before it counts, so that one of the two always sees the other. It waits for a wake
// or a rollout only if it has to, and gives up if the command is stopped, as what holds that lock may be waiting for the command to end. That is why stopping them again goes on
// in the background.
func (d *Deployer) holdServicesForRun(ctx context.Context, app, env string, plan manifest.Plan) (func(), error) {
	if len(plan.Services) == 0 || d.opts.Services == nil {
		return func() {}, nil
	}
	users := d.serviceUsers(sleepKey(app, env))
	users.Add(1)
	lock := d.envLock(app, env)
	if d.Asleep(app, env) {
		for !lock.TryLock() {
			select {
			case <-ctx.Done():
				users.Add(-1)
				return nil, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		var err error
		if d.Asleep(app, env) {
			err = d.opts.Services.Wake(ctx, app, env)
		}
		lock.Unlock()
		if err != nil {
			users.Add(-1)
			return nil, err
		}
	}
	return func() {
		if users.Add(-1) != 0 {
			return
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			// A restore or an upgrade can hold this for hours, and stopping the server does not wait for those. What sweeps up after commands sees to this if it never gets its turn.
			for !lock.TryLock() {
				select {
				case <-d.ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
			defer lock.Unlock()
			d.restServices(app, env, plan)
		}()
	}, nil
}

// restServices stops the services of an environment that is asleep, if nothing is using them, with the environment's lock held. A wake or a rollout that was under way has gone first,
// and is then what it finds.
func (d *Deployer) restServices(app, env string, plan manifest.Plan) {
	if d.serviceUsers(sleepKey(app, env)).Load() != 0 || !d.Asleep(app, env) || needsServicesAsleep(plan) {
		return
	}
	if err := d.opts.Services.Sleep(d.ctx, app, env); err != nil {
		d.log.Warn("stop the services of an environment that is asleep", "app", app, "env", env, "err", err)
	}
}

// restLeftServices stops what a server that stopped during a command left running for it, in an environment that is asleep.
func (d *Deployer) restLeftServices(ctx context.Context, app, env string) {
	if !d.opts.Services.AnyRunning(ctx, app, env) {
		return
	}
	lock := d.envLock(app, env)
	if !lock.TryLock() {
		return
	}
	defer lock.Unlock()
	_, b, err := d.db.CurrentRelease(ctx, app, env)
	if err != nil {
		return
	}
	var plan manifest.Plan
	if err := json.Unmarshal(b.Plan, &plan); err != nil {
		return
	}
	d.restServices(app, env, plan)
}

// needsServicesAsleep says that something goes on running while an environment sleeps, and so needs its services.
func needsServicesAsleep(plan manifest.Plan) bool {
	for _, p := range plan.Processes {
		if p.Sleep != nil && !*p.Sleep {
			return true
		}
	}
	return false
}

// BackService puts a service back on what it held before its last upgrade or restore, with what connects to it stopped.
func (d *Deployer) BackService(ctx context.Context, app, env, name string) error {
	lock := d.envLock(app, env)
	lock.Lock()
	defer lock.Unlock()
	if err := d.opts.Services.CheckBack(ctx, app, env, name); err != nil {
		return err
	}
	key := sleepKey(app, env)
	d.upgrading.Store(key, true)
	defer d.upgrading.Delete(key)
	d.cancelRuns(app, env)
	return d.opts.Runtime.Offline(ctx, app, env, func() error {
		return d.opts.Services.Back(ctx, app, env, name)
	})
}

// FinishServiceUpgrade lets go of what a service held before its last upgrade.
func (d *Deployer) FinishServiceUpgrade(ctx context.Context, app, env, name string) error {
	lock := d.envLock(app, env)
	lock.Lock()
	defer lock.Unlock()
	return d.opts.Services.FinishUpgrade(ctx, app, env, name)
}
