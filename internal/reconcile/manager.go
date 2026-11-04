// Package reconcile runs process apps: it starts their instances through the
// driver, waits for them to become healthy, switches traffic to them and
// drains the ones they replace, checks their health, replaces instances
// whose shim died, and adopts running instances after the server restarts
// (spec/02-architecture.md, "Desired state and reconciliation").
package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/driver/native"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/ingress"
	"github.com/robert-mcdermott/moorage/internal/isolate"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
	"github.com/robert-mcdermott/moorage/internal/shim"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Options configure a Manager.
type Options struct {
	DataDir string
	// ProxyToken is what shows an environment's instances that a request came through the
	// router, for those that do not trust what else can reach them on this machine.
	ProxyToken func(envID string) string
	// ShimCmd runs `moor shim`; Shell runs `moor sh -c` for commands that
	// are not simple. ShimEnv and ShellEnv are added to their environments.
	ShimCmd  []string
	ShimEnv  []string
	Shell    []string
	ShellEnv []string
	// PortMin and PortMax bound the ports given to instances.
	PortMin, PortMax int
	// Probe is the interval of readiness checks during a rollout; Tick that
	// of the checks for dead instances.
	Probe time.Duration
	Tick  time.Duration
	// BackoffMin, if set, overrides the shims' first restart delay.
	BackoffMin time.Duration
	// Trusted lists the proxies whose forwarded headers apps may see.
	Trusted []netip.Prefix
	// ToolchainBins returns the PATH directories of an installed toolchain.
	ToolchainBins func(name, version string) []string
	// Secrets opens the sealed values of secret variables.
	Secrets Opener
	// Containers runs apps in containers; without it, the server has no
	// container engine.
	Containers *container.Driver
	// Events hears of instances that exit, sicken and recover.
	Events *events.Bus
	// Isolation says how apps are kept from one another.
	Isolation *isolate.Policy
}

// runner is what the manager asks of a driver about an instance it runs.
type runner interface {
	Status(id string) (shim.State, bool, error)
	Restart(id string) error
	Stop(id string, stopTimeout time.Duration) error
	CleanupOrphan(st shim.State)
	Remove(id string)
	List() []string
}

// Opener decrypts the value of a secret variable.
type Opener interface {
	Open(ctx context.Context, appID, scope, name string, sealed []byte) (string, error)
}

// Manager runs the instances of every process app.
type Manager struct {
	db     *store.DB
	cfg    config.Config
	opts   Options
	driver *native.Driver
	log    *slog.Logger
	ctx    context.Context // ends when the manager shuts down
	cancel context.CancelFunc

	mu       sync.Mutex
	envs     map[string]*envState // by "app/env"
	reserved map[int]bool         // ports being handed out
}

type envState struct {
	app, env string
	rollout  sync.Mutex // one rollout at a time
	pool     *ingress.Pool

	mu        sync.Mutex
	instances map[string]*instance
}

type instance struct {
	id        string
	process   string
	index     int
	releaseID string
	version   int
	buildID   string
	port      int
	proc      manifest.Process
	logFile   string
	tmpDir    string
	backend   *ingress.Backend // nil for a process that does not listen
	run       runner           // the driver that runs it
	container bool             // whether that is the container driver

	// softLimit is the memory it may use, to watch where the kernel will not enforce it.
	softLimit int64

	stopHealth context.CancelFunc
}

// New returns a manager. Call Adopt before serving, and Run to keep
// instances alive.
func New(db *store.DB, cfg config.Config, opts Options, log *slog.Logger) *Manager {
	if opts.PortMin == 0 {
		opts.PortMin, opts.PortMax = 20000, 29999
	}
	if opts.Probe == 0 {
		opts.Probe = 100 * time.Millisecond
	}
	if opts.Tick == 0 {
		opts.Tick = 10 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		db: db, cfg: cfg, opts: opts, log: log, ctx: ctx, cancel: cancel,
		driver:   native.New(filepath.Join(opts.DataDir, "run", "shims"), opts.ShimCmd, opts.ShimEnv),
		envs:     map[string]*envState{},
		reserved: map[int]bool{},
	}
}

