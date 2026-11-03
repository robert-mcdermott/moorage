package reconcile

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/shim"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// What store.Instance.Driver says.
const (
	driverNative    = "native"
	driverContainer = "container"
)

// containerEnv is the environment of a container: [env], the release's
// variables and Moorage's. Nothing comes from the server's own environment: the
// image has its own PATH and HOME.
func (m *Manager) containerEnv(ctx context.Context, app, env string, rel store.Release, build store.Build, plan manifest.Plan) (vars, secrets map[string]string, err error) {
	vars, secrets = map[string]string{}, map[string]string{}
	for k, v := range plan.Env {
		vars[k] = v
	}
	if err := m.addConfig(ctx, rel.AppID, rel.Config, vars, secrets); err != nil {
		return nil, nil, err
	}
	m.addMoorage(app, env, rel, build, "0.0.0.0", plan.Container.Mount, vars, secrets)
	return vars, secrets, nil
}

// containerData makes an environment's data directory ready to be mounted. The
// user in the image is whoever the image says, which the server cannot know, so
// any user may write here; the directories above keep out the server's other
// users.
func (m *Manager) containerData(app, env string) (string, error) {
	dir := filepath.Join(m.opts.DataDir, "data", app, env)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		return "", err
	}
	return dir, nil
}

// containerSpec describes an instance for the container driver.
func (m *Manager) containerSpec(ctx context.Context, es *envState, rel store.Release, build store.Build, plan manifest.Plan, in *instance) (container.Spec, error) {
	vars, secrets, err := m.containerEnv(ctx, es.app, es.env, rel, build, plan)
	if err != nil {
		return container.Spec{}, err
	}
	dataDir, err := m.containerData(es.app, es.env)
	if err != nil {
		return container.Spec{}, err
	}
	vars["MOORAGE_PROCESS"] = in.process
	vars["MOORAGE_INSTANCE"] = strconv.Itoa(in.index)
	vars["MOORAGE_INSTANCE_ID"] = in.id
	c := plan.Container
	spec := m.baseContainerSpec(es.app, es.env, in, plan, rel.Version)
	spec.Vars, spec.Secrets = vars, secrets
	spec.DataDir, spec.Mount = dataDir, c.Mount
	spec.User, spec.Memory, spec.CPU, spec.PIDs = c.User, c.Memory, c.CPU, c.PIDs
	spec.Command = in.proc.Command
	if in.port != 0 {
		vars["PORT"] = strconv.Itoa(c.Port)
		spec.HostPort, spec.ContainerPort = in.port, c.Port
	}
	return spec, nil
}

// baseContainerSpec is what the driver must know of a container that already
// exists.
func (m *Manager) baseContainerSpec(app, env string, in *instance, plan manifest.Plan, version int) container.Spec {
	return container.Spec{
		ID: in.id, App: app, Env: env, Process: in.process, Index: in.index, Release: "v" + strconv.Itoa(version),
		Image:   plan.Container.Image,
		Restart: in.proc.Restart, StopSignal: in.proc.StopSignal, StopTimeout: in.proc.StopTimeout.Std(),
		LogFile: in.logFile, BackoffMin: m.opts.BackoffMin,
	}
}

// adoptContainer takes over a container a previous server left, with the values
// of its secrets, to go on removing them from what it prints.
func (m *Manager) adoptContainer(ctx context.Context, li store.LiveInstance, plan manifest.Plan, in *instance) (st shim.State, err error) {
	if m.opts.Containers == nil {
		return st, container.ErrNoEngine
	}
	if plan.Container == nil {
		return st, fmt.Errorf("the plan of build %s has no container", li.Build.ID)
	}
	spec := m.baseContainerSpec(li.App, li.Env, in, plan, li.Version)
	spec.Secrets = map[string]string{}
	cfg, err := m.db.ReleaseConfig(ctx, li.App, li.Env, li.Version)
	if err == nil {
		err = m.addConfig(ctx, li.Build.AppID, cfg, map[string]string{}, spec.Secrets)
	}
	if err != nil {
		// It is still better adopted, than stopped for this.
		m.log.Warn("cannot read a container's secrets; they may appear in its log", "app", li.App, "instance", li.ID, "err", err)
	}
	return m.opts.Containers.Adopt(ctx, spec)
}

// runContainerOnce runs a command once in a container of the release's image:
// the release command of a deployment.
func (m *Manager) runContainerOnce(ctx context.Context, app, env string, rel store.Release, build store.Build, plan manifest.Plan, command string, timeout time.Duration, log io.Writer) error {
	if m.opts.Containers == nil {
		return container.ErrNoEngine
	}
	vars, secrets, err := m.containerEnv(ctx, app, env, rel, build, plan)
	if err != nil {
		return err
	}
	dataDir, err := m.containerData(app, env)
	if err != nil {
		return err
	}
	vars["MOORAGE_PROCESS"] = "release"
	c := plan.Container
	values := make([]string, 0, len(secrets))
	for _, v := range secrets {
		values = append(values, v)
	}
	out := logs.NewWriter(log, logs.Stdout).Redact(values...)
	errw := logs.NewWriter(log, logs.Stderr).Redact(values...)
	defer out.Flush()
	defer errw.Flush()
	return m.opts.Containers.RunOnce(ctx, container.Spec{
		ID: ids.New(ids.Instance), App: app, Env: env, Process: "release", Release: "v" + strconv.Itoa(rel.Version),
		Image: c.Image, Command: command, Vars: vars, Secrets: secrets, DataDir: dataDir, Mount: c.Mount,
		User: c.User, Memory: c.Memory, CPU: c.CPU, PIDs: c.PIDs,
	}, timeout, out, errw)
}
