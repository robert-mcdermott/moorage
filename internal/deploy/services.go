// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"

	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/services"
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
			for name := range plan.Services {
				named[name] = true
			}
		}
	}
	rows := make([]ServiceRow, 0, len(list))
	for _, st := range list {
		rows = append(rows, ServiceRow{Status: st, Unused: !named[st.Name]})
	}
	return rows, nil
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
	key := sleepKey(app, env)
	d.upgrading.Store(key, true)
	defer d.upgrading.Delete(key)
	d.cancelRuns(app, env)
	return d.opts.Runtime.Offline(ctx, app, env, func() error {
		return d.opts.Services.Restore(ctx, app, env, name)
	})
}

// usingServices says how many commands of an asleep environment are using its services, which started for them.
func (d *Deployer) serviceUsers(key string) *atomic.Int64 {
	counter, _ := d.runningAsleep.LoadOrStore(key, &atomic.Int64{})
	return counter.(*atomic.Int64)
}

// startServicesForRun starts what a command needs, if its environment is asleep, and says what to do when the command has ended.
func (d *Deployer) startServicesForRun(ctx context.Context, app, env string) (func(), error) {
	if !d.Asleep(app, env) {
		return func() {}, nil
	}
	key := sleepKey(app, env)
	users := d.serviceUsers(key)
	users.Add(1)
	if err := d.opts.Services.Wake(ctx, app, env); err != nil {
		users.Add(-1)
		return nil, err
	}
	return func() {
		// The last one out, if it has not woken for anything else in the meantime.
		if users.Add(-1) == 0 && d.Asleep(app, env) {
			if err := d.opts.Services.Sleep(context.WithoutCancel(ctx), app, env); err != nil {
				d.log.Warn("stop the services of an environment that is asleep", "app", app, "env", env, "err", err)
			}
		}
	}, nil
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

// FinishServiceUpgrade lets go of what a service held before its last upgrade.
func (d *Deployer) FinishServiceUpgrade(ctx context.Context, app, env, name string) error {
	lock := d.envLock(app, env)
	lock.Lock()
	defer lock.Unlock()
	return d.opts.Services.FinishUpgrade(ctx, app, env, name)
}
