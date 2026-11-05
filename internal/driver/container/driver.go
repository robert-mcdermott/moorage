package container

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/oneoff"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/shim"
)

// ErrNotFound is returned for an instance that has no container.
var ErrNotFound = errors.New("no such container")

// Labels on every container Moorage runs.
const (
	labelManaged  = "io.moorage.managed"
	labelServer   = "io.moorage.server"
	labelApp      = "io.moorage.app"
	labelEnv      = "io.moorage.env"
	labelRelease  = "io.moorage.release"
	labelProcess  = "io.moorage.process"
	labelInstance = "io.moorage.instance"
	labelService  = "io.moorage.service"
)

// Spec describes a container to run.
type Spec struct {
	// Network is what it reaches its environment's services on, if it has any.
	Network               string
	ID, App, Env, Process string
	Index                 int
	Release               string // "v8"
	Image                 string
	// Command replaces what the image runs, entrypoint and all, if set. A
	// simple command runs directly; anything else runs in the image's /bin/sh.
	Command string
	// Vars are the container's variables. Secrets are too, but reach the engine
	// through its own environment, so that they appear on no command line and
	// in no file, and are removed from the container's captured output.
	Vars, Secrets map[string]string
	// HostPort is published on the loopback interface, to ContainerPort.
	HostPort, ContainerPort int
	// DataDir appears at Mount.
	DataDir, Mount string
	User           string
	Memory         int64
	CPU            float64
	PIDs           int
	Restart        string
	StopSignal     string
	StopTimeout    time.Duration
	LogFile        string
	// BackoffMin is the first delay before a restart (a second by default).
	BackoffMin time.Duration
}

// exitKilled is the exit status of a process killed with SIGKILL.
const exitKilled = 137

// How restarts slow down: the delay doubles up to backoffMax, and a container
// that keeps exiting waits crashBackoff.
const (
	backoffMax   = time.Minute
	crashLimit   = 5
	crashWindow  = 10 * time.Minute
	crashBackoff = 5 * time.Minute
)

// Driver runs instances as containers. They outlive the server: a new server
// finds them again by their labels.
type Driver struct {
	engine *Engine
	server string
	log    *slog.Logger

	mu      sync.Mutex
	tracked map[string]*tracked
	// launched is what this process has started and not yet removed. A one-off command has no record of an instance, and should not be taken for something left behind.
	launched map[string]bool
}

// New returns a driver. server identifies this Moorage server among others
// that may use the same engine: each manages only its own containers.
func New(engine *Engine, server string, log *slog.Logger) *Driver {
	return &Driver{engine: engine, server: server, log: log, tracked: map[string]*tracked{}, launched: map[string]bool{}}
}

// Engine returns the driver's engine.
func (d *Driver) Engine() *Engine { return d.engine }

// How long the engine has for what should not take long, however it is doing. A daemon can hang with its socket open, and what is waiting holds up its environment.
const (
	// runLimit can include pulling an image that has been removed since it was built.
	runLimit = 10 * time.Minute
	// stopMargin is on top of what a container has to stop.
	stopMargin   = 30 * time.Second
	removeLimit  = time.Minute
	inspectLimit = 30 * time.Second
)

// tell asks the engine for what has a limit of its own.
func (d *Driver) tell(limit time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	return d.engine.output(ctx, nil, args...)
}

// launch notes something this process is starting.
func (d *Driver) launch(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.launched == nil {
		d.launched = map[string]bool{}
	}
	d.launched[id] = true
}

func (d *Driver) landed(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.launched, id)
}

// Launched says that this process started the container of an instance or a one-off command, which is not something that a previous server left.
func (d *Driver) Launched(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.launched[id]
}

// tracked is a container this server watches.
type tracked struct {
	spec Spec
	name string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{} // ends the wait before a restart

	mu         sync.Mutex
	state      shim.State
	lost       bool
	stopping   bool
	restarting bool

	logMu    sync.Mutex
	logFile  *logs.File
	redact   *strings.Replacer
	lastSec  time.Time
	recorded map[string]int // the lines of lastSec, to tell them from a replay
	resume   time.Time
	replay   map[string]int
}

var nameChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

// containerName is moorage-<app>-<env>-<process>-<n>-<short id>.
func containerName(s Spec) string {
	short := s.ID
	if len(short) > 8 {
		short = short[len(short)-8:]
	}
	name := fmt.Sprintf("moorage-%s-%s-%s-%d-%s", s.App, s.Env, s.Process, s.Index, strings.ToLower(short))
	return nameChars.ReplaceAllString(name, "-")
}

