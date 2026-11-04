package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/ingress"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/oneoff"
	"github.com/robert-mcdermott/moorage/internal/shim"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// RolloutError reports a release that did not become healthy, with the
// last lines of the failing instance's output.
type RolloutError struct {
	Message string
	Output  []string
}

func (e *RolloutError) Error() string {
	if len(e.Output) == 0 {
		return e.Message
	}
	return e.Message + "; its last output:\n  " + strings.Join(e.Output, "\n  ")
}

// Rollout runs release rel (of build, following plan) in an environment.
// With the rolling strategy, new instances start beside the old ones, and
// traffic moves to them once every one is healthy; the old ones are then
// drained and stopped. With the recreate strategy, the old ones are stopped
// first. If a new instance fails, the new instances are stopped and the old
// ones keep serving (rolling) — the error says why.
func (m *Manager) Rollout(ctx context.Context, app, env string, rel store.Release, build store.Build, plan manifest.Plan) error {
	plan.Defaults()
	es := m.env(app, env)
	es.rollout.Lock()
	defer es.rollout.Unlock()
	formation, err := m.db.Formation(ctx, app, env)
	if err != nil {
		return err
	}
	plan = plan.WithFormation(formation)

	old := es.list()
	if plan.Deploy.Strategy == "recreate" {
		es.pool.Set(nil)
		m.retire(es, old, plan.Deploy.Drain.Std())
		old = nil
	}
	var fresh []*instance
	for _, name := range manifest.SortedProcesses(plan.Processes) {
		proc := plan.Processes[name]
		for i := 1; i <= *proc.Instances; i++ {
			in, err := m.start(ctx, es, rel, build, plan, name, proc, i)
			if err != nil {
				if in != nil {
					fresh = append(fresh, in)
				}
				m.retire(es, fresh, 0)
				return err
			}
			fresh = append(fresh, in)
		}
	}
	errs := make(chan error, len(fresh))
	for _, in := range fresh {
		go func() { errs <- m.awaitReady(ctx, es, in) }()
	}
	var failed error
	for range fresh {
		if err := <-errs; err != nil && failed == nil {
			failed = err
		}
	}
	if failed != nil {
		m.retire(es, fresh, 0)
		return failed
	}
	for _, in := range fresh {
		m.watch(es, in)
	}
	// Exactly the new instances take the traffic: a restart rolls out the
	// release already running, so its old instances share the release.
	es.servePool(fresh)
	m.retire(es, old, plan.Deploy.Drain.Std())
	return nil
}

// Restart rolls out the current release again: new instances replace the
// running ones without downtime.
func (m *Manager) Restart(ctx context.Context, app, env string) error {
	rel, build, err := m.db.CurrentRelease(ctx, app, env)
	if err != nil {
		return err
	}
	plan, err := readPlan(build)
	if err != nil {
		return err
	}
	if plan.Kind != manifest.KindProcess {
		return fmt.Errorf("%s is a static site; it has no processes to restart", app)
	}
	return m.Rollout(ctx, app, env, rel, build, plan)
}

// ScaleError reports a formation that cannot be: a process the app does not have, or
// a number out of range.
type ScaleError struct{ Msg string }

func (e *ScaleError) Error() string { return e.Msg }

