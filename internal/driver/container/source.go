// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	// Unknown is an engine that has not finished answering for the first time.
	Unknown State = "unknown"
)

const (
	// probeLimit is how long an engine has to answer. A daemon can hang with its socket open, and nothing here should wait for it.
	probeLimit = 3 * time.Second
	// statusKept is how long an answer stands, so that asking often costs nothing.
	statusKept = 30 * time.Second
	// firstAnswerWait is how long what only wants to know waits for the first answer there is, which an engine that answers gives in far less.
	firstAnswerWait = time.Second
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
	// Until it says, its name is all there is to go by.
	if strings.Contains(strings.ToLower(filepath.Base(bin)), "podman") {
		st.Kind = "podman"
	}
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
	st.Kind = "docker"
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
	// Pin records an engine once something has been built, fetched or started under it, as that stays with it.
	Pin func(bin string)
	// Kept is how long an answer stands, for tests.
	Kept time.Duration
	Log  *slog.Logger
}

// Source is this server's engine, found when something wants it, so that starting one an hour later or installing one needs no restart. Nothing here polls: if
// nothing wants an engine, nothing asks. A nil Source is a server with none.
type Source struct {
	o SourceOptions
	// probing is one probe at a time. It is not held with mu, so that what only wants what was last found never waits for an engine that hangs.
	probing    chan struct{}
	refreshing atomic.Bool
	answered   chan struct{}
	answer     sync.Once

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
	return &Source{o: o, probing: make(chan struct{}, 1), answered: make(chan struct{}), pinned: o.Prefer != "" && (o.Setting == "" || o.Setting == "auto")}
}

// Status says how the engine stands, as of the last half a minute, and asks it if that has gone by.
func (s *Source) Status(ctx context.Context) Status {
	if s == nil {
		return Status{State: Absent}
	}
	if st, fresh := s.last(); fresh {
		return st
	}
	select {
	case s.probing <- struct{}{}:
		defer func() { <-s.probing }()
	case <-ctx.Done():
		return s.orUnknown()
	}
	// It may have been asked while this waited.
	if st, fresh := s.last(); fresh {
		return st
	}
	st := probe(ctx, s.o.Setting, s.preferred())
	// One that was asked and could not say because whoever asked has gone says nothing of the engine, and nothing should go by it.
	if ctx.Err() != nil && !st.Ready() {
		if last := s.orUnknown(); last.State != Unknown {
			return last
		}
		return st
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.Bin != "" && (s.engine == nil || s.engine.Bin != st.Bin) {
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
	if st.State != s.status.State || st.Bin != s.status.Bin {
		s.report(st)
	}
	s.status, s.checked = st, time.Now()
	s.answer.Do(func() { close(s.answered) })
	return st
}

// Known says how the engine stood when last asked, without waiting for it, and has it asked again behind the scenes if that was a while ago. This is
// for what only wants to show how things stand.
func (s *Source) Known() Status {
	if s == nil {
		return Status{State: Absent}
	}
	st, fresh := s.last()
	if fresh {
		return st
	}
	if s.refreshing.CompareAndSwap(false, true) {
		go func() {
			defer s.refreshing.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 4*probeLimit)
			defer cancel()
			s.Status(ctx)
		}()
	}
	if st.State == "" {
		select {
		case <-s.answered:
		case <-time.After(firstAnswerWait):
		}
	}
	return s.orUnknown()
}

// last is what was last found, and whether it still stands.
func (s *Source) last() (Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, !s.checked.IsZero() && time.Since(s.checked) < s.o.Kept
}

func (s *Source) orUnknown() Status {
	st, _ := s.last()
	if st.State == "" {
		st.State = Unknown
	}
	return st
}

// Claim gives the engine as last found, for something to be built, fetched or started under it, and from then on this server stays with that engine: an image is in the
// store of the engine that made it, and another would not have it. An engine that only answered is not chosen by that, so that one found by chance holds nothing.
func (s *Source) Claim() *Engine {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	e := s.engine
	pin := e != nil && !s.pinned
	if pin {
		s.pinned = true
	}
	s.mu.Unlock()
	if pin && s.o.Pin != nil {
		s.o.Pin(e.Bin)
	}
	return e
}

// preferred is what to ask first: what it was told to prefer, or once it has run something here, what it ran it with.
func (s *Source) preferred() string {
	s.mu.Lock()
	defer s.mu.Unlock()
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
		// Only what needs one should sound worried, and what does says so itself: an engine nothing here needs is nothing to warn of.
		log.Info("the container engine is not answering: apps can run natively, not in containers", "engine", st.Kind, "path", st.Bin, "said", st.Reason)
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