// engineOwned are variables the engine's command line reads for itself: a
// secret can reach a container only through that command's environment, so one
// with these names cannot.
var engineOwned = []string{
	"PATH", "HOME", "TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "BUILDKIT_PROGRESS", "REGISTRY_AUTH_FILE",
	"DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH",
	"CONTAINER_HOST", "CONTAINER_SSHKEY", "CONTAINERS_CONF", "CONTAINERS_STORAGE_CONF",
}

// imageArgs ends a command line: the image, and what to run in it. An image's
// entrypoint would be given a command as its arguments, so a command replaces it.
func imageArgs(s Spec) []string {
	argv := command(s)
	if len(argv) == 0 {
		return []string{s.Image}
	}
	return append([]string{"--entrypoint", argv[0], s.Image}, argv[1:]...)
}

// runArgs are the arguments common to a long-running and a one-off container,
// and the environment that carries the secrets.
func (d *Driver) runArgs(s Spec) (args, env []string, err error) {
	args = []string{"--init", "--security-opt", "no-new-privileges"}
	if s.Network != "" {
		args = append(args, "--network", s.Network)
	}
	for _, k := range sortedKeys(s.Vars) {
		args = append(args, "-e", k+"="+s.Vars[k])
	}
	for _, k := range sortedKeys(s.Secrets) {
		if slices.Contains(engineOwned, k) {
			return nil, nil, fmt.Errorf("the secret %s cannot reach a container: the container engine's command line uses that name for itself. Rename it, or make it an ordinary variable", k)
		}
		args = append(args, "-e", k)
		env = append(env, k+"="+s.Secrets[k])
	}
	if s.DataDir != "" {
		args = append(args, "-v", s.DataDir+":"+s.Mount+volumeOptions())
	}
	if s.User != "" {
		args = append(args, "--user", s.User)
	}
	if s.Memory > 0 {
		args = append(args, "--memory", strconv.FormatInt(s.Memory, 10))
	}
	if s.CPU > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(s.CPU, 'f', -1, 64))
	}
	if s.PIDs > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(s.PIDs))
	}
	return args, env, nil
}

// volumeOptions relabels a mount where SELinux is enforcing, without which the
// container could not use it.
func volumeOptions() string {
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/sys/fs/selinux/enforce"); err == nil {
			return ":Z"
		}
	}
	return ""
}

// plainWords is a command that means the same to any shell: its words can be
// run directly, in an image that has no shell.
var plainWords = regexp.MustCompile(`^[A-Za-z0-9_./:,@%+-][A-Za-z0-9_./:,@%+= -]*$`)

// command returns what runs in the container, if not what the image says to.
// Variables are the container's, so anything but plain words is left to the
// image's shell.
func command(s Spec) []string { return commandLine(s.Command) }

func commandLine(line string) []string {
	s := Spec{Command: line}
	switch {
	case s.Command == "":
		return nil
	case plainWords.MatchString(s.Command) && !strings.Contains(strings.Fields(s.Command)[0], "="):
		return strings.Fields(s.Command)
	}
	return []string{"/bin/sh", "-c", s.Command}
}

func (d *Driver) labels(s Spec) []string {
	var args []string
	for _, l := range [][2]string{
		{labelManaged, "true"}, {labelServer, d.server}, {labelApp, s.App}, {labelEnv, s.Env},
		{labelRelease, s.Release}, {labelProcess, s.Process}, {labelInstance, s.ID},
	} {
		args = append(args, "--label", l[0]+"="+l[1])
	}
	return args
}

func (d *Driver) logOptions() []string {
	if d.engine.Kind == "podman" {
		return []string{"--log-driver", "k8s-file", "--log-opt", "max-size=10m"}
	}
	return []string{"--log-driver", "json-file", "--log-opt", "max-size=10m", "--log-opt", "max-file=3"}
}