// Scale changes how many instances of the named processes an environment runs, and
// brings it there: it starts the instances it lacks and stops, once their requests
// finish, those beyond the number. The setting stays, over the manifest's
// `instances`, for the releases to come. If an instance cannot start, the environment
// goes back to how it was and the error says why. It returns the formation it runs.
func (m *Manager) Scale(ctx context.Context, app, env string, change map[string]int) (map[string]int, error) {
	es := m.env(app, env)
	es.rollout.Lock()
	defer es.rollout.Unlock()
	rel, build, err := m.db.CurrentRelease(ctx, app, env)
	if err != nil {
		return nil, err
	}
	plan, err := readPlan(build)
	if err != nil {
		return nil, err
	}
	if plan.Kind != manifest.KindProcess {
		return nil, &ScaleError{app + " is a static site; it has no processes to scale"}
	}
	for _, name := range sortedKeys(change) {
		if _, ok := plan.Processes[name]; !ok {
			return nil, &ScaleError{fmt.Sprintf("%s has no process %s: it has %s", app, name, strings.Join(manifest.SortedProcesses(plan.Processes), ", "))}
		}
		if n := change[name]; n < 0 || n > manifest.MaxInstances {
			return nil, &ScaleError{fmt.Sprintf("%s: the number of instances must be between 0 and %d", name, manifest.MaxInstances)}
		}
	}
	before, err := m.db.Formation(ctx, app, env)
	if err != nil {
		return nil, err
	}
	after := map[string]int{}
	for name, n := range before {
		after[name] = n
	}
	for name, n := range change {
		after[name] = n
	}
	if err := m.db.SetFormation(ctx, app, env, after); err != nil {
		return nil, err
	}
	if err := m.ensureEnv(ctx, es, rel, build, plan.WithFormation(after)); err != nil {
		// Whatever the caller does now, the environment goes back to what it was.
		back := context.WithoutCancel(ctx)
		if serr := m.db.SetFormation(back, app, env, before); serr != nil {
			m.log.Error("restore a formation", "app", app, "env", env, "err", serr)
		}
		if berr := m.ensureEnv(back, es, rel, build, plan.WithFormation(before)); berr != nil {
			m.log.Error("restore a formation", "app", app, "env", env, "err", berr)
		}
		return nil, err
	}
	running := map[string]int{}
	for name, proc := range plan.WithFormation(after).Processes {
		running[name] = *proc.Instances
	}
	return running, nil
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Retire stops every instance of an environment, as when it starts serving
// a static site instead.
func (m *Manager) Retire(app, env string) {
	es := m.env(app, env)
	es.rollout.Lock()
	defer es.rollout.Unlock()
	es.pool.Set(nil)
	m.retire(es, es.list(), 0)
}

// Forget lets go of what the manager holds for an app that has been retired: its files
// are moving or going, and the name may come back as another app.
func (m *Manager) Forget(app string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.envs {
		if strings.HasPrefix(key, app+"/") {
			delete(m.envs, key)
		}
	}
}

// start creates an instance of a process and starts its shim.
func (m *Manager) start(ctx context.Context, es *envState, rel store.Release, build store.Build, plan manifest.Plan, name string, proc manifest.Process, index int) (*instance, error) {
	envID, err := m.db.EnvID(ctx, es.app, es.env)
	if err != nil {
		return nil, err
	}
	in := &instance{
		id: ids.New(ids.Instance), process: name, index: index, releaseID: rel.ID, version: rel.Version,
		buildID: build.ID, proc: proc, run: m.driver,
	}
	driver := driverNative
	if plan.Container != nil {
		if m.opts.Containers == nil {
			return nil, container.ErrNoEngine
		}
		in.run, in.container, driver = m.opts.Containers, true, driverContainer
	}
	in.logFile = m.logFile(es.app, es.env, name, index, in.id)
	in.tmpDir = m.tmpDir(in.id)
	for attempt := 0; ; attempt++ {
		if *proc.Expose {
			if in.port, err = m.allocPort(ctx); err != nil {
				return nil, err
			}
		}
		err = m.db.CreateInstance(ctx, store.Instance{
			ID: in.id, EnvID: envID, ReleaseID: rel.ID, Process: name, Index: index,
			Driver: driver, Port: in.port, CreatedAt: time.Now(),
		})
		m.release(in.port)
		if err == nil {
			break
		}
		if attempt == 5 {
			return nil, err
		}
	}
	if in.port != 0 {
		in.backend = ingress.NewBackend(in.id, net.JoinHostPort("127.0.0.1", strconv.Itoa(in.port)))
		in.backend.SetHealthy(false)
	}
	es.add(in)
	var st shim.State
	if in.container {
		spec, err := m.containerSpec(ctx, es, rel, build, plan, in)
		if err != nil {
			return in, err
		}
		st, err = m.opts.Containers.Start(ctx, spec)
		if err != nil {
			return in, fmt.Errorf("start the %s process: %w", name, err)
		}
	} else {
		spec, secretEnv, err := m.spec(ctx, es, rel, build, plan, in)
		if err != nil {
			return in, err
		}
		st, err = m.driver.Start(ctx, spec, secretEnv)
		if err != nil {
			return in, fmt.Errorf("start the %s process: %w", name, err)
		}
	}
	if st.Final() {
		return in, m.failure(in, fmt.Sprintf("the %s process could not start (exit status %d)", name, exitCode(st)))
	}
	return in, nil
}

func exitCode(st shim.State) int {
	if st.LastExit != nil {
		return st.LastExit.Code
	}
	return -1
}

// spec builds what the shim needs to run an instance.
// spec describes an instance for its shim. Secrets are returned apart, as
// KEY=value entries, for the shim's standard input: the spec is written to
// disk.
func (m *Manager) spec(ctx context.Context, es *envState, rel store.Release, build store.Build, plan manifest.Plan, in *instance) (shim.Spec, []string, error) {
	vars, secrets, err := m.appEnv(ctx, es.app, es.env, rel, build, plan, in.tmpDir)
	if err != nil {
		return shim.Spec{}, nil, err
	}
	vars["MOORAGE_PROCESS"] = in.process
	vars["MOORAGE_INSTANCE"] = strconv.Itoa(in.index)
	vars["MOORAGE_INSTANCE_ID"] = in.id
	if in.port != 0 {
		vars["PORT"] = strconv.Itoa(in.port)
	}
	return shim.Spec{
		ID:          in.id,
		Dir:         m.appDir(build),
		Command:     in.proc.Command,
		Env:         oneoff.List(vars),
		Shell:       m.opts.Shell,
		ShellEnv:    m.opts.ShellEnv,
		LogFile:     in.logFile,
		Restart:     in.proc.Restart,
		StopSignal:  in.proc.StopSignal,
		StopTimeout: in.proc.StopTimeout.Std(),
		BackoffMin:  m.opts.BackoffMin,
	}, oneoff.List(secrets), nil
}

// appDir is the directory a build's app runs in.
func (m *Manager) appDir(build store.Build) string {
	return filepath.Join(m.opts.DataDir, filepath.FromSlash(build.ArtifactRef), "app")
}

// appEnv is the environment of an app's commands: the server's base
// variables, the runtime's own, [env], the release's variables, and
// Moorage's (spec/05-runtime.md, "Environment variables"), with the build's
// toolchains and tool directories first on PATH. The values of secret
// variables are returned apart, decrypted.
func (m *Manager) appEnv(ctx context.Context, app, env string, rel store.Release, build store.Build, plan manifest.Plan, tmpDir string) (vars, secrets map[string]string, err error) {
	dir := m.appDir(build)
	dataDir := filepath.Join(m.opts.DataDir, "data", app, env)
	for _, d := range []string{dataDir, tmpDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, nil, err
		}
	}
	vars = oneoff.BaseEnv()
	secrets = map[string]string{}
	vars["HOME"] = dataDir
	vars["TMPDIR"], vars["TEMP"], vars["TMP"] = tmpDir, tmpDir, tmpDir
	for k, v := range plan.RunEnv {
		vars[k] = v
	}
	for k, v := range plan.DirEnv {
		vars[k] = filepath.Join(dir, filepath.FromSlash(v))
	}
	for k, v := range plan.Env {
		vars[k] = v
	}
	if err := m.addConfig(ctx, rel.AppID, rel.Config, vars, secrets); err != nil {
		return nil, nil, err
	}
	var path []string
	for _, t := range plan.Toolchains {
		if t.Version != "system" && m.opts.ToolchainBins != nil {
			path = append(path, m.opts.ToolchainBins(t.Name, t.Version)...)
		}
	}
	for _, p := range plan.Path {
		path = append(path, filepath.Join(dir, filepath.FromSlash(p)))
	}
	vars["PATH"] = oneoff.PrependPath(path, vars["PATH"])
	m.addMoorage(app, env, rel, build, "127.0.0.1", dataDir, vars, secrets)
	vars["MOORAGE_BIN"] = filepath.Join(dir, ".moorage", "bin")
	delete(secrets, "MOORAGE_BIN")
	return vars, secrets, nil
}

