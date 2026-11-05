// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"context"
	"encoding/json"
	"io"

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

// UpgradeService moves a service to a newer major version. What connects to it stops for that, as what it wrote after the dump would be lost, and starts again however it went.
func (d *Deployer) UpgradeService(ctx context.Context, app, env, name, version string, log io.Writer) error {
	lock := d.envLock(app, env)
	lock.Lock()
	defer lock.Unlock()
	if d.opts.Runtime == nil {
		return d.opts.Services.Upgrade(ctx, app, env, name, version, log)
	}
	return d.opts.Runtime.Offline(ctx, app, env, func() error {
		return d.opts.Services.Upgrade(ctx, app, env, name, version, log)
	})
}

// FinishServiceUpgrade lets go of what a service held before its last upgrade.
func (d *Deployer) FinishServiceUpgrade(ctx context.Context, app, env, name string) error {
	lock := d.envLock(app, env)
	lock.Lock()
	defer lock.Unlock()
	return d.opts.Services.FinishUpgrade(ctx, app, env, name)
}
