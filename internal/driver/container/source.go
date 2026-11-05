// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// A container engine is optional, and this is the one answer to whether this server can run containers (spec/05-runtime.md, "The engine").
type State string

const (
	// Absent is no program to run.
	Absent State = "absent"
	// NotAnswering is a program with no daemon or machine behind it, or one that has hung.
	NotAnswering State = "not answering"
	// Ready is one that answered.
	Ready State = "ready"
)

const (
	// probeLimit is how long an engine has to answer. A daemon can hang with its socket open, and nothing here should wait for it.
	probeLimit = 3 * time.Second
	// statusKept is how long an answer stands, so that asking often costs nothing.
	statusKept = 30 * time.Second
)

// ErrNotAnswering says that there is an engine, and nothing behind it.
var ErrNotAnswering = errors.New("this server's container engine is not answering")

// Status is how an engine stands.
type Status struct {
	State   State
	Kind    string
	Version string
	Bin     string
	// Reason is what it said, for those who look after the server.
	Reason string
}

// Ready says that containers can run.
func (s Status) Ready() bool { return s.State == Ready }

// Err is what to return from what needs an engine.
func (s Status) Err() error {
	switch s.State {
	case Ready:
		return nil
	case NotAnswering:
		return fmt.Errorf("%w: %s", ErrNotAnswering, s.Advice())
	}
	return ErrNoEngine
}

// Advice says what to do about it.
func (s Status) Advice() string {
	switch s.State {
	case NotAnswering:
		if s.Kind == "podman" {
			return "start it, with podman machine start on macOS or Windows, and see that the server's user may use it"
		}
		return "start it, and see that the server's user may use it"
	case Absent:
		return "install Podman or Docker, or set engine under [containers] in server.toml. Nothing else needs one"
	}
	return ""
}

// Problem says what is wrong, for whoever asked for something that needs an engine.
func (s Status) Problem() string {
	switch s.State {
	case NotAnswering:
		return fmt.Sprintf("this server's container engine, %s, is not answering: %s", s.Kind, s.Advice())
	case Absent:
		return "this server has no container engine: " + s.Advice()
	}
	return ""
}

// ask finds out how the engine at a path stands. The version comes from the program, and it takes the daemon or machine behind it to list what is running.
func ask(ctx context.Context, bin string) Status {
	st := Status{State: NotAnswering, Bin: bin, Kind: "docker"}
	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, probeLimit)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.WaitDelay = waitForDescendants
		return cmd.CombinedOutput()
	}
	out, err := run("--version")
	if err != nil {
		st.Reason = strings.TrimSpace(firstLine(out, err))
		return st
	}
	st.Version = strings.TrimSpace(string(out))
	if strings.Contains(strings.ToLower(st.Version), "podman") {
		st.Kind = "podman"
	}
	out, err = run("ps", "-q")
	if err != nil {
		st.Reason = strings.TrimSpace(firstLine(out, err))
		return st
	}
	st.State = Ready
	return st
}

func firstLine(out []byte, err error) string {
	text, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if text == "" {
		return err.Error()
	}
	return text
}

// Probe says how an engine setting stands now: "auto" or "" prefers one that is ready to the first one found, and otherwise it names a program.
func Probe(ctx context.Context, setting string) Status { return probe(ctx, setting, "") }

func probe(ctx context.Context, setting, prefer string) Status {
	if setting != "" && setting != "auto" {
		bin, err := exec.LookPath(setting)
		if err != nil {
			return Status{State: Absent, Reason: fmt.Sprintf("there is no program %q", setting)}
		}
		return ask(ctx, bin)
	}
	// What already runs under one engine stays with it, whichever is answering.
	if prefer != "" {
		if bin, err := exec.LookPath(prefer); err == nil {
			return ask(ctx, bin)
		}
	}
	found := Status{State: Absent}
	for _, name := range []string{"podman", "docker"} {
		bin, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		st := ask(ctx, bin)
		if st.Ready() {
			return st
		}
		if found.State == Absent {
			found = st
		}
	}
	return found
}

// SourceOptions say how a server uses its engine.
type SourceOptions struct {
	// Setting is engine under [containers].
	Setting string
	// Prefer is the engine that this server last used, if any.
	Prefer string
	// Prepare gives an engine what it needs of the server.
	Prepare func(*Engine)
	// NewDriver makes the driver for an engine.
	NewDriver func(*Engine) *Driver
	// Pin records an engine once it has answered, as what it comes to run stays with it.
	Pin func(bin string)
	// Kept is how long an answer stands, for tests.
	Kept time.Duration
	Log  *slog.Logger
}

// Source is this server's engine, found when something wants it, so that starting one an hour later or installing one needs no restart. Nothing here polls: if
// nothing wants an engine, nothing asks. A nil Source is a server with none.
type Source struct {
	o       SourceOptions
	mu      sync.Mutex
	status  Status
	checked time.Time
	engine  *Engine
	driver  *Driver
	pinned  bool
}

// NewSource makes a Source, and asks nothing yet.
func NewSource(o SourceOptions) *Source {
	if o.Kept == 0 {
		o.Kept = statusKept
	}
	return &Source{o: o, pinned: o.Prefer != "" && (o.Setting == "" || o.Setting == "auto")}
}

// Status says how the engine stands, as of the last half a minute.
func (s *Source) Status(ctx context.Context) Status {
	if s == nil {
		return Status{State: Absent}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.checked.IsZero() && time.Since(s.checked) < s.o.Kept {
		return s.status
	}
	st := probe(ctx, s.o.Setting, s.preferred())
	// One that was asked and could not say because the server is going away says nothing of the engine.
	if ctx.Err() != nil && !st.Ready() && !s.checked.IsZero() {
		return s.status
	}
	if st.Bin != "" && (s.engine == nil || s.engine.Bin != st.Bin || s.engine.Kind != st.Kind) {
		e := &Engine{Bin: st.Bin, Kind: st.Kind}
		if s.o.Prepare != nil {
			s.o.Prepare(e)
		}
		s.engine = e
		s.driver = nil
		if s.o.NewDriver != nil {
			s.driver = s.o.NewDriver(e)
		}
	}
	if st.Ready() && !s.pinned {
		s.pinned = true
		if s.o.Pin != nil {
			s.o.Pin(st.Bin)
		}
	}
	if st.State != s.status.State || st.Bin != s.status.Bin {
		s.report(st)
	}
	s.status, s.checked = st, time.Now()
	return st
}

// preferred is what to ask first: what it was told to prefer, or once it has run something here, what it ran it with.
func (s *Source) preferred() string {
	if s.pinned && s.engine != nil {
		return s.engine.Bin
	}
	return s.o.Prefer
}

func (s *Source) report(st Status) {
	log := s.o.Log
	if log == nil {
		return
	}
	switch st.State {
	case Ready:
		log.Info("apps can run in containers", "engine", st.Kind, "path", st.Bin)
	case NotAnswering:
		log.Warn("the container engine is not answering", "engine", st.Kind, "path", st.Bin, "said", st.Reason)
	default:
		log.Info("no container engine: apps can run natively, not in containers")
	}
}

// Engine is the engine as last found, which may not be answering, and asks nothing. It is nil if there has been none.
func (s *Source) Engine() *Engine {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine
}

// Driver is the driver as last found, and asks nothing. It is nil if there has been no engine.
func (s *Source) Driver() *Driver {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.driver
}
