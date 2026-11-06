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
	"sync/atomic"
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
	"slices"
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
	// EnsureToolchain fetches a toolchain that an app needs and this server does not have.
	EnsureToolchain func(ctx context.Context, name, version string) error
	// Secrets opens the sealed values of secret variables.
	Secrets Opener
	// Containers runs apps in containers; without it, the server has no
	// container engine.
	Containers *container.Source
	// Events hears of instances that exit, sicken and recover.
	Events *events.Bus
	// Services connects an app to what it declares under [services].
	Services Services
	// Isolation says how apps are kept from one another.
	Isolation *isolate.Policy
}

// Services gives the addresses of an environment's services, for an instance as it starts.
type Services interface {
	Addresses(ctx context.Context, app, env string, plan manifest.Plan, inContainer bool) (secret, plain map[string]string, network string, err error)
	Any(ctx context.Context) bool
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
	// pending is what a previous server left in containers, until the engine can be asked about it.
	pendingMu sync.Mutex
	pending   []store.LiveInstance
	// adopting is whether that is being looked at, which does not hold up anything else.
	adopting atomic.Bool
	// engineDown is what was last said of the engine.
	engineDown bool
	db         *store.DB
	cfg        config.Config
	opts       Options
	driver     *native.Driver
	log        *slog.Logger
	ctx        context.Context // ends when the manager shuts down
	cancel     context.CancelFunc

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
	// leaving is one that should have stopped, and could not be got to: the engine is asked again once it answers.
	leaving   atomic.Bool
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
	// memoryLimit is the memory that it may use, however that is held to.
	memoryLimit int64
	// sharesUser says that it runs as the server does on a server that keeps apps apart: it was started before
	// it did, and goes on so until it restarts.
	sharesUser bool

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
	if d := m.opts.Containers.Driver(); d != nil {
		d.Shutdown()
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
		if in.releaseID == releaseID && in.backend != nil && !in.leaving.Load() {
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

const (
	// engineAtStart and adoptAtStart are how long a server waits as it starts for an engine to say how it stands, and for it to say what it is running.
	engineAtStart = 5 * time.Second
	adoptAtStart  = 15 * time.Second
	// adoptLimit is how long it has to say so of one container.
	adoptLimit = 10 * time.Second
)

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
	// Only what runs in containers has to wait for an engine: most servers never ask here. A server does not wait long, and what it did not get to waits for the engine.
	engineReady := false
	for _, li := range lives {
		if li.Driver == driverContainer {
			asking, cancel := context.WithTimeout(ctx, engineAtStart)
			engineReady = m.opts.Containers.Status(asking).Ready()
			cancel()
			break
		}
	}
	deadline := time.Now().Add(adoptAtStart)
	for _, li := range lives {
		known[li.ID] = true
		plan, err := readPlan(li.Build)
		if err != nil {
			return err
		}
		if li.Driver == driverContainer && (!engineReady || time.Now().After(deadline)) {
			m.waitForEngine(li)
			continue
		}
		if err := m.adoptOne(ctx, li, plan); err != nil {
			return err
		}
	}
	for _, id := range m.driver.List() {
		if !known[id] {
			m.log.Warn("stopping an instance unknown to the database", "instance", id)
			m.driver.Stop(id, 10*time.Second)
		}
	}
	// That can wait for the engine to say, and nothing else waits for it. With something still to adopt, it comes after.
	if len(m.pendingContainers()) == 0 {
		go m.stopUnknownContainers(ctx)
	}
	// Serve the current release of every adopted environment; instances of
	// any other release are what an interrupted rollout left, and go.
	for _, es := range m.allEnvs() {
		rel, build, err := m.db.CurrentRelease(ctx, es.app, es.env)
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
		// What each process speaks, and whether web sticks, is in this release's plan.
		if plan, err := readPlan(build); err == nil {
			es.pool.SetSticky(plan.Processes[manifest.Web].Sticky)
			for _, in := range es.list() {
				if in.backend != nil {
					in.backend.UseH2C(plan.Processes[in.process].Protocol == "h2c")
				}
			}
		}
		es.serve(rel.ID)
		m.retire(es, stale, 0)
	}
	return nil
}

// adoptOne takes over one instance that a previous server left running, or records that it has gone.
func (m *Manager) adoptOne(ctx context.Context, li store.LiveInstance, plan manifest.Plan) error {
	in := &instance{
		id: li.ID, process: li.Process, index: li.Index, releaseID: li.ReleaseID, version: li.Version,
		buildID: li.Build.ID, port: li.Port, proc: plan.Processes[li.Process],
		logFile: m.logFile(li.App, li.Env, li.Process, li.Index, li.ID), tmpDir: m.tmpDir(li.ID),
		run: m.driver,
	}
	if kind, err := m.db.EnvKind(ctx, li.App, li.Env); err == nil {
		in.softLimit = m.softLimitOf(plan.ForEnv(li.Env, kind))
		in.memoryLimit = memoryLimitOf(plan.ForEnv(li.Env, kind))
	}
	var st shim.State
	var err error
	if li.Driver == driverContainer {
		in.container = true
		if st, err = m.adoptContainer(ctx, li, plan, in); err != nil {
			if errors.Is(err, container.ErrNotFound) {
				m.db.StopInstance(ctx, li.ID)
				return nil
			}
			// Not knowing is not the same as its having gone: a daemon that is down says nothing of what it was running.
			m.log.Warn("cannot adopt a container yet", "app", li.App, "instance", li.ID, "err", err)
			m.waitForEngine(li)
			return nil
		}
		in.run = m.opts.Containers.Driver()
	} else {
		var alive bool
		st, alive, err = m.driver.Status(li.ID)
		if err != nil || !alive {
			if err == nil {
				m.driver.CleanupOrphan(st)
			}
			m.driver.Remove(li.ID)
			m.db.StopInstance(ctx, li.ID)
			return nil
		}
	}
	es := m.env(li.App, li.Env)
	if li.Port != 0 {
		in.backend = ingress.NewBackend(li.ID, fmt.Sprintf("127.0.0.1:%d", li.Port))
		in.backend.UseH2C(plan.Processes[li.Process].Protocol == "h2c")
	}
	if m.opts.Isolation.Isolating() && !in.container && !m.driver.RunsAsItsOwnUser(li.ID) {
		in.sharesUser = true
		m.log.Warn("this instance started before apps were kept apart, and goes on as the server's user until it restarts", "app", li.App, "env", li.Env, "instance", li.ID)
	}
	es.add(in)
	m.log.Info("adopted a running instance", "app", li.App, "env", li.Env, "instance", li.ID, "pid", st.AppPID)
	return nil
}

// waitForEngine leaves an instance to be adopted once the engine answers. Its record stays as it is.
func (m *Manager) waitForEngine(li store.LiveInstance) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	m.pending = append(m.pending, li)
}

func (m *Manager) pendingContainers() []store.LiveInstance {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return slices.Clone(m.pending)
}

// dropPending takes one instance off what is waiting for the engine.
func (m *Manager) dropPending(id string) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	m.pending = slices.DeleteFunc(m.pending, func(li store.LiveInstance) bool { return li.ID == id })
}

