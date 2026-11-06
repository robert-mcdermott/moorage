// SPDX-License-Identifier: Apache-2.0

package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ServiceSpec describes the container of a service: something that an app declares, which outlasts its releases. It carries a label of its own and not an instance's, so
// that what removes unknown instances at start leaves it alone (spec/13-services.md).
type ServiceSpec struct {
	// ID is the service's, and Name its container's, which does not depend on the app's name, so that renaming an app changes nothing.
	ID, Name, App, Env, Service string
	Image                       string
	// Network is the one that a container app of its environment reaches it on, by Name.
	Network string
	// Volumes are mounted where these say, by name.
	Volumes map[string]string
	// HostPort is published on the loopback address, for an app that does not run in a container.
	HostPort, ContainerPort int
	Vars                    map[string]string
	// Secrets go through the environment of the engine's command, and so not on a command line.
	Secrets map[string]string
	Memory  int64
	// StopSignal and StopTimeout are how it stops without losing anything.
	StopSignal  string
	StopTimeout time.Duration
	// Command replaces what the image runs, if set.
	Command []string
	// Private says that no app is to reach it yet: it has no port or network, and what looks after services does not take it for running.
	Private bool
}

// serviceLimit is how long the engine has for what should not take long, and pulling an image is not one of those.
const serviceLimit = 30 * time.Second

// ServiceName is the container of a service.
func ServiceName(id string) string { return "moorage-svc-" + idPart(id) }

// ServiceVolume is where a service keeps what it holds. A named volume, as a database belongs inside the engine's VM on macOS and Windows.
func ServiceVolume(id string) string { return "moorage-svc-" + idPart(id) + "-data" }

// ServiceMetadataVolume is where a service keeps what goes with its files, if it keeps it apart.
func ServiceMetadataVolume(id string) string { return "moorage-svc-" + idPart(id) + "-meta" }

// ServiceUpgradeVolume is where a service keeps what it holds after moving to a major version. No volume's name starts with another's, as Podman takes a name for anything that it starts.
func ServiceUpgradeVolume(id, version string) string {
	return "moorage-svc-" + idPart(id) + "-pg" + version
}

// ServiceRestoredVolume is where a service goes when it is put back to a dump, while what it held is kept.
func ServiceRestoredVolume(id string, at int64) string {
	return "moorage-svc-" + idPart(id) + "-restored-" + strconv.FormatInt(at, 10)
}

// ServiceNetwork is the network of an environment's services and of its apps in containers.
func ServiceNetwork(envID string) string { return "moorage-env-" + idPart(envID) }

// idPart makes an ID fit to name something with, which the engines want in lower case.
func idPart(id string) string {
	_, part, ok := strings.Cut(id, "_")
	if !ok {
		part = id
	}
	return strings.ToLower(part)
}