// addConfig adds a release's variables: the values of secrets, decrypted, go to
// secrets.
func (m *Manager) addConfig(ctx context.Context, appID string, cfg []store.ConfigVar, vars, secrets map[string]string) error {
	for _, v := range cfg {
		if !v.Secret {
			vars[v.Name] = v.Value
			continue
		}
		if m.opts.Secrets == nil {
			return fmt.Errorf("%s is a secret, and this server cannot decrypt secrets", v.Name)
		}
		value, err := m.opts.Secrets.Open(ctx, appID, v.Scope, v.Name, v.Sealed)
		if err != nil {
			return err
		}
		delete(vars, v.Name)
		secrets[v.Name] = value
	}
	return nil
}

// addMoorage adds the variables Moorage sets, which nothing overrides.
func (m *Manager) addMoorage(app, env string, rel store.Release, build store.Build, host, dataDir string, vars, secrets map[string]string) {
	for k, v := range map[string]string{
		"HOST":              host,
		"MOORAGE_APP":       app,
		"MOORAGE_ENV":       env,
		"MOORAGE_ENV_KIND":  "production",
		"MOORAGE_RELEASE":   "v" + strconv.Itoa(rel.Version),
		"MOORAGE_BUILD_ID":  build.ID,
		"MOORAGE_URL":       m.cfg.PublicURL(app + "." + m.cfg.Domain),
		"MOORAGE_BASE_PATH": "",
		"MOORAGE_DATA_DIR":  dataDir,
	} {
		vars[k] = v
		delete(secrets, k)
	}
}