// isPending says that an environment has instances waiting for the engine, which are running as far as anyone knows.
func (m *Manager) isPending(app, env string) bool {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return slices.ContainsFunc(m.pending, func(li store.LiveInstance) bool { return li.App == app && li.Env == env })
}

// startAdopting looks at what was waiting for the engine behind the scenes, as it can be slow, and every other app should go on being looked after.
func (m *Manager) startAdopting(ctx context.Context) {
	if len(m.pendingContainers()) == 0 || !m.adopting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer m.adopting.Store(false)
		m.adoptPending(ctx)
	}()
}

// adoptPending takes over what was waiting for the engine, if it now answers. What cannot be got to stays waiting. Each is taken with its environment's
// rollout lock, and comes off the list only once that is held, so that nothing starts another in its place.
func (m *Manager) adoptPending(ctx context.Context) {
	pending := m.pendingContainers()
	if len(pending) == 0 || !m.opts.Containers.Status(ctx).Ready() {
		return
	}
	for _, li := range pending {
		es := m.env(li.App, li.Env)
		if !es.rollout.TryLock() {
			continue // a rollout is under way: the next look gets it, if the rollout has not
		}
		m.adoptWaiting(ctx, es, li)
		es.rollout.Unlock()
	}
	if len(m.pendingContainers()) == 0 {
		m.stopUnknownContainers(ctx)
	}
}