// RunService starts a service's container, replacing any that is there. The data is in the volume, so that nothing is lost by starting afresh, and what it
// was started with is always what is on record.
func (d *Driver) RunService(ctx context.Context, s ServiceSpec) error {
	for _, name := range sortedKeys(s.Volumes) {
		if err := d.ensureVolume(ctx, name, s); err != nil {
			return err
		}
	}
	if s.Network != "" {
		if err := d.EnsureNetwork(ctx, s.Network); err != nil {
			return err
		}
	}
	if _, err := d.tell(removeLimit, "rm", "-f", s.Name); err != nil && !IsNotFound(err) {
		return err
	}
	args := []string{"run", "-d", "--name", s.Name, "--restart=no", "--security-opt", "no-new-privileges"}
	args = append(args, d.serviceLabels(s)...)
	for _, name := range sortedKeys(s.Volumes) {
		args = append(args, "-v", name+":"+s.Volumes[name]+volumeOptions())
	}
	if s.Network != "" {
		args = append(args, "--network", s.Network)
	}
	if s.HostPort != 0 {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", s.HostPort, s.ContainerPort))
	}
	if s.Memory > 0 {
		args = append(args, "--memory", strconv.FormatInt(s.Memory, 10))
	}
	if s.StopSignal != "" {
		args = append(args, "--stop-signal", s.StopSignal)
	}
	if s.StopTimeout > 0 {
		args = append(args, "--stop-timeout", stopSeconds(s.StopTimeout))
	}
	for _, k := range sortedKeys(s.Vars) {
		args = append(args, "-e", k+"="+s.Vars[k])
	}
	var env []string
	for _, k := range sortedKeys(s.Secrets) {
		args = append(args, "-e", k)
		env = append(env, k+"="+s.Secrets[k])
	}
	args = append(args, s.Image)
	args = append(args, s.Command...)
	authEnv, cleanup, err := d.engine.withAuth(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	running, stopWaiting := context.WithTimeout(ctx, runLimit)
	defer stopWaiting()
	if _, err := d.engine.output(running, append(env, authEnv...), args...); err != nil {
		d.tell(removeLimit, "rm", "-f", s.Name)
		return err
	}
	return nil
}

func (d *Driver) serviceLabels(s ServiceSpec) []string {
	var args []string
	for _, l := range [][2]string{{labelManaged, "true"}, {labelServer, d.server}, {labelService, s.ID}, {labelApp, s.App}, {labelEnv, s.Env}} {
		args = append(args, "--label", l[0]+"="+l[1])
	}
	if s.Private {
		args = append(args, "--label", labelPrivate+"=true")
	}
	return args
}

func (d *Driver) ensureVolume(ctx context.Context, name string, s ServiceSpec) error {
	there, err := d.VolumeThere(ctx, name)
	if err != nil || there {
		return err
	}
	asking, cancel := context.WithTimeout(ctx, serviceLimit)
	defer cancel()
	args := []string{"volume", "create"}
	for _, l := range [][2]string{{labelManaged, "true"}, {labelServer, d.server}, {labelService, s.ID}} {
		args = append(args, "--label", l[0]+"="+l[1])
	}
	_, err = d.engine.output(asking, nil, append(args, name)...)
	return err
}

// VolumeThere says whether there is a volume of exactly that name. Only the engine saying that there is none counts: one that is having trouble says nothing of what it holds.
// Podman takes a name for anything that it starts, so that what answers has to be asked what it is called.
func (d *Driver) VolumeThere(ctx context.Context, name string) (bool, error) {
	asking, cancel := context.WithTimeout(ctx, serviceLimit)
	defer cancel()
	out, err := d.engine.output(asking, nil, "volume", "inspect", "--format", "{{.Name}}", name)
	switch {
	case err == nil:
		return strings.TrimSpace(string(out)) == name, nil
	case IsNotFound(err):
		return false, nil
	}
	return false, err
}

// EnsureNetwork makes a network, if there is none of that name.
func (d *Driver) EnsureNetwork(ctx context.Context, name string) error {
	asking, cancel := context.WithTimeout(ctx, serviceLimit)
	defer cancel()
	_, err := d.engine.output(asking, nil, "network", "inspect", name)
	if err == nil {
		return nil
	}
	if !IsNotFound(err) {
		return err
	}
	_, err = d.engine.output(asking, nil, "network", "create", "--label", labelManaged+"=true", "--label", labelServer+"="+d.server, name)
	return err
}

// ServiceRunning says whether a service's container is running, where an app can reach it. One that is private is on its way to something else, and may have been left by a server that
// stopped halfway.
func (d *Driver) ServiceRunning(ctx context.Context, name string) (bool, error) {
	asking, cancel := context.WithTimeout(ctx, serviceLimit)
	defer cancel()
	out, err := d.engine.output(asking, nil, "container", "inspect", "--format", `{{.State.Running}} {{index .Config.Labels "`+labelPrivate+`"}}`, name)
	if IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fields := strings.Fields(string(out))
	return len(fields) == 1 && fields[0] == "true", nil
}

// ExecService runs a command in a service's container, and says what it wrote and whether it worked.
func (d *Driver) ExecService(ctx context.Context, name string, env map[string]string, args ...string) (string, error) {
	command, secrets := execArgs(name, env, false, args)
	out, err := d.engine.output(ctx, secrets, command...)
	return string(out), err
}

// execArgs gives a command the values of env by way of the engine command's own environment, and so not on a command line.
func execArgs(name string, env map[string]string, interactive bool, args []string) (command, secrets []string) {
	command = []string{"exec"}
	if interactive {
		command = append(command, "-i")
	}
	for _, k := range sortedKeys(env) {
		command = append(command, "-e", k)
		secrets = append(secrets, k+"="+env[k])
	}
	return append(append(command, name), args...), secrets
}

// ExecServiceTo runs a command in a service's container, with what it writes going to w.
func (d *Driver) ExecServiceTo(ctx context.Context, name string, env map[string]string, w io.Writer, args ...string) error {
	command, secrets := execArgs(name, env, false, args)
	var said bytes.Buffer
	if err := d.engine.stream(ctx, secrets, w, &said, command...); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(said.String()))
	}
	return nil
}

// ExecServiceFrom runs a command in a service's container, reading r.
func (d *Driver) ExecServiceFrom(ctx context.Context, name string, env map[string]string, r io.Reader, args ...string) error {
	command, secrets := execArgs(name, env, true, args)
	cmd := exec.CommandContext(ctx, d.engine.Bin, command...)
	cmd.Env = append(os.Environ(), secrets...)
	cmd.Stdin = r
	cmd.WaitDelay = waitForDescendants
	said, err := cmd.CombinedOutput()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(said)))
	}
	return nil
}

// StopService stops a service's container and removes it. What it holds stays in its volume.
func (d *Driver) StopService(name string, timeout time.Duration) error {
	_, err := d.tell(timeout+stopMargin, "stop", "-t", stopSeconds(timeout), name)
	if err != nil && !IsNotFound(err) {
		return err
	}
	if _, err := d.tell(removeLimit, "rm", "-f", name); err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// RemoveVolume removes what a service holds. It looks first, as Podman would otherwise take this name for another that starts with it, and remove what should be kept.
func (d *Driver) RemoveVolume(name string) error {
	there, err := d.VolumeThere(context.Background(), name)
	if err != nil || !there {
		return err
	}
	_, err = d.tell(removeLimit, "volume", "rm", "-f", name)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// RemoveNetwork removes a network, if nothing is using it.
func (d *Driver) RemoveNetwork(name string) error {
	_, err := d.tell(removeLimit, "network", "rm", name)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// ServiceLogs writes the newest of what a service has said.
func (d *Driver) ServiceLogs(ctx context.Context, name string, tail int) (string, error) {
	asking, cancel := context.WithTimeout(ctx, serviceLimit)
	defer cancel()
	// A container writes to both, and the engine keeps them apart on the way out, which is more than anyone asking needs.
	return d.engine.combined(asking, "logs", "--tail", strconv.Itoa(tail), name)
}
