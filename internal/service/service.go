// Package service installs `moor server run` as a system service — a
// systemd unit on Linux, a launchd job on macOS — and drives it
// (spec/02-architecture.md, "Linux: service unit and cgroup layout";
// decision D18). Both are set up so that stopping or restarting the server
// leaves the shims, and so the apps, running: the server adopts them when
// it starts again.
package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrUnsupported is returned where Moorage cannot install a service yet.
var ErrUnsupported = errors.New("installing the server as a service is not available on this system yet")

// Spec describes the service to install.
type Spec struct {
	// Exe is the absolute path of the moor binary the service runs.
	Exe string
	// Config is the absolute path of server.toml.
	Config string
	// DataDir is the server's data directory (for launchd's own log).
	DataDir string
	// System installs a system-wide service (as root); otherwise one for
	// the current user.
	System bool
	// Path is the PATH the server, and so the apps, run with.
	Path string
}

// Validate checks the spec's paths.
func (s Spec) Validate() error {
	for name, p := range map[string]string{"the moor binary": s.Exe, "the configuration": s.Config, "the data directory": s.DataDir} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("%s must be given as an absolute path, not %q", name, p)
		}
	}
	return nil
}

// Status is what the service manager reports.
type Status struct {
	// Installed reports that the service's file exists.
	Installed bool
	// Running reports a running server, with its PID when known.
	Running bool
	PID     int
	// Starting says that the service manager is trying to get it running, as it does for one that keeps failing.
	Starting bool
	// Detail is the service manager's own summary.
	Detail string
}

// Runner runs a command of the service manager and returns its combined
// output; tests replace it.
type Runner func(name string, args ...string) ([]byte, error)

// Run runs commands for real.
func Run(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return out, nil
}

// Manager installs and drives the service.
type Manager interface {
	// File is where the service's definition is written.
	File() string
	// Render returns the service's definition.
	Render(Spec) ([]byte, error)
	// Install writes the definition and registers the service; with start,
	// it also starts it (and starts it at boot or login).
	Install(spec Spec, start bool) error
	// Uninstall stops the service and removes it.
	Uninstall() error
	Start() error
	Stop() error
	Restart() error
	Status() (Status, error)
	// Logs says how to read the server's own log.
	Logs() string
}

// writeFile writes a service definition, creating its directory.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