// adoptWaiting takes over one instance that was waiting for the engine. It takes the environment's rollout lock being held, and what cannot yet be found out goes on waiting.
func (m *Manager) adoptWaiting(ctx context.Context, es *envState, li store.LiveInstance) {
	m.dropPending(li.ID)
	plan, err := readPlan(li.Build)
	if err != nil {
		m.log.Error("cannot read the plan of a container", "instance", li.ID, "err", err)
		return
	}
	if err := m.adoptOne(ctx, li, plan); err != nil {
		m.log.Error("cannot adopt a container", "instance", li.ID, "err", err)
		return
	}
	for _, in := range es.list() {
		if in.id == li.ID {
			m.watch(es, in)
		}
	}
	// Whichever release is current now, which need not be the one this was running.
	if rel, _, err := m.db.CurrentRelease(ctx, li.App, li.Env); err == nil {
		es.serve(rel.ID)
	}
}

// stopUnknownContainers removes what no record explains, once the engine can be asked.
func (m *Manager) stopUnknownContainers(ctx context.Context) {
	d, err := m.containerDriver(ctx)
	if err != nil {
		return
	}
	// In this order, as a record is made before its container is.
	ids := d.List()
	lives, err := m.db.LiveInstances(ctx)
	if err != nil {
		return
	}
	known := make(map[string]bool, len(lives))
	for _, li := range lives {
		known[li.ID] = true
	}
	for _, id := range ids {
		// A one-off command has no record of an instance.
		if !known[id] && !d.Launched(id) {
			m.log.Warn("stopping a container unknown to the database", "instance", id)
			d.Stop(id, 10*time.Second)
		}
	}
}

// containerDriver is the driver to use, if the engine is answering, and why not otherwise.
func (m *Manager) containerDriver(ctx context.Context) (*container.Driver, error) {
	if err := m.opts.Containers.Status(ctx).Err(); err != nil {
		return nil, err
	}
	return m.opts.Containers.Driver(), nil
}

// engineFor is the engine's problem, if it has one and this environment needs it: for what its plan runs, for what it is running, or for what may still be running from
// before. Going on without it would stop nothing and record that it had.
func (m *Manager) engineFor(ctx context.Context, es *envState, plan manifest.Plan) error {
	if !m.needsEngine(es, plan) {
		return nil
	}
	if err := m.opts.Containers.Status(ctx).Err(); err != nil {
		return err
	}
	// What was waiting for it is found now, or whatever comes next starts another beside it. The rollout lock is held.
	for _, li := range m.pendingContainers() {
		if li.App == es.app && li.Env == es.env {
			m.adoptWaiting(ctx, es, li)
		}
	}
	if m.isPending(es.app, es.env) {
		return fmt.Errorf("%w: it has not said yet what %s of %s is running", container.ErrNotAnswering, es.env, es.app)
	}
	return nil
}

// EngineBlocks says why what is running in an environment could not be stopped, or with no environment, in any of an app's. Whatever is about to
// remove the record of a container asks this first, as it would otherwise run on with nothing to say that it should stop.
func (m *Manager) EngineBlocks(ctx context.Context, app, env string) error {
	needs := slices.ContainsFunc(m.pendingContainers(), func(li store.LiveInstance) bool { return li.App == app && (env == "" || li.Env == env) })
	for _, es := range m.allEnvs() {
		if needs {
			break
		}
		if es.app == app && (env == "" || es.env == env) {
			needs = slices.ContainsFunc(es.list(), func(in *instance) bool { return in.container })
		}
	}
	if !needs {
		return nil
	}
	return m.opts.Containers.Status(ctx).Err()
}