// RunOnce runs a command of a release once, as the release's processes
// would run (its directory and environment, no port), and waits for it:
// the release command of a deployment. Its output goes to log as log
// lines, with the values of secrets removed.
func (m *Manager) RunOnce(ctx context.Context, app, env string, rel store.Release, build store.Build, plan manifest.Plan, command string, timeout time.Duration, log io.Writer) error {
	if plan.Container != nil {
		return m.runContainerOnce(ctx, app, env, rel, build, plan, command, timeout, log)
	}
	tmp := m.tmpDir("run-" + rel.ID)
	defer os.RemoveAll(tmp)
	vars, secrets, err := m.appEnv(ctx, app, env, rel, build, plan, tmp)
	if err != nil {
		return err
	}
	vars["MOORAGE_PROCESS"] = "release"
	values := make([]string, 0, len(secrets))
	for k, v := range secrets {
		vars[k] = v
		values = append(values, v)
	}
	out := logs.NewWriter(log, logs.Stdout).Redact(values...)
	errw := logs.NewWriter(log, logs.Stderr).Redact(values...)
	defer out.Flush()
	defer errw.Flush()
	return oneoff.Run(ctx, oneoff.Command{
		Command: command, Env: oneoff.List(vars), Dir: m.appDir(build),
		Shell: m.opts.Shell, ShellEnv: m.opts.ShellEnv,
		Stdout: out, Stderr: errw, Timeout: timeout,
	})
}

