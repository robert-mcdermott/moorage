// Package reconcile runs process apps: it starts their instances through the
// driver, waits for them to become healthy, switches traffic to them and
// drains the ones they replace, checks their health, replaces instances
// whose shim died, and adopts running instances after the server restarts
// (spec/02-architecture.md, "Desired state and reconciliation").
package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/driver/native"
	"github.com/robert-mcdermott/moorage/internal/ingress"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/shim"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Options configure a Manager.
type Options struct {
	DataDir string
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
func (m *Manager) Shutdown() { m.cancel() }

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
		st, alive, err := m.driver.Status(li.ID)
		if err != nil || !alive {
			if err == nil {
				m.driver.CleanupOrphan(st)
			}
			m.driver.Remove(li.ID)
			m.db.StopInstance(ctx, li.ID)
			continue
		}
		plan, err := readPlan(li.Build)
		if err != nil {
			return err
		}
		es := m.env(li.App, li.Env)
		in := &instance{
			id: li.ID, process: li.Process, index: li.Index, releaseID: li.ReleaseID, version: li.Version,
			buildID: li.Build.ID, port: li.Port, proc: plan.Processes[li.Process],
			logFile: m.logFile(li.App, li.Env, li.Process, li.Index, li.ID), tmpDir: m.tmpDir(li.ID),
		}
		if li.Port != 0 {
			in.backend = ingress.NewBackend(li.ID, fmt.Sprintf("127.0.0.1:%d", li.Port))
		}
		es.add(in)
		m.log.Info("adopted a running instance", "app", li.App, "env", li.Env, "instance", li.ID, "pid", st.AppPID)
	}
	for _, id := range m.driver.List() {
		if !known[id] {
			m.log.Warn("stopping an instance unknown to the database", "instance", id)
			m.driver.Stop(id, 10*time.Second)
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
	t := time.NewTicker(m.opts.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.ensure(ctx)
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
		if r.Env != names.Production {
			continue
		}
		plan, err := readPlan(r.Build)
		if err != nil || plan.Kind != manifest.KindProcess {
			continue
		}
		es := m.env(r.App, r.Env)
		if !es.rollout.TryLock() {
			continue // a rollout is under way
		}
		rel, _, err := m.db.CurrentRelease(ctx, r.App, r.Env)
		if err == nil {
			m.ensureEnv(ctx, es, rel, r.Build, plan)
		}
		es.rollout.Unlock()
	}
}

func (m *Manager) ensureEnv(ctx context.Context, es *envState, rel store.Release, build store.Build, plan manifest.Plan) {
	have := map[string]bool{}
	for _, in := range es.list() {
		if in.releaseID != rel.ID {
			continue
		}
		st, alive, err := m.driver.Status(in.id)
		if err == nil && (alive || st.Final()) {
			have[fmt.Sprintf("%s/%d", in.process, in.index)] = true
			continue
		}
		m.log.Warn("an instance's shim is gone; replacing the instance", "app", es.app, "instance", in.id)
		if err == nil {
			m.driver.CleanupOrphan(st)
		}
		m.retire(es, []*instance{in}, 0)
	}
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
				m.log.Error("cannot start an instance", "app", es.app, "process", name, "err", err)
				if in != nil {
					m.retire(es, []*instance{in}, 0)
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
}

// logFile is an instance's log: logs/apps/<app>/<env>/<process>.<index>.<id>.log.
func (m *Manager) logFile(app, env, process string, index int, id string) string {
	return filepath.Join(LogDir(m.opts.DataDir, app, env), fmt.Sprintf("%s.%d.%s.log", process, index, id))
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
		st, alive, err := m.driver.Status(in.id)
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

// removeTmp deletes an instance's temporary directory.
func removeTmp(in *instance) {
	if in.tmpDir != "" {
		os.RemoveAll(in.tmpDir)
	}
}