func (m *Manager) needsEngine(es *envState, plan manifest.Plan) bool {
	if plan.Container != nil || m.isPending(es.app, es.env) {
		return true
	}
	return slices.ContainsFunc(es.list(), func(in *instance) bool { return in.container })
}

// noteEngineNotNeeded clears what was said, if nothing here is waiting for it any more.
func (m *Manager) noteEngineNotNeeded(ctx context.Context) {
	if !m.engineDown {
		return
	}
	m.engineDown = false
	if m.opts.Events != nil {
		m.opts.Events.Publish(ctx, store.Event{Type: events.EngineOK, Data: map[string]any{"not_needed": true}})
	}
}

// noteEngine says, once, when what apps here need has stopped answering, and when it comes back.
func (m *Manager) noteEngine(ctx context.Context, st container.Status) {
	if st.State == container.Unknown {
		return // it has yet to say
	}
	down := !st.Ready()
	if down == m.engineDown {
		return
	}
	m.engineDown = down
	if m.opts.Events == nil {
		return
	}
	if down {
		m.opts.Events.Publish(ctx, store.Event{Type: events.EngineDown, Data: map[string]any{"problem": st.Problem()}})
	} else {
		m.opts.Events.Publish(ctx, store.Event{Type: events.EngineOK})
	}
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
	m.startAdopting(ctx)
	wanted := false
	for _, r := range routes {
		plan, err := readPlan(r.Build)
		if err != nil {
			continue
		}
		if plan.Kind != manifest.KindProcess {
			wanted = m.retireLeaving(ctx, r) || wanted
			continue
		}
		es := m.env(r.App, r.Env)
		// This is the only thing here that asks for an engine, and only for an app that needs one. With none, what is running is left as it
		// is, and nothing is started to fail every few seconds.
		if m.needsEngine(es, plan) {
			wanted = true
			// As last found, as a daemon that hangs should not hold up looking after every other app. It is asked again behind the scenes.
			st := m.opts.Containers.Known()
			m.noteEngine(ctx, st)
			if !st.Ready() {
				continue
			}
		}
		if !es.rollout.TryLock() {
			continue // a rollout is under way
		}
		// What is waiting may yet be found running, and nothing should start in its place.
		if m.isPending(r.App, r.Env) {
			es.rollout.Unlock()
			continue
		}
		if err := m.ensureRoute(ctx, es, r, plan); err != nil && ctx.Err() == nil {
			m.log.Error("cannot start an instance", "app", r.App, "env", r.Env, "err", err)
		}
		es.rollout.Unlock()
	}
	if !wanted && m.opts.Services != nil && m.opts.Services.Any(ctx) {
		wanted = true
		m.noteEngine(ctx, m.opts.Containers.Known())
	}
	if !wanted {
		m.noteEngineNotNeeded(ctx)
	}
}

// retireLeaving stops what an environment that now serves a static site could not stop before, and says whether there was any.
func (m *Manager) retireLeaving(ctx context.Context, r store.Route) bool {
	es := m.env(r.App, r.Env)
	var leaving []*instance
	for _, in := range es.list() {
		if in.leaving.Load() {
			leaving = append(leaving, in)
		}
	}
	if len(leaving) == 0 {
		return false
	}
	st := m.opts.Containers.Status(ctx)
	m.noteEngine(ctx, st)
	if st.Ready() && es.rollout.TryLock() {
		m.retire(es, leaving, 0)
		es.rollout.Unlock()
	}
	return true
}

