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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/ingress"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
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

// Retire stops every instance of an environment, as when it starts serving
// a static site instead.
func (m *Manager) Retire(app, env string) {
	es := m.env(app, env)
	es.rollout.Lock()
	defer es.rollout.Unlock()
	es.pool.Set(nil)
	m.retire(es, es.list(), 0)
}

// start creates an instance of a process and starts its shim.
func (m *Manager) start(ctx context.Context, es *envState, rel store.Release, build store.Build, plan manifest.Plan, name string, proc manifest.Process, index int) (*instance, error) {
	envID, err := m.db.EnvID(ctx, es.app, es.env)
	if err != nil {
		return nil, err
	}
	in := &instance{
		id: ids.New(ids.Instance), process: name, index: index, releaseID: rel.ID, version: rel.Version,
		buildID: build.ID, proc: proc,
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
			Driver: "native", Port: in.port, CreatedAt: time.Now(),
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
	spec, err := m.spec(es, rel, build, plan, in)
	if err != nil {
		return in, err
	}
	st, err := m.driver.Start(ctx, spec)
	if err != nil {
		return in, fmt.Errorf("start the %s process: %w", name, err)
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
func (m *Manager) spec(es *envState, rel store.Release, build store.Build, plan manifest.Plan, in *instance) (shim.Spec, error) {
	dir := filepath.Join(m.opts.DataDir, filepath.FromSlash(build.ArtifactRef), "app")
	dataDir := filepath.Join(m.opts.DataDir, "data", es.app, es.env)
	for _, d := range []string{dataDir, in.tmpDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return shim.Spec{}, err
		}
	}
	host := es.app + "." + m.cfg.Domain
	vars := map[string]string{}
	for _, k := range baseVars() {
		if v, ok := os.LookupEnv(k); ok {
			vars[k] = v
		}
	}
	vars["HOME"] = dataDir
	vars["TMPDIR"], vars["TEMP"], vars["TMP"] = in.tmpDir, in.tmpDir, in.tmpDir
	for k, v := range plan.Env {
		vars[k] = v
	}
	injected := map[string]string{
		"HOST":                "127.0.0.1",
		"MOORAGE_APP":         es.app,
		"MOORAGE_ENV":         es.env,
		"MOORAGE_ENV_KIND":    "production",
		"MOORAGE_RELEASE":     "v" + strconv.Itoa(rel.Version),
		"MOORAGE_BUILD_ID":    build.ID,
		"MOORAGE_URL":         m.cfg.PublicURL(host),
		"MOORAGE_BASE_PATH":   "",
		"MOORAGE_PROCESS":     in.process,
		"MOORAGE_INSTANCE":    strconv.Itoa(in.index),
		"MOORAGE_INSTANCE_ID": in.id,
		"MOORAGE_DATA_DIR":    dataDir,
		"MOORAGE_BIN":         filepath.Join(dir, ".moorage", "bin"),
	}
	if in.port != 0 {
		injected["PORT"] = strconv.Itoa(in.port)
	}
	for k, v := range injected {
		vars[k] = v
	}
	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return shim.Spec{
		ID:          in.id,
		Dir:         dir,
		Command:     in.proc.Command,
		Env:         env,
		Shell:       m.opts.Shell,
		ShellEnv:    m.opts.ShellEnv,
		LogFile:     in.logFile,
		Restart:     in.proc.Restart,
		StopSignal:  in.proc.StopSignal,
		StopTimeout: in.proc.StopTimeout.Std(),
		BackoffMin:  m.opts.BackoffMin,
	}, nil
}

// baseVars are taken from the server's environment: what programs need to
// run at all (instances do not inherit the rest).
func baseVars() []string {
	if runtime.GOOS == "windows" {
		return []string{"PATH", "PATHEXT", "SystemRoot", "SystemDrive", "WINDIR", "ComSpec", "ProgramData", "ProgramFiles", "USERPROFILE", "APPDATA", "LOCALAPPDATA"}
	}
	return []string{"PATH", "LANG", "TZ", "USER", "LOGNAME"}
}

// awaitReady waits until a new instance is healthy: an exposed process
// answers its health check, any other has run for min_uptime.
func (m *Manager) awaitReady(ctx context.Context, es *envState, in *instance) error {
	h := in.proc.Health
	deadline := time.Now().Add(h.Startup.Std())
	t := time.NewTicker(m.opts.Probe)
	defer t.Stop()
	for {
		st, alive, err := m.driver.Status(in.id)
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
		conn.Close()
		return true
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
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			ok := false
			if st, alive, err := m.driver.Status(in.id); err == nil && alive && st.Status == shim.Running {
				ok = in.backend == nil || m.probe(es, in)
			}
			if ok {
				if failures >= h.Threshold {
					m.log.Info("instance healthy again", "app", es.app, "instance", in.id)
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
				if in.backend != nil {
					in.backend.SetHealthy(false)
				}
			case 2 * h.Threshold:
				m.log.Warn("instance still unhealthy; restarting it", "app", es.app, "instance", in.id)
				m.driver.Restart(in.id)
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
	}
	done := make(chan struct{}, len(ins))
	for _, in := range ins {
		go func() {
			defer func() { done <- struct{}{} }()
			if err := m.driver.Stop(in.id, in.proc.StopTimeout.Std()); err != nil {
				m.log.Warn("stopping an instance", "instance", in.id, "err", err)
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
