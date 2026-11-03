// Package shim supervises one app instance in its own small process
// (`moor shim`), so that apps keep running while the Moorage server
// restarts (spec/02-architecture.md, "Shims"). The shim starts the app,
// writes its output to the instance's log file, restarts it with backoff
// when it exits, records its state in a file, and takes commands — status,
// signal, restart, stop — from the server over a loopback connection
// guarded by a per-shim secret.
package shim

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"
)

// Spec tells a shim what to run. The server writes it to a file readable
// only by its own user, since the environment may hold secrets.
type Spec struct {
	ID string `json:"id"`
	// Dir is the app's working directory.
	Dir string `json:"dir"`
	// Command is the manifest command. A simple command is executed
	// directly; anything else runs in the embedded shell through Shell.
	Command string   `json:"command"`
	Env     []string `json:"env"`
	// StdinEnv says that more of the environment — secrets — comes on the
	// shim's standard input as a JSON array of KEY=value, so that it is
	// never written to disk. The shim also removes those values from the
	// app's captured output.
	StdinEnv bool `json:"stdin_env,omitempty"`
	// Shell is the program and arguments that run a command that is not
	// simple; the command is appended. ShellEnv is added to its environment.
	Shell    []string `json:"shell"`
	ShellEnv []string `json:"shell_env,omitempty"`

	LogFile    string `json:"log_file"`
	LogMaxSize int64  `json:"log_max_size,omitempty"`
	LogKeep    int    `json:"log_keep,omitempty"`
	StateFile  string `json:"state_file"`
	// Token authenticates the server's commands.
	Token string `json:"token"`

	Restart     string        `json:"restart"`
	StopSignal  string        `json:"stop_signal"`
	StopTimeout time.Duration `json:"stop_timeout"`

	BackoffMin   time.Duration `json:"backoff_min,omitempty"`
	BackoffMax   time.Duration `json:"backoff_max,omitempty"`
	CrashLimit   int           `json:"crash_limit,omitempty"`
	CrashWindow  time.Duration `json:"crash_window,omitempty"`
	CrashBackoff time.Duration `json:"crash_backoff,omitempty"`
}

func (s *Spec) defaults() {
	if s.Restart == "" {
		s.Restart = "always"
	}
	if s.StopSignal == "" {
		s.StopSignal = "TERM"
	}
	if s.StopTimeout == 0 {
		s.StopTimeout = 30 * time.Second
	}
	if s.BackoffMin == 0 {
		s.BackoffMin = time.Second
	}
	if s.BackoffMax == 0 {
		s.BackoffMax = time.Minute
	}
	if s.CrashLimit == 0 {
		s.CrashLimit = 5
	}
	if s.CrashWindow == 0 {
		s.CrashWindow = 10 * time.Minute
	}
	if s.CrashBackoff == 0 {
		s.CrashBackoff = 5 * time.Minute
	}
	if s.LogMaxSize == 0 {
		s.LogMaxSize = 10 << 20
	}
	if s.LogKeep == 0 {
		s.LogKeep = 5
	}
}

// Statuses of an instance, as its shim reports them.
const (
	Starting = "starting" // the shim is starting the app
	Running  = "running"
	Backoff  = "backoff" // the app exited and will be restarted
	Crashed  = "crashed" // the app keeps exiting; restarts are slowed down
	Stopping = "stopping"
	Stopped  = "stopped" // stopped on request; the shim has exited
	Exited   = "exited"  // exited and not restarted (restart policy); the shim has exited
)

// State is what a shim reports about its instance. The shim keeps it
// current in Spec.StateFile.
type State struct {
	ID      string `json:"id"`
	ShimPID int    `json:"shim_pid"`
	// Addr is the shim's control address.
	Addr   string `json:"addr"`
	Status string `json:"status"`
	AppPID int    `json:"app_pid,omitempty"`
	// AppStart identifies the app process's start, so a reused PID is not
	// mistaken for it (0 where unsupported).
	AppStart  int64     `json:"app_start,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Restarts  int       `json:"restarts"`
	LastExit  *Exit     `json:"last_exit,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Exit describes how the app last ended.
type Exit struct {
	Code   int       `json:"code"`
	Signal string    `json:"signal,omitempty"`
	At     time.Time `json:"at"`
}

// Final reports whether the shim has finished (and exited).
func (s State) Final() bool { return s.Status == Stopped || s.Status == Exited }

// ReadSpec reads a spec file.
func ReadSpec(path string) (Spec, error) {
	var s Spec
	err := readJSON(path, &s)
	return s, err
}

// WriteSpec writes a spec file readable only by its owner.
func WriteSpec(path string, s Spec) error { return writeJSON(path, s, 0o600) }

// ReadState reads a state file.
func ReadState(path string) (State, error) {
	var s State
	err := readJSON(path, &s)
	return s, err
}

// Windows refuses to replace or open a file in some states that Unix
// allows (a file being renamed over, a file another process holds open), so
// state files are read and written with a few short retries.
const fileRetries = 50

func readJSON(path string, v any) error {
	var err error
	for i := 0; i < fileRetries; i++ {
		var data []byte
		if data, err = os.ReadFile(path); err == nil {
			return json.Unmarshal(data, v)
		}
		if errors.Is(err, fs.ErrNotExist) {
			return err
		}
		time.Sleep(2 * time.Millisecond)
	}
	return err
}

// writeJSON replaces path atomically.
func writeJSON(path string, v any, mode os.FileMode) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	for i := 0; i < fileRetries; i++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return err
}