// Shutdown stops health checks. Instances keep running: a new server adopts
// them.
func (m *Manager) Shutdown() {
	m.cancel()
	if m.opts.Containers != nil {
		m.opts.Containers.Shutdown()
	}
}

// Pool returns the backend pool of an environment (ingress.PoolSource).
func (m *Manager) Pool(app, env string) *ingress.Pool { return m.env(app, env).pool }

func (m *Manager) env(app, env string) *envState {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := app + "/" + env
	es := m.envs[key]
	if es == nil {
		es = &envState{app: app, env: env, instances: map[string]*instance{},
			pool: ingress.NewPool(app, env, m.opts.Trusted, m.log)}
		m.envs[key] = es
	}
	return es
}

func (m *Manager) allEnvs() []*envState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*envState, 0, len(m.envs))
	for _, es := range m.envs {
		out = append(out, es)
	}
	return out
}

func (es *envState) add(in *instance) {
	es.mu.Lock()
	es.instances[in.id] = in
	es.mu.Unlock()
}

func (es *envState) remove(in *instance) {
	es.mu.Lock()
	delete(es.instances, in.id)
	es.mu.Unlock()
}

func (es *envState) list() []*instance {
	es.mu.Lock()
	defer es.mu.Unlock()
	out := make([]*instance, 0, len(es.instances))
	for _, in := range es.instances {
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].process != out[j].process {
			return out[i].process == manifest.Web || (out[j].process != manifest.Web && out[i].process < out[j].process)
		}
		return out[i].index < out[j].index
	})
	return out
}

// servePool points the pool at the web instances among ins.
func (es *envState) servePool(ins []*instance) {
	var backends []*ingress.Backend
	for _, in := range ins {
		if in.backend != nil {
			backends = append(backends, in.backend)
		}
	}
	es.pool.Set(backends)
}

// serve points the pool at the web instances of a release.
func (es *envState) serve(releaseID string) {
	var backends []*ingress.Backend
	for _, in := range es.list() {
		if in.releaseID == releaseID && in.backend != nil {
			backends = append(backends, in.backend)
		}
	}
	es.pool.Set(backends)
}

// readPlan decodes a build's plan with defaults filled in.
func readPlan(b store.Build) (manifest.Plan, error) {
	var plan manifest.Plan
	if err := json.Unmarshal(b.Plan, &plan); err != nil {
		return plan, fmt.Errorf("read the plan of build %s: %w", b.ID, err)
	}
	plan.Defaults()
	return plan, nil
}

// Adopt finds the instances a previous server left running: those whose
// shims are alive are taken over, the rest are recorded as stopped (and an
// app a dead shim left behind is killed). Instances not in the database are
// stopped. Missing instances of current releases are started by Run.
func (m *Manager) Adopt(ctx context.Context) error {
	lives, err := m.db.LiveInstances(ctx)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, li := range lives {
		known[li.ID] = true
		plan, err := readPlan(li.Build)
		if err != nil {
			return err
		}
		in := &instance{
			id: li.ID, process: li.Process, index: li.Index, releaseID: li.ReleaseID, version: li.Version,
			buildID: li.Build.ID, port: li.Port, proc: plan.Processes[li.Process],
			logFile: m.logFile(li.App, li.Env, li.Process, li.Index, li.ID), tmpDir: m.tmpDir(li.ID),
			run: m.driver,
		}
		if kind, err := m.db.EnvKind(ctx, li.App, li.Env); err == nil {
			in.softLimit = m.softLimitOf(plan.ForEnv(li.Env, kind))
		}
		var st shim.State
		if li.Driver == driverContainer {
			in.container = true
			if st, err = m.adoptContainer(ctx, li, plan, in); err != nil {
				if !errors.Is(err, container.ErrNotFound) {
					m.log.Warn("cannot adopt a container", "app", li.App, "instance", li.ID, "err", err)
				}
				m.db.StopInstance(ctx, li.ID)
				continue
			}
			in.run = m.opts.Containers
		} else {
			var alive bool
			st, alive, err = m.driver.Status(li.ID)
			if err != nil || !alive {
				if err == nil {
					m.driver.CleanupOrphan(st)
				}
				m.driver.Remove(li.ID)
				m.db.StopInstance(ctx, li.ID)
				continue
			}
		}
		es := m.env(li.App, li.Env)
		if li.Port != 0 {
			in.backend = ingress.NewBackend(li.ID, fmt.Sprintf("127.0.0.1:%d", li.Port))
		}
		es.add(in)
		m.log.Info("adopted a running instance", "app", li.App, "env", li.Env, "instance", li.ID, "pid", st.AppPID)
	}
	runners := []runner{m.driver}
	if m.opts.Containers != nil {
		runners = append(runners, m.opts.Containers)
	}
	for _, r := range runners {
		for _, id := range r.List() {
			if !known[id] {
				m.log.Warn("stopping an instance unknown to the database", "instance", id)
				r.Stop(id, 10*time.Second)
			}
		}
	}
	// Serve the current release of every adopted environment; instances of
	// any other release are what an interrupted rollout left, and go.
	for _, es := range m.allEnvs() {
		rel, _, err := m.db.CurrentRelease(ctx, es.app, es.env)
		if err != nil {
			continue
		}
		var stale []*instance
		for _, in := range es.list() {
			if in.releaseID != rel.ID {
				stale = append(stale, in)
			} else {
				m.watch(es, in)
			}
		}
		es.serve(rel.ID)
		m.retire(es, stale, 0)
	}
	return nil
}