// awaitReady waits until a new instance is healthy: an exposed process
// answers its health check, any other has run for min_uptime.
func (m *Manager) awaitReady(ctx context.Context, es *envState, in *instance) error {
	h := in.proc.Health
	deadline := time.Now().Add(h.Startup.Std())
	t := time.NewTicker(m.opts.Probe)
	defer t.Stop()
	for {
		st, alive, err := in.run.Status(in.id)
		if err != nil || !alive || st.Status != shim.Running || st.Restarts > 0 || st.LastExit != nil {
			code := exitCode(st)
			return m.failure(in, fmt.Sprintf("the %s process exited while starting (status %d)", in.process, code))
		}
		if in.backend != nil {
			if m.probe(es, in) {
				in.backend.SetHealthy(true)
				return nil
			}
		} else if time.Since(st.StartedAt) >= h.MinUptime.Std() {
			return nil
		}
		if time.Now().After(deadline) {
			check := "its port " + strconv.Itoa(in.port)
			if h.Path != "" {
				check = h.Path
			}
			return m.failure(in, fmt.Sprintf("the %s process did not answer on %s within %s", in.process, check, h.Startup.Std()))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (m *Manager) failure(in *instance, msg string) error {
	lines, _ := logs.Tail(in.logFile, 20)
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l.Stream != logs.Moorage || strings.HasPrefix(l.Text, "exited") || strings.HasPrefix(l.Text, "cannot") {
			out = append(out, l.Text)
		}
	}
	return &RolloutError{Message: msg, Output: out}
}

// probe runs one health check of an exposed instance.
func (m *Manager) probe(es *envState, in *instance) bool {
	h := in.proc.Health
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(in.port))
	if h.Path == "" {
		conn, err := net.DialTimeout("tcp", addr, h.Timeout.Std())
		if err != nil {
			return false
		}
		defer conn.Close()
		return !in.container || answers(conn, h.Timeout.Std())
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+h.Path, nil)
	if err != nil {
		return false
	}
	req.Host = es.app + "." + m.cfg.Domain
	req.Header.Set("User-Agent", "moorage-health")
	client := &http.Client{Timeout: h.Timeout.Std(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 400 {
		return false
	}
	if h.Expect == "" {
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return true
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return strings.Contains(string(body), h.Expect)
}

// answers reports whether something is behind a published port: the engine
// accepts connections to it whatever the container is doing, and closes them at
// once if nothing in the container listens.
func answers(conn net.Conn, timeout time.Duration) bool {
	conn.SetReadDeadline(time.Now().Add(min(timeout, 300*time.Millisecond)))
	_, err := conn.Read(make([]byte, 1))
	return err == nil || errors.Is(err, os.ErrDeadlineExceeded)
}

// watch checks an instance's health until it is retired: after threshold
// failures it gets no traffic, and after as many more it is restarted.
func (m *Manager) watch(es *envState, in *instance) {
	ctx, cancel := context.WithCancel(m.ctx)
	in.stopHealth = cancel
	h := in.proc.Health
	go func() {
		t := time.NewTicker(h.Interval.Std())
		defer t.Stop()
		failures := 0
		restarts := -1
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			ok := false
			st, alive, err := in.run.Status(in.id)
			if err == nil {
				if restarts >= 0 && st.Restarts > restarts && st.LastExit != nil {
					m.emit(es, in, events.InstanceExited, map[string]any{"code": st.LastExit.Code, "signal": st.LastExit.Signal})
				}
				restarts = st.Restarts
			}
			if err == nil && alive && st.Status == shim.Running {
				ok = in.backend == nil || m.probe(es, in)
			}
			if ok {
				if failures >= h.Threshold {
					m.log.Info("instance healthy again", "app", es.app, "instance", in.id)
					m.emit(es, in, events.InstanceHealthy, nil)
				}
				failures = 0
				if in.backend != nil {
					in.backend.SetHealthy(true)
				}
				continue
			}
			failures++
			switch failures {
			case h.Threshold:
				m.log.Warn("instance unhealthy; it gets no traffic", "app", es.app, "instance", in.id)
				m.emit(es, in, events.InstanceUnhealthy, nil)
				if in.backend != nil {
					in.backend.SetHealthy(false)
				}
			case 2 * h.Threshold:
				m.log.Warn("instance still unhealthy; restarting it", "app", es.app, "instance", in.id)
				in.run.Restart(in.id)
				failures = h.Threshold
			}
		}
	}()
}

// retire stops instances: they leave the pool's rotation (the caller has
// already switched it), finish their requests in flight for up to drain,
// and are stopped.
func (m *Manager) retire(es *envState, ins []*instance, drain time.Duration) {
	if len(ins) == 0 {
		return
	}
	for _, in := range ins {
		if in.stopHealth != nil {
			in.stopHealth()
		}
	}
	if drain > 0 {
		// Requests that picked an instance just before the switch get a
		// moment to be counted as in flight.
		time.Sleep(100 * time.Millisecond)
	}
	deadline := time.Now().Add(drain)
	for _, in := range ins {
		for in.backend != nil && in.backend.InFlight() > 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if in.backend != nil && in.backend.InFlight() > 0 {
			m.log.Warn("the drain ended with requests still in flight", "app", es.app, "env", es.env,
				"instance", in.id, "requests", in.backend.InFlight(), "drain", drain)
		}
	}
	done := make(chan struct{}, len(ins))
	for _, in := range ins {
		go func() {
			defer func() { done <- struct{}{} }()
			start := time.Now()
			if err := in.run.Stop(in.id, in.proc.StopTimeout.Std()); err != nil {
				m.log.Warn("stopping an instance", "instance", in.id, "err", err)
			}
			if took := time.Since(start); took > 5*time.Second {
				m.log.Warn("an instance was slow to stop", "app", es.app, "env", es.env, "instance", in.id,
					"took", took.Round(100*time.Millisecond), "stop_timeout", in.proc.StopTimeout.Std())
			}
			m.db.StopInstance(context.Background(), in.id)
			removeTmp(in)
			es.remove(in)
		}()
	}
	for range ins {
		<-done
	}
}

// allocPort picks a free port at random in the configured range, skipping
// ports reserved by live instances and checking it can be bound.
func (m *Manager) allocPort(ctx context.Context) (int, error) {
	live, err := m.db.LivePorts(ctx)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	span := m.opts.PortMax - m.opts.PortMin + 1
	for i := 0; i < 200; i++ {
		port := m.opts.PortMin + randIntN(span)
		if live[port] || m.reserved[port] {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		ln.Close()
		m.reserved[port] = true
		return port, nil
	}
	return 0, errors.New("no free port in the instance port range")
}

func (m *Manager) release(port int) {
	if port == 0 {
		return
	}
	m.mu.Lock()
	delete(m.reserved, port)
	m.mu.Unlock()
}