// Start runs an instance's container, and returns once it has started.
func (d *Driver) Start(ctx context.Context, s Spec) (shim.State, error) {
	common, env, err := d.runArgs(s)
	if err != nil {
		return shim.State{}, err
	}
	name := containerName(s)
	args := append([]string{"run", "-d", "--name", name, "--restart=no"}, d.labels(s)...)
	args = append(args, d.logOptions()...)
	args = append(args, common...)
	if s.HostPort != 0 {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", s.HostPort, s.ContainerPort))
	}
	if s.StopSignal != "" {
		args = append(args, "--stop-signal", s.StopSignal)
	}
	if s.StopTimeout > 0 {
		args = append(args, "--stop-timeout", strconv.Itoa(int(s.StopTimeout.Seconds())))
	}
	args = append(args, imageArgs(s)...)

	authEnv, cleanup, err := d.engine.withAuth(ctx)
	if err != nil {
		return shim.State{}, err
	}
	defer cleanup()
	d.launch(s.ID)
	running, stopWaiting := context.WithTimeout(ctx, runLimit)
	defer stopWaiting()
	if _, err := d.engine.output(running, append(env, authEnv...), args...); err != nil {
		// Without the container, nothing was started; a failed run can leave
		// one that was created and not started.
		d.tell(removeLimit, "rm", "-f", name)
		d.landed(s.ID)
		return shim.State{}, err
	}
	t, err := d.track(s, name)
	if err != nil {
		d.tell(removeLimit, "rm", "-f", name)
		d.landed(s.ID)
		return shim.State{}, err
	}
	return t.current(), nil
}

// Adopt takes over a container a previous server left. A container that has
// exited is treated as if it had just done so.
func (d *Driver) Adopt(ctx context.Context, s Spec) (shim.State, error) {
	name := containerName(s)
	if _, err := d.inspect(ctx, name); err != nil {
		if IsNotFound(err) {
			return shim.State{}, ErrNotFound
		}
		return shim.State{}, err
	}
	t, err := d.track(s, name)
	if err != nil {
		return shim.State{}, err
	}
	return t.current(), nil
}

// state is the part of `inspect` that the driver reads.
type state struct {
	Status     string    `json:"Status"`
	Running    bool      `json:"Running"`
	ExitCode   int       `json:"ExitCode"`
	OOMKilled  bool      `json:"OOMKilled"`
	Pid        int       `json:"Pid"`
	StartedAt  time.Time `json:"StartedAt"`
	FinishedAt time.Time `json:"FinishedAt"`
}

func (d *Driver) inspect(ctx context.Context, name string) (state, error) {
	out, err := d.engine.output(ctx, nil, "inspect", "--format", "{{json .State}}", name)
	if err != nil {
		return state{}, err
	}
	var st state
	if err := json.Unmarshal(out, &st); err != nil {
		return st, fmt.Errorf("read what the engine says of %s: %w", name, err)
	}
	return st, nil
}

// track starts watching a container.
func (d *Driver) track(s Spec, name string) (*tracked, error) {
	if s.BackoffMin == 0 {
		s.BackoffMin = time.Second
	}
	lf, err := logs.Open(s.LogFile, 10<<20, 5)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &tracked{
		spec: s, name: name, ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), logFile: lf,
		recorded: map[string]int{},
		state:    shim.State{ID: s.ID, Status: shim.Running, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
	}
	t.setRedaction()
	t.loadLog()
	// This holds up whoever started it, which holds its environment, and is not worth it: what it finds out, supervising finds out too.
	asking, stopAsking := context.WithTimeout(ctx, inspectLimit)
	defer stopAsking()
	if st, err := d.inspect(asking, name); err == nil {
		t.state.AppPID = st.Pid
		if !st.StartedAt.IsZero() {
			t.state.StartedAt = st.StartedAt.UTC()
		}
	}
	d.mu.Lock()
	d.tracked[s.ID] = t
	d.mu.Unlock()
	go d.supervise(t)
	return t, nil
}

func (t *tracked) setRedaction() {
	var pairs []string
	for _, v := range t.spec.Secrets {
		if len(v) >= logs.MinRedacted {
			pairs = append(pairs, v, "[secret]")
		}
	}
	if len(pairs) > 0 {
		t.redact = strings.NewReplacer(pairs...)
	}
}

// loadLog remembers where the instance's log has got to, for an adopted
// container: its logs are read again from that second, as the engine's times may
// be no finer than that.
func (t *tracked) loadLog() {
	lines, err := logs.Tail(t.spec.LogFile, 5000)
	if err != nil {
		return
	}
	for _, l := range lines {
		if l.Stream == logs.Moorage {
			continue
		}
		sec := l.Time.UTC().Truncate(time.Second)
		if sec.After(t.lastSec) {
			t.lastSec = sec
			clear(t.recorded)
		}
		if sec.Equal(t.lastSec) {
			t.recorded[l.Stream+l.Text]++
		}
	}
}