// Run keeps the instances of current releases running until ctx ends: it
// starts missing instances and replaces those whose shim died.
func (m *Manager) Run(ctx context.Context) {
	m.ensure(ctx)
	m.cleanLogs()
	t := time.NewTicker(m.opts.Tick)
	defer t.Stop()
	clean := time.NewTicker(time.Hour)
	defer clean.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.ensure(ctx)
		case <-clean.C:
			m.cleanLogs()
		}
	}
}

// LogRetention is how long the logs of an instance that has stopped are kept.
const LogRetention = 14 * 24 * time.Hour

// cleanLogs removes the logs of instances that stopped long ago.
func (m *Manager) cleanLogs() {
	running := map[string]bool{}
	for _, es := range m.allEnvs() {
		for _, in := range es.list() {
			running[in.id] = true
		}
	}
	removeOldLogs(m.opts.DataDir, running, LogRetention, m.log)
}

// removeOldLogs removes the log files of instances that are not running and have not been
// written to for a while: every release leaves a file per instance, and they would
// otherwise add up for ever.
func removeOldLogs(dataDir string, running map[string]bool, age time.Duration, log *slog.Logger) {
	dirs, _ := filepath.Glob(filepath.Join(dataDir, "logs", "apps", "*", "*"))
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*.log*"))
		for _, file := range files {
			name := filepath.Base(file)
			if i := strings.Index(name, ".log"); i >= 0 {
				name = name[:i]
			}
			parts := strings.SplitN(name, ".", 3)
			if len(parts) != 3 || running[parts[2]] {
				continue
			}
			info, err := os.Stat(file)
			if err != nil || time.Since(info.ModTime()) < age {
				continue
			}
			if err := os.Remove(file); err != nil {
				log.Warn("remove an old log", "file", file, "err", err)
			}
		}
	}
}

// ensure brings every process app's production environment to its current
// release: missing instances are started and dead ones replaced.
func (m *Manager) ensure(ctx context.Context) {
	routes, err := m.db.Routes(ctx)
	if err != nil {
		m.log.Error("cannot read the current releases", "err", err)
		return
	}
	for _, r := range routes {
		plan, err := readPlan(r.Build)
		if err != nil || plan.Kind != manifest.KindProcess {
			continue
		}
		es := m.env(r.App, r.Env)
		if !es.rollout.TryLock() {
			continue // a rollout is under way
		}
		if err := m.ensureRoute(ctx, es, r, plan); err != nil && ctx.Err() == nil {
			m.log.Error("cannot start an instance", "app", r.App, "env", r.Env, "err", err)
		}
		es.rollout.Unlock()
	}
}

