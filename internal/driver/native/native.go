// Package native runs app instances as supervised OS processes, each through
// its own shim (docs/05-runtime.md, "Drivers"). Instances outlive the
// server: a new server finds them again from their files in the run
// directory.
package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/shim"
)

// ErrNotFound is returned for an instance the driver has no files for.
var ErrNotFound = errors.New("no such instance")

// Driver starts and controls shims.
type Driver struct {
	dir     string
	shimCmd []string
	shimEnv []string
}

// New returns a driver keeping its files in dir. shimCmd runs `moor shim`
// (the moor binary and "shim"); shimEnv is added to the shims' environment.
func New(dir string, shimCmd, shimEnv []string) *Driver {
	return &Driver{dir: dir, shimCmd: shimCmd, shimEnv: shimEnv}
}

func (d *Driver) specPath(id string) string  { return filepath.Join(d.dir, id+".spec.json") }
func (d *Driver) statePath(id string) string { return filepath.Join(d.dir, id+".state.json") }
func (d *Driver) shimLog(id string) string   { return filepath.Join(d.dir, id+".shim.log") }

// Start writes the instance's spec and starts its shim, returning once the
// shim has started the app (or failed to).
func (d *Driver) Start(ctx context.Context, spec shim.Spec) (shim.State, error) {
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return shim.State{}, err
	}
	var token [16]byte
	rand.Read(token[:])
	spec.Token = hex.EncodeToString(token[:])
	spec.StateFile = d.statePath(spec.ID)
	os.Remove(spec.StateFile)
	if err := shim.WriteSpec(d.specPath(spec.ID), spec); err != nil {
		return shim.State{}, err
	}
	out, err := os.OpenFile(d.shimLog(spec.ID), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return shim.State{}, err
	}
	defer out.Close()
	cmd := shim.Command(d.shimCmd, d.specPath(spec.ID))
	cmd.Env = append(os.Environ(), d.shimEnv...)
	cmd.Dir = spec.Dir
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return shim.State{}, fmt.Errorf("start the shim: %w", err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }() // reap the shim when it exits

	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if st, err := shim.ReadState(spec.StateFile); err == nil && st.Status != shim.Starting {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return shim.State{}, ctx.Err()
		case <-exited:
			if st, err := shim.ReadState(spec.StateFile); err == nil {
				return st, nil
			}
			return shim.State{}, fmt.Errorf("the shim exited at once: %s", d.lastLines(spec.ID))
		case <-deadline.C:
			return shim.State{}, fmt.Errorf("the shim did not report within 15s: %s", d.lastLines(spec.ID))
		case <-tick.C:
		}
	}
}

func (d *Driver) lastLines(id string) string {
	data, _ := os.ReadFile(d.shimLog(id))
	s := strings.TrimSpace(string(data))
	if len(s) > 500 {
		s = s[len(s)-500:]
	}
	if s == "" {
		return "no output"
	}
	return s
}

// Status returns an instance's state and whether its shim is alive. The
// state of a dead shim is the last one it recorded.
func (d *Driver) Status(id string) (shim.State, bool, error) {
	st, err := shim.ReadState(d.statePath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return shim.State{}, false, ErrNotFound
	}
	if err != nil {
		return shim.State{}, false, err
	}
	if st.Final() {
		return st, false, nil
	}
	res, err := shim.Call(st.Addr, shim.Request{Token: d.token(id), Op: "status"}, 3*time.Second)
	if err != nil {
		return st, false, nil
	}
	return res.State, true, nil
}

func (d *Driver) token(id string) string {
	spec, err := shim.ReadSpec(d.specPath(id))
	if err != nil {
		return ""
	}
	return spec.Token
}

// Restart asks the shim to restart its app.
func (d *Driver) Restart(id string) error {
	return d.call(id, "restart", time.Minute)
}

func (d *Driver) call(id, op string, timeout time.Duration) error {
	st, err := shim.ReadState(d.statePath(id))
	if err != nil {
		return err
	}
	_, err = shim.Call(st.Addr, shim.Request{Token: d.token(id), Op: op}, timeout)
	return err
}

// Stop stops an instance: its shim stops the app (signal, then kill after
// the stop timeout) and exits. If the shim is gone, an app it left behind is
// killed. The instance's files are removed.
func (d *Driver) Stop(id string, stopTimeout time.Duration) error {
	st, alive, err := d.Status(id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if alive {
		if _, err := shim.Call(st.Addr, shim.Request{Token: d.token(id), Op: "stop"}, stopTimeout+15*time.Second); err != nil {
			return fmt.Errorf("stop %s: %w", id, err)
		}
	} else {
		d.CleanupOrphan(st)
	}
	d.Remove(id)
	return nil
}

// CleanupOrphan kills the app of a dead shim if it is still running — where
// the operating system does not do it (macOS has no parent-death signal).
// The process is identified by its PID and start time, so a reused PID is
// never killed by mistake.
func (d *Driver) CleanupOrphan(st shim.State) {
	if st.AppPID <= 0 || st.AppStart == 0 {
		return
	}
	if t, err := platform.ProcessStartTime(st.AppPID); err == nil && t == st.AppStart {
		platform.KillGroup(st.AppPID)
	}
}

// Remove deletes an instance's files.
func (d *Driver) Remove(id string) {
	os.Remove(d.specPath(id))
	os.Remove(d.statePath(id))
	os.Remove(d.shimLog(id))
}

// List returns the IDs of the instances the driver has files for.
func (d *Driver) List() []string {
	matches, _ := filepath.Glob(filepath.Join(d.dir, "*.spec.json"))
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, strings.TrimSuffix(filepath.Base(m), ".spec.json"))
	}
	return ids
}

// Tail returns the last n lines of an instance's log.
func Tail(logFile string, n int) []logs.Line {
	lines, _ := logs.Tail(logFile, n)
	return lines
}