// ensureRoute brings one environment to what it should be running now. It takes the
// environment's rollout lock being held.
func (m *Manager) ensureRoute(ctx context.Context, es *envState, r store.Route, plan manifest.Plan) error {
	rel, _, err := m.db.CurrentRelease(ctx, r.App, r.Env)
	if err != nil {
		return err
	}
	// The route was read before the lock was held, and a rollout may have finished in between. Going by it
	// would start a release that has just been replaced. The next look has the new one.
	if rel.Version != r.Version {
		return nil
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
	es := m.env(app, env)
	es.rollout.Lock()
	defer es.rollout.Unlock()
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
		if err := m.engineFor(ctx, es, plan); err != nil {
			return err
		}
		return m.ensureRoute(ctx, es, r, plan)
	}
	return nil
}

// ensureEnv brings an environment to what its plan says, by starting the instances
// it lacks and stopping those beyond its number. It goes on after an instance that
// cannot start, and returns the first such failure.
func (m *Manager) ensureEnv(ctx context.Context, es *envState, rel store.Release, build store.Build, plan manifest.Plan) error {
	// Nothing may have survived to be adopted: after a reboot, or for an environment that was asleep.
	es.pool.SetSticky(plan.Processes[manifest.Web].Sticky)
	have := map[string]bool{}
	var keep, extra, earlier []*instance
	for _, in := range es.list() {
		if in.leaving.Load() {
			m.retire(es, []*instance{in}, 0)
			continue
		}
		if in.releaseID != rel.ID {
			earlier = append(earlier, in)
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
	// With the lock held, no rollout is under way, so that these are of a release that is not current: what a
	// server that stopped part of the way through a rollout left, say. They go once what should be running is.
	if len(earlier) > 0 && failed == nil {
		m.log.Warn("instances of another release were running; stopping them", "app", es.app, "env", es.env, "instances", len(earlier), "release", rel.Version)
		es.serve(rel.ID)
		m.retire(es, earlier, plan.Deploy.Drain.Std())
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
	st := m.opts.Isolation.Status
	waiting := 0
	for _, es := range m.allEnvs() {
		waiting += es.sharingUser()
	}
	if waiting > 0 {
		st.Notes = append(append([]string{}, st.Notes...), fmt.Sprintf("%d instances started before apps were kept apart, and go on as the server's user until they restart", waiting))
	}
	return st
}

// SharingUser says how many of an environment's instances still run as the server does.
func (m *Manager) SharingUser(app, env string) int { return m.env(app, env).sharingUser() }

func (es *envState) sharingUser() int {
	n := 0
	for _, in := range es.list() {
		if in.sharesUser {
			n++
		}
	}
	return n
}

// softLimitOf is the memory to watch, where the kernel will not enforce the limit itself.
func (m *Manager) softLimitOf(plan manifest.Plan) int64 {
	if plan.Resources == nil || m.Isolation().Enforced() {
		return 0
	}
	return plan.Resources.Memory
}

func memoryLimitOf(plan manifest.Plan) int64 {
	if plan.Resources == nil {
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
			out = append(out, metrics.Instance{SoftLimit: in.softLimit, MemoryLimit: in.memoryLimit, EnvID: r.EnvID, ID: in.id, Part: fmt.Sprintf("%s.%d", in.process, in.index), PID: st.AppPID})
		}
	}
	return out
}

// LogSources lists the log files of what is running now.
func (m *Manager) LogSources(ctx context.Context) ([]LogSource, error) {
	routes, err := m.db.Routes(ctx)
	if err != nil {
		return nil, err
	}
	var out []LogSource
	for _, r := range routes {
		m.mu.Lock()
		es := m.envs[r.App+"/"+r.Env]
		m.mu.Unlock()
		if es == nil {
			continue
		}
		for _, in := range es.list() {
			if in.logFile != "" {
				out = append(out, LogSource{App: r.App, Env: r.Env, Process: in.process, Instance: in.id, Path: in.logFile})
			}
		}
	}
	return out, nil
}

// LogSource is a log file of something that is running.
type LogSource struct{ App, Env, Process, Instance, Path string }

// MetricsContainers says what the container engine knows of the instances it runs.
func (m *Manager) MetricsContainers(ctx context.Context) map[string]metrics.Usage {
	d := m.opts.Containers.Driver()
	if d == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	stats, err := d.Stats(ctx)
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