// ensureRoute brings one environment to what it should be running now. It takes the
// environment's rollout lock being held.
func (m *Manager) ensureRoute(ctx context.Context, es *envState, r store.Route, plan manifest.Plan) error {
	rel, _, err := m.db.CurrentRelease(ctx, r.App, r.Env)
	if err != nil {
		return err
	}
	formation, err := m.db.Formation(ctx, r.App, r.Env)
	if err != nil {
		return err
	}
	running := plan.ForEnv(r.Env, r.Kind).WithFormation(formation)
	if r.Asleep {
		running = running.Sleeping()
	}
	return m.ensureEnv(ctx, es, rel, r.Build, running)
}

// Settle brings an environment to what it should be running now, at once, and returns when it
// is: its instances gone if it has gone to sleep, or healthy if it has woken.
func (m *Manager) Settle(ctx context.Context, app, env string) error {
	routes, err := m.db.Routes(ctx)
	if err != nil {
		return err
	}
	for _, r := range routes {
		if r.App != app || r.Env != env {
			continue
		}
		plan, err := readPlan(r.Build)
		if err != nil || plan.Kind != manifest.KindProcess {
			return err
		}
		es := m.env(app, env)
		es.rollout.Lock()
		defer es.rollout.Unlock()
		return m.ensureRoute(ctx, es, r, plan)
	}
	return nil
}

// ensureEnv brings an environment to what its plan says, by starting the instances
// it lacks and stopping those beyond its number. It goes on after an instance that
// cannot start, and returns the first such failure.
func (m *Manager) ensureEnv(ctx context.Context, es *envState, rel store.Release, build store.Build, plan manifest.Plan) error {
	have := map[string]bool{}
	var keep, extra []*instance
	for _, in := range es.list() {
		if in.releaseID != rel.ID {
			continue
		}
		if proc, ok := plan.Processes[in.process]; !ok || in.index > *proc.Instances {
			extra = append(extra, in)
			continue
		}
		st, alive, err := in.run.Status(in.id)
		if err == nil && (alive || st.Final()) {
			have[fmt.Sprintf("%s/%d", in.process, in.index)] = true
			keep = append(keep, in)
			continue
		}
		m.log.Warn("an instance's shim or container is gone; replacing the instance", "app", es.app, "instance", in.id)
		m.emit(es, in, events.InstanceReplaced, nil)
		if err == nil {
			in.run.CleanupOrphan(st)
		}
		m.retire(es, []*instance{in}, 0)
	}
	if len(extra) > 0 {
		// Those going get no new requests, and finish the ones they have.
		es.servePool(keep)
		m.retire(es, extra, plan.Deploy.Drain.Std())
	}
	var failed error
	changed := false
	for _, name := range manifest.SortedProcesses(plan.Processes) {
		proc := plan.Processes[name]
		for i := 1; i <= *proc.Instances; i++ {
			if have[fmt.Sprintf("%s/%d", name, i)] {
				continue
			}
			in, err := m.start(ctx, es, rel, build, plan, name, proc, i)
			if err == nil {
				err = m.awaitReady(ctx, es, in)
			}
			if err != nil {
				if in != nil {
					m.retire(es, []*instance{in}, 0)
				}
				if failed == nil {
					failed = err
				}
				continue
			}
			m.watch(es, in)
			changed = true
		}
	}
	if changed {
		es.serve(rel.ID)
	}
	return failed
}

// emit says what happened to an instance.
func (m *Manager) emit(es *envState, in *instance, kind string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["process"], data["index"], data["instance"] = in.process, in.index, in.id
	m.opts.Events.Publish(m.ctx, store.Event{Type: kind, App: es.app, Env: es.env, Data: data})
}

// logFile is an instance's log: logs/apps/<app>/<env>/<process>.<index>.<id>.log.
func (m *Manager) logFile(app, env, process string, index int, id string) string {
	return filepath.Join(LogDir(m.opts.DataDir, app, env), fmt.Sprintf("%s.%d.%s.log", process, index, id))
}

// Isolation says what is in effect on this server.
func (m *Manager) Isolation() isolate.Status {
	if m.opts.Isolation == nil {
		return isolate.Status{Asked: isolate.None, Level: isolate.None}
	}
	return m.opts.Isolation.Status
}

