package shim

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/shell"
)

// Main is `moor shim --spec FILE`. It returns the shim's exit status.
func Main(args []string) int {
	fs := flag.NewFlagSet("shim", flag.ContinueOnError)
	specPath := fs.String("spec", "", "instance spec file")
	if err := fs.Parse(args); err != nil || *specPath == "" {
		fmt.Fprintln(os.Stderr, "usage: moor shim --spec FILE")
		return 2
	}
	spec, err := ReadSpec(*specPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "moor shim:", err)
		return 1
	}
	spec.defaults()
	var secrets []string
	if spec.StdinEnv {
		var env []string
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&env); err != nil {
			fmt.Fprintln(os.Stderr, "moor shim: read the environment from standard input:", err)
			return 1
		}
		for _, kv := range env {
			if _, v, ok := strings.Cut(kv, "="); ok {
				secrets = append(secrets, v)
			}
		}
		spec.Env = append(spec.Env, env...)
	}
	s, err := newSupervisor(spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "moor shim:", err)
		return 1
	}
	s.secrets = secrets
	return s.run()
}

type request struct {
	Request
	reply chan Response
}

type supervisor struct {
	spec     Spec
	log      *logs.File
	ln       net.Listener
	requests chan request
	done     chan struct{} // closed when the shim is finishing
	handlers sync.WaitGroup

	mu    sync.Mutex
	state State

	// secrets are the values of the environment that came on standard
	// input, removed from the app's captured output.
	secrets []string
}

func newSupervisor(spec Spec) (*supervisor, error) {
	log, err := logs.Open(spec.LogFile, spec.LogMaxSize, spec.LogKeep)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Close()
		return nil, err
	}
	s := &supervisor{
		spec:     spec,
		log:      log,
		ln:       ln,
		requests: make(chan request),
		done:     make(chan struct{}),
		state:    State{ID: spec.ID, ShimPID: os.Getpid(), Addr: ln.Addr().String(), Status: Starting},
	}
	return s, nil
}

func (s *supervisor) current() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// update changes the state and writes it to the state file.
func (s *supervisor) update(change func(*State)) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.state)
	s.state.UpdatedAt = time.Now().UTC()
	if err := writeJSON(s.spec.StateFile, s.state, 0o600); err != nil {
		s.notef("cannot write the state file: %v", err)
	}
	return s.state
}

// notef writes a line from Moorage into the app's log.
func (s *supervisor) notef(format string, args ...any) {
	io.WriteString(s.log, logs.Format(time.Now(), logs.Moorage, fmt.Sprintf(format, args...)))
}

func (s *supervisor) run() int {
	s.update(func(*State) {})
	go s.serve(s.ln)

	// A shim told to terminate stops its app first.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		reply := make(chan Response, 1)
		select {
		case s.requests <- request{Request: Request{Op: "stop"}, reply: reply}:
		case <-s.done:
		}
	}()

	backoff := s.spec.BackoffMin
	var recent []time.Time
	for {
		p, err := s.start()
		if err != nil {
			s.notef("cannot start %q: %v", s.spec.Command, err)
			s.update(func(st *State) {
				st.AppPID, st.AppStart = 0, 0
				st.LastExit = &Exit{Code: 127, At: time.Now().UTC()}
			})
			if !s.restartAfter(127) {
				return s.finish(Exited, nil)
			}
		} else {
			started := time.Now()
			result, reply := s.supervise(p)
			switch result {
			case stopped:
				return s.finish(Stopped, reply)
			case restartRequested:
				backoff, recent = s.spec.BackoffMin, nil
				s.update(func(st *State) { st.Restarts++ })
				continue
			}
			if time.Since(started) >= s.spec.CrashWindow {
				backoff, recent = s.spec.BackoffMin, nil
			}
			if !s.restartAfter(p.exit.Code) {
				return s.finish(Exited, nil)
			}
		}

		now := time.Now()
		recent = append(recent, now)
		for len(recent) > 0 && now.Sub(recent[0]) > s.spec.CrashWindow {
			recent = recent[1:]
		}
		wait, status := backoff, Backoff
		if len(recent) >= s.spec.CrashLimit {
			wait, status = max(backoff, s.spec.CrashBackoff), Crashed
		}
		s.notef("restarting in %s", wait)
		s.update(func(st *State) { st.Status = status })
		switch result, reply := s.pause(wait); result {
		case stopped:
			return s.finish(Stopped, reply)
		case restartRequested:
			backoff, recent = s.spec.BackoffMin, nil
		default:
			backoff = min(backoff*2, s.spec.BackoffMax)
		}
		s.update(func(st *State) { st.Restarts++ })
	}
}

func (s *supervisor) restartAfter(code int) bool {
	switch s.spec.Restart {
	case "never":
		return false
	case "on-failure":
		return code != 0
	}
	return true
}

type outcome int

const (
	exited outcome = iota
	stopped
	restartRequested
)