func (t *tracked) current() shim.State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *tracked) update(fn func(*shim.State)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fn(&t.state)
	t.state.UpdatedAt = time.Now().UTC()
}

func (t *tracked) note(format string, args ...any) {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	io.WriteString(t.logFile, logs.Format(time.Now(), logs.Moorage, fmt.Sprintf(format, args...)))
}

// line records a line the engine printed: "<time> <text>".
func (t *tracked) line(stream, raw string) {
	at := time.Now()
	text := raw
	if ts, rest, ok := strings.Cut(raw, " "); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			at, text = parsed, rest
		}
	}
	if t.redact != nil {
		text = t.redact.Replace(text)
	}
	key := stream + text
	sec := at.UTC().Truncate(time.Second)
	t.logMu.Lock()
	defer t.logMu.Unlock()
	if !t.resume.IsZero() {
		if sec.Before(t.resume) {
			return
		}
		if sec.Equal(t.resume) && t.replay[key] > 0 {
			t.replay[key]--
			return
		}
	}
	if sec.After(t.lastSec) {
		t.lastSec = sec
		clear(t.recorded)
	}
	if sec.Equal(t.lastSec) {
		t.recorded[key]++
	}
	io.WriteString(t.logFile, logs.Format(at, stream, text))
}

// follow copies the container's output to its log until the container stops or
// ctx ends. Lines already recorded, that the engine prints again, are skipped.
func (d *Driver) follow(ctx context.Context, t *tracked) <-chan struct{} {
	done := make(chan struct{})
	args := []string{"logs", "--follow", "--timestamps"}
	t.logMu.Lock()
	t.resume = t.lastSec
	t.replay = map[string]int{}
	for k, v := range t.recorded {
		t.replay[k] = v
	}
	if !t.lastSec.IsZero() {
		args = append(args, "--since", t.lastSec.Format(time.RFC3339))
	}
	t.logMu.Unlock()
	args = append(args, t.name)
	cmd := exec.CommandContext(ctx, d.engine.Bin, args...)
	stdout, err1 := cmd.StdoutPipe()
	stderr, err2 := cmd.StderrPipe()
	if err1 != nil || err2 != nil || cmd.Start() != nil {
		close(done)
		return done
	}
	var wg sync.WaitGroup
	for _, s := range []struct {
		stream string
		r      io.Reader
	}{{logs.Stdout, stdout}, {logs.Stderr, stderr}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			br := bufio.NewReader(s.r)
			for {
				line, err := br.ReadString('\n')
				if line != "" {
					t.line(s.stream, strings.TrimRight(line, "\r\n"))
				}
				if err != nil {
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		cmd.Wait()
		close(done)
	}()
	return done
}

// supervise watches a container for as long as this server does: it records the
// container's output, and when it exits, says why and restarts it as its
// restart policy says. Moorage owns restarts; the engine's policy is "no".
func (d *Driver) supervise(t *tracked) {
	defer close(t.done)
	defer t.closeLog()
	delay := t.spec.BackoffMin
	var crashes []time.Time
	for {
		logCtx, stopLogs := context.WithCancel(t.ctx)
		logged := d.follow(logCtx, t)
		_, err := d.engine.output(t.ctx, nil, "wait", t.name)
		// What the container wrote before it ended is still to come.
		select {
		case <-logged:
		case <-time.After(2 * time.Second):
		}
		stopLogs()
		<-logged
		if t.ctx.Err() != nil {
			return
		}
		if err != nil {
			if IsNotFound(err) {
				t.mu.Lock()
				t.lost = true
				t.mu.Unlock()
				d.log.Warn("a container is gone", "instance", t.spec.ID, "container", t.name)
				return
			}
			d.log.Warn("waiting for a container", "instance", t.spec.ID, "err", err)
			if !sleep(t.ctx, 2*time.Second) {
				return
			}
			continue
		}
		st, err := d.inspect(t.ctx, t.name)
		if err != nil {
			if t.ctx.Err() != nil {
				return
			}
			if IsNotFound(err) {
				t.mu.Lock()
				t.lost = true
				t.mu.Unlock()
				return
			}
			if !sleep(t.ctx, 2*time.Second) {
				return
			}
			continue
		}
		if st.Running { // restarted by someone; keep watching
			continue
		}
		t.mu.Lock()
		stopping, restarting := t.stopping, t.restarting
		t.restarting = false
		t.mu.Unlock()
		if stopping {
			return
		}
		if !restarting {
			if !d.exited(t, st, &delay, &crashes) {
				return
			}
		}
		if err := d.startAgain(t); err != nil {
			if t.ctx.Err() != nil {
				return
			}
			t.note("the container could not start again: %v", err)
			if !sleep(t.ctx, delay) {
				return
			}
		}
	}
}

// exited records an exit, and waits out the delay before a restart. It reports
// false if the container stays stopped.
func (d *Driver) exited(t *tracked, st state, delay *time.Duration, crashes *[]time.Time) bool {
	at := st.FinishedAt
	if at.IsZero() || at.Year() < 2000 {
		at = time.Now()
	}
	ran := at.Sub(t.current().StartedAt)
	exit := &shim.Exit{Code: st.ExitCode, At: at.UTC()}
	why := fmt.Sprintf("exited with status %d", st.ExitCode)
	switch {
	case st.OOMKilled:
		exit.Signal = "OOM"
		why = fmt.Sprintf("was killed for running out of memory (status %d)", st.ExitCode)
	case st.ExitCode == exitKilled:
		// Not every engine reports running out of memory (Podman 4.9 does not,
		// with an init process), so this may be that too.
		exit.Signal = "KILL"
		why = fmt.Sprintf("was killed (status %d)", st.ExitCode)
		if t.spec.Memory > 0 {
			why += ", perhaps for going over its memory limit"
		}
	}
	policy := t.spec.Restart
	if policy == "never" || (policy == "on-failure" && st.ExitCode == 0) {
		t.note("the container %s", why)
		t.update(func(s *shim.State) { s.Status, s.LastExit, s.AppPID = shim.Exited, exit, 0 })
		return false
	}
	now := time.Now()
	if ran > backoffMax {
		*delay = t.spec.BackoffMin // it ran for a while: this is not a crash loop
	}
	*crashes = slices.DeleteFunc(*crashes, func(c time.Time) bool { return now.Sub(c) > crashWindow })
	*crashes = append(*crashes, now)
	wait, status := *delay, shim.Backoff
	if len(*crashes) >= crashLimit {
		wait, status = crashBackoff, shim.Crashed
	}
	t.note("the container %s; restarting in %s", why, wait)
	t.update(func(s *shim.State) { s.Status, s.LastExit, s.AppPID = status, exit, 0 })
	select {
	case <-t.ctx.Done():
		return false
	case <-t.wake:
	case <-time.After(wait):
	}
	if *delay *= 2; *delay > backoffMax {
		*delay = backoffMax
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.restarting = false
	return !t.stopping
}

// signal ends the wait before a restart, if there is one.
func (t *tracked) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (d *Driver) startAgain(t *tracked) error {
	authEnv, cleanup, err := d.engine.withAuth(t.ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := d.engine.output(t.ctx, authEnv, "start", t.name); err != nil {
		return err
	}
	st, err := d.inspect(t.ctx, t.name)
	if err != nil {
		return err
	}
	t.update(func(s *shim.State) {
		s.Status, s.AppPID, s.StartedAt = shim.Running, st.Pid, st.StartedAt.UTC()
		s.Restarts++
	})
	return nil
}

func (t *tracked) closeLog() {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	t.logFile.Close()
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// Status returns an instance's state, and whether it is alive: running, or
// waiting to be restarted.
func (d *Driver) Status(id string) (shim.State, bool, error) {
	d.mu.Lock()
	t := d.tracked[id]
	d.mu.Unlock()
	if t == nil {
		return shim.State{}, false, ErrNotFound
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lost {
		return t.state, false, ErrNotFound
	}
	return t.state, !t.state.Final(), nil
}

// Restart stops an instance's container and starts it again.
func (d *Driver) Restart(id string) error {
	d.mu.Lock()
	t := d.tracked[id]
	d.mu.Unlock()
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	if t.state.Final() || t.lost { // nothing watches it now; a new rollout replaces it
		t.mu.Unlock()
		return nil
	}
	waiting := t.state.Status != shim.Running
	t.restarting = !waiting
	t.mu.Unlock()
	if waiting {
		t.signal()
		return nil
	}
	_, err := d.tell(t.spec.StopTimeout+stopMargin, "stop", "-t", stopSeconds(t.spec.StopTimeout), t.name)
	return err
}

// stopSeconds is a stop timeout for the engine, in whole seconds.
func stopSeconds(d time.Duration) string {
	return strconv.Itoa(int((d + time.Second - 1) / time.Second))
}

// Stop stops an instance's container and removes it.
func (d *Driver) Stop(id string, stopTimeout time.Duration) error {
	d.mu.Lock()
	t := d.tracked[id]
	d.mu.Unlock()
	name := ""
	if t != nil {
		name = t.name
		t.mu.Lock()
		t.stopping = true
		t.mu.Unlock()
		t.update(func(s *shim.State) { s.Status = shim.Stopping })
		t.signal()
	} else {
		var err error
		if name, err = d.nameOf(id); err != nil {
			return err
		}
		if name == "" {
			return nil
		}
	}
	_, err := d.tell(stopTimeout+stopMargin, "stop", "-t", stopSeconds(stopTimeout), name)
	if err != nil && !IsNotFound(err) {
		d.log.Warn("stopping a container", "instance", id, "err", err)
	}
	if t != nil {
		t.update(func(s *shim.State) { s.Status = shim.Stopped })
		// Its last lines of output arrive as it ends.
		select {
		case <-t.done:
		case <-time.After(3 * time.Second):
			t.cancel()
			<-t.done
		}
	}
	return d.forget(id, name)
}

func (d *Driver) forget(id, name string) error {
	d.mu.Lock()
	t := d.tracked[id]
	delete(d.tracked, id)
	delete(d.launched, id)
	d.mu.Unlock()
	if t != nil {
		t.cancel()
		<-t.done
	}
	_, err := d.tell(removeLimit, "rm", "-f", name)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// listTimeout bounds asking the engine what it has: an engine that does not
// answer must not hold up a server's start.
const listTimeout = 20 * time.Second

// nameOf returns the name of an instance's container, or "".
func (d *Driver) nameOf(id string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	out, err := d.engine.output(ctx, nil, "ps", "-a",
		"--filter", "label="+labelServer+"="+d.server, "--filter", "label="+labelInstance+"="+id, "--format", "{{.Names}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Remove removes an instance's container, if there is one, and forgets it.
func (d *Driver) Remove(id string) {
	d.mu.Lock()
	t := d.tracked[id]
	d.mu.Unlock()
	name := ""
	if t != nil {
		name = t.name
	} else if n, err := d.nameOf(id); err == nil {
		name = n
	}
	if name == "" {
		return
	}
	if err := d.forget(id, name); err != nil {
		d.log.Warn("removing a container", "instance", id, "err", err)
	}
}

// CleanupOrphan has nothing to do: a container has no shim to outlive.
func (d *Driver) CleanupOrphan(shim.State) {}

// Usage is what a container is using.
type Usage struct {
	CPU    float64 // percent of one core
	Memory uint64  // bytes
}

// Stats asks the engine what the containers it runs for us are using, by instance ID.
func (d *Driver) Stats(ctx context.Context) (map[string]Usage, error) {
	args := []string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}"}
	d.mu.Lock()
	byName := make(map[string]string, len(d.tracked))
	for id, t := range d.tracked {
		byName[t.name] = id
		args = append(args, t.name)
	}
	d.mu.Unlock()
	if len(byName) == 0 {
		return nil, nil
	}
	out, err := d.engine.output(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseStats(string(out), byName), nil
}

// parseStats reads what the engine says of its containers, a line each: name, CPU and memory, as
// 0.12% and 12.5MiB / 7.6GiB.
func parseStats(text string, byName map[string]string) map[string]Usage {
	usage := map[string]Usage{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) < 3 {
			continue
		}
		id, ok := byName[f[0]]
		if !ok {
			continue
		}
		cpu, _ := strconv.ParseFloat(strings.TrimSuffix(f[1], "%"), 64)
		used, _, _ := strings.Cut(f[2], "/")
		usage[id] = Usage{CPU: cpu, Memory: parseSize(strings.TrimSpace(used))}
	}
	return usage
}

// parseSize reads a size as engines write them, such as 12.5MiB or 1.2GB.
func parseSize(s string) uint64 {
	for _, u := range []struct {
		suffix string
		factor float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"B", 1}} {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			if err != nil {
				return 0
			}
			return uint64(v * u.factor)
		}
	}
	return 0
}

// List returns the instances that have containers of this server's. Their
// labels are read with inspect, whose output is the same on every engine and
// version; ps's templates are not.
func (d *Driver) List() []string {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	out, err := d.engine.output(ctx, nil, "ps", "-aq", "--filter", "label="+labelServer+"="+d.server)
	if err != nil {
		d.log.Warn("listing containers", "err", err)
		return nil
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	args := append([]string{"inspect", "--format", `{{index .Config.Labels "` + labelInstance + `"}}`}, ids...)
	out, err = d.engine.output(ctx, nil, args...)
	if err != nil && !IsNotFound(err) { // one may have gone since; the rest are listed
		d.log.Warn("listing containers", "err", err)
		return nil
	}
	return strings.Fields(string(out))
}

// RunOnce runs a command in a one-off container of an image, with its
// variables and data directory, and waits for it: a release command. Its output
// goes to stdout and stderr, with the values of secrets removed by the caller.
func (d *Driver) RunOnce(ctx context.Context, s Spec, timeout time.Duration, stdout, stderr io.Writer) error {
	common, env, err := d.runArgs(s)
	if err != nil {
		return err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	name := containerName(s)
	args := append([]string{"run", "--name", name}, d.labels(s)...)
	args = append(args, common...)
	args = append(args, imageArgs(s)...)
	authEnv, cleanup, err := d.engine.withAuth(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	// Not --rm: the engine's command line may stop before the container, and may
	// report success for a container somebody removed. What the engine has on
	// record says how it ended.
	d.launch(s.ID)
	defer d.landed(s.ID)
	defer d.tell(removeLimit, "rm", "-f", name)
	err = d.engine.stream(ctx, append(env, authEnv...), stdout, stderr, args...)
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("it did not finish within %s", timeout)
	}
	if err != nil {
		return err
	}
	st, err := d.inspect(ctx, name)
	switch {
	case IsNotFound(err):
		return errors.New("its container was removed while it ran")
	case err != nil:
		return err
	case st.ExitCode != 0:
		return fmt.Errorf("it exited with status %d", st.ExitCode)
	}
	return nil
}

// RunSession is RunOnce with someone at the other end of it. The engine's command line runs in the session, in a terminal
// if it has one, which is what -t asks of the container's.
func (d *Driver) RunSession(ctx context.Context, s Spec, timeout time.Duration, session *oneoff.Session) error {
	common, env, err := d.runArgs(s)
	if err != nil {
		return err
	}
	name := containerName(s)
	args := append([]string{"run", "--name", name, "--interactive"}, d.labels(s)...)
	if session.TTY {
		args = append(args, "--tty")
	}
	args = append(args, common...)
	args = append(args, imageArgs(s)...)
	authEnv, cleanup, err := d.engine.withAuth(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	d.launch(s.ID)
	defer d.landed(s.ID)
	defer d.tell(removeLimit, "rm", "-f", name)
	cmd := exec.Command(d.engine.Bin, args...)
	cmd.Env = append(append(os.Environ(), env...), authEnv...)
	return oneoff.Attach(ctx, cmd, s.Command, session, timeout, platform.TreeLimits{}, nil)
}

// ExecSession runs a command in a container that is running. The engine runs it through the container's own shell, if the
// command needs one, as what is in an image is not known here.
func (d *Driver) ExecSession(ctx context.Context, id, command string, timeout time.Duration, session *oneoff.Session) error {
	d.mu.Lock()
	t := d.tracked[id]
	d.mu.Unlock()
	if t == nil {
		return fmt.Errorf("the instance %s is not running", id)
	}
	args := []string{"exec", "--interactive"}
	if session.TTY {
		args = append(args, "--tty")
	}
	args = append(append(args, t.name), commandLine(command)...)
	cmd := exec.Command(d.engine.Bin, args...)
	cmd.Env = os.Environ()
	return oneoff.Attach(ctx, cmd, command, session, timeout, platform.TreeLimits{}, nil)
}

// Shutdown stops watching: the containers go on running, for the next server to
// adopt.
func (d *Driver) Shutdown() {
	d.mu.Lock()
	all := make([]*tracked, 0, len(d.tracked))
	for _, t := range d.tracked {
		all = append(all, t)
	}
	d.mu.Unlock()
	for _, t := range all {
		t.cancel()
	}
	for _, t := range all {
		<-t.done
	}
}