// softLimitOf is the memory to watch, where the kernel will not enforce the limit itself.
func (m *Manager) softLimitOf(plan manifest.Plan) int64 {
	if plan.Resources == nil || m.Isolation().Enforced() {
		return 0
	}
	return plan.Resources.Memory
}

// OverMemory restarts an instance that has gone over its limit where nothing else would stop it.
func (m *Manager) OverMemory(id string, used, limit int64) {
	m.mu.Lock()
	var found *instance
	var where *envState
	for _, es := range m.envs {
		for _, in := range es.list() {
			if in.id == id {
				found, where = in, es
			}
		}
	}
	m.mu.Unlock()
	if found == nil {
		return
	}
	m.log.Warn("instance over its memory limit; restarting it", "app", where.app, "instance", id, "used", used, "limit", limit)
	m.emit(where, found, events.InstanceExited, map[string]any{"code": -1, "signal": "OOM"})
	if err := found.run.Restart(id); err != nil {
		m.log.Warn("restart an instance", "app", where.app, "instance", id, "err", err)
	}
}

// LogDir is the directory of an environment's app logs.
func LogDir(dataDir, app, env string) string {
	return filepath.Join(dataDir, "logs", "apps", app, env)
}

func (m *Manager) tmpDir(id string) string {
	return filepath.Join(m.opts.DataDir, "tmp", id)
}

// InstanceInfo describes an instance for `moor ps`.
type InstanceInfo struct {
	ID        string
	Process   string
	Index     int
	Version   int
	Port      int
	Status    string
	Healthy   bool
	PID       int
	Restarts  int
	StartedAt time.Time
	LastExit  *shim.Exit
}

// Instances lists an environment's instances.
func (m *Manager) Instances(app, env string) []InstanceInfo {
	es := m.env(app, env)
	var out []InstanceInfo
	for _, in := range es.list() {
		info := InstanceInfo{ID: in.id, Process: in.process, Index: in.index, Version: in.version, Port: in.port}
		st, alive, err := in.run.Status(in.id)
		switch {
		case err != nil:
			info.Status = "unknown"
		case !alive && !st.Final():
			info.Status = "lost"
		default:
			info.Status = st.Status
		}
		info.PID, info.Restarts, info.StartedAt, info.LastExit = st.AppPID, st.Restarts, st.StartedAt, st.LastExit
		if in.backend != nil {
			info.Healthy = in.backend.Healthy() && info.Status == shim.Running
		} else {
			info.Healthy = info.Status == shim.Running
		}
		out = append(out, info)
	}
	return out
}

// MetricsInstances lists what is running, for sampling.
func (m *Manager) MetricsInstances(ctx context.Context) []metrics.Instance {
	routes, err := m.db.Routes(ctx)
	if err != nil {
		return nil
	}
	var out []metrics.Instance
	for _, r := range routes {
		m.mu.Lock()
		es := m.envs[r.App+"/"+r.Env]
		m.mu.Unlock()
		if es == nil {
			continue
		}
		for _, in := range es.list() {
			st, _, err := in.run.Status(in.id)
			if err != nil || st.Status != shim.Running {
				continue
			}
			out = append(out, metrics.Instance{SoftLimit: in.softLimit, EnvID: r.EnvID, ID: in.id, Part: fmt.Sprintf("%s.%d", in.process, in.index), PID: st.AppPID})
		}
	}
	return out
}

// MetricsContainers says what the container engine knows of the instances it runs.
func (m *Manager) MetricsContainers(ctx context.Context) map[string]metrics.Usage {
	if m.opts.Containers == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	stats, err := m.opts.Containers.Stats(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Debug("read what containers are using", "err", err)
		}
		return nil
	}
	out := make(map[string]metrics.Usage, len(stats))
	for id, u := range stats {
		out[id] = metrics.Usage{CPU: u.CPU, Memory: u.Memory}
	}
	return out
}

// removeTmp deletes an instance's temporary directory.
func removeTmp(in *instance) {
	if in.tmpDir != "" {
		os.RemoveAll(in.tmpDir)
	}
}