// supervise waits for the app to exit, answering commands meanwhile.
func (s *supervisor) supervise(p *proc) (outcome, chan Response) {
	for {
		select {
		case <-p.done:
			s.recordExit(p)
			return exited, nil
		case req := <-s.requests:
			switch req.Op {
			case "signal":
				err := p.signal(req.Signal)
				req.reply <- response(s.current(), err)
			case "restart":
				s.notef("restarting on request")
				s.stop(p)
				s.recordExit(p)
				req.reply <- response(s.current(), nil)
				return restartRequested, nil
			case "stop":
				s.update(func(st *State) { st.Status = Stopping })
				s.notef("stopping on request")
				s.stop(p)
				s.recordExit(p)
				return stopped, req.reply
			}
		}
	}
}

// pause waits out a backoff, answering commands meanwhile.
func (s *supervisor) pause(d time.Duration) (outcome, chan Response) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			return exited, nil
		case req := <-s.requests:
			switch req.Op {
			case "signal":
				req.reply <- response(s.current(), errors.New("the app is not running"))
			case "restart":
				req.reply <- response(s.current(), nil)
				return restartRequested, nil
			case "stop":
				return stopped, req.reply
			}
		}
	}
}

func response(st State, err error) Response {
	if err != nil {
		return Response{Error: err.Error(), State: st}
	}
	return Response{OK: true, State: st}
}

// stop asks the app to stop with its stop signal and kills it after the
// stop timeout.
func (s *supervisor) stop(p *proc) {
	if err := p.signal(s.spec.StopSignal); err != nil {
		p.kill()
	}
	select {
	case <-p.done:
	case <-time.After(s.spec.StopTimeout):
		s.notef("did not stop within %s; killing it", s.spec.StopTimeout)
		p.kill()
		<-p.done
	}
}

func (s *supervisor) recordExit(p *proc) {
	p.cleanup()
	e := p.exit
	if e.Signal != "" {
		s.notef("exited (signal %s)", e.Signal)
	} else {
		s.notef("exited with status %d", e.Code)
	}
	s.update(func(st *State) {
		st.LastExit = &e
		st.AppPID, st.AppStart = 0, 0
	})
}

// finish records the final status, answers a pending stop, lets
// connections finish and exits.
func (s *supervisor) finish(status string, reply chan Response) int {
	st := s.update(func(st *State) { st.Status = status })
	close(s.done)
	if reply != nil {
		reply <- Response{OK: true, State: st}
	}
	s.ln.Close()
	waited := make(chan struct{})
	go func() { s.handlers.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
	}
	s.log.Close()
	return 0
}

// proc is a running app process.
type proc struct {
	cmd    *exec.Cmd
	done   chan struct{}
	exit   Exit
	osProc osProc
	out    *logs.Writer
	errw   *logs.Writer
}

// start starts the app: a simple command directly, anything else through
// the embedded shell.
func (s *supervisor) start() (*proc, error) {
	path, argv, env, simple, err := shell.Simple(s.spec.Command, s.spec.Env, s.spec.Dir)
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	if simple {
		cmd = &exec.Cmd{Path: path, Args: argv, Env: env}
	} else {
		if len(s.spec.Shell) == 0 {
			return nil, errors.New("no shell is configured for commands that are not simple")
		}
		args := append(append([]string{}, s.spec.Shell[1:]...), s.spec.Command)
		cmd = exec.Command(s.spec.Shell[0], args...)
		cmd.Env = append(append([]string{}, s.spec.Env...), s.spec.ShellEnv...)
	}
	cmd.Dir = s.spec.Dir
	p := &proc{cmd: cmd, done: make(chan struct{})}
	p.out = logs.NewWriter(s.log, logs.Stdout).Redact(s.secrets...)
	p.errw = logs.NewWriter(s.log, logs.Stderr).Redact(s.secrets...)
	cmd.Stdout, cmd.Stderr = p.out, p.errw
	cmd.WaitDelay = 2 * time.Second // do not wait forever for descendants holding the output open
	if err := startProcess(p); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	startTime, _ := platform.ProcessStartTime(pid)
	s.notef("started (pid %d)", pid)
	s.update(func(st *State) {
		st.Status = Running
		st.AppPID, st.AppStart = pid, startTime
		st.StartedAt = time.Now().UTC()
	})
	go func() {
		err := cmd.Wait()
		p.out.Flush()
		p.errw.Flush()
		p.exit = exitOf(cmd, err)
		close(p.done)
	}()
	return p, nil
}

func exitOf(cmd *exec.Cmd, err error) Exit {
	e := Exit{Code: -1, At: time.Now().UTC()}
	if ps := cmd.ProcessState; ps != nil {
		e.Code = ps.ExitCode()
		e.Signal = signalOf(ps)
	} else if err != nil {
		e.Code = 1
	}
	return e
}

// Command returns the command that starts a shim for the spec in specPath:
// shimCmd is the program and arguments that run `moor shim` (normally the
// moor binary and "shim"). The shim is detached from its parent — its own
// session on Unix, its own hidden console on Windows — so that it outlives
// the server and is not reached by signals meant for the server.
func Command(shimCmd []string, specPath string) *exec.Cmd {
	args := append(append([]string{}, shimCmd[1:]...), "--spec", specPath)
	cmd := exec.Command(shimCmd[0], args...)
	detach(cmd)
	return cmd
}
