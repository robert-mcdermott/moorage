// Package server runs a Moorage server: it prepares the data directory,
// takes the directory's lock, opens and migrates the database, and serves
// apps, platform endpoints and the API until its context is cancelled.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	stdlog "log"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/robert-mcdermott/moorage/internal/api"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/backup"
	"github.com/robert-mcdermott/moorage/internal/build"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
	consolepages "github.com/robert-mcdermott/moorage/internal/console"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/forge"
	"github.com/robert-mcdermott/moorage/internal/gitserver"
	"github.com/robert-mcdermott/moorage/internal/ingress"
	"github.com/robert-mcdermott/moorage/internal/isolate"
	"github.com/robert-mcdermott/moorage/internal/layout"
	"github.com/robert-mcdermott/moorage/internal/localdns"
	"github.com/robert-mcdermott/moorage/internal/notify"
	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/secrets"
	"github.com/robert-mcdermott/moorage/internal/services"
	"github.com/robert-mcdermott/moorage/internal/sso"
	"github.com/robert-mcdermott/moorage/internal/store"
	"github.com/robert-mcdermott/moorage/internal/telemetry"
	"github.com/robert-mcdermott/moorage/internal/toolchain"
	"github.com/robert-mcdermott/moorage/internal/truststore"
)

// Layout names the files and directories inside a data directory.
type Layout = layout.Layout

// NewLayout returns the layout of the data directory root.
func NewLayout(root string) Layout { return layout.New(root) }

// logWriter writes what a tool says, a line at a time, to the server's log.
type logWriter struct {
	log  *slog.Logger
	what []any
}

func (w logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if line != "" {
			w.log.Info(line, w.what...)
		}
	}
	return len(p), nil
}

// fetchToolchains gets the toolchains that apps need and this server does not have, as on a restored server, which
// has their builds and not those. It happens on its own: what keeps instances running goes through apps one at a
// time, and one waiting for a download should not hold up the rest. The app starts the next time it is looked at.
func fetchToolchains(life context.Context, toolchains *toolchain.Manager, log *slog.Logger) func(ctx context.Context, name, version string) error {
	var mu sync.Mutex
	going := map[string]bool{}
	failedAt := map[string]time.Time{}
	return func(_ context.Context, name, version string) error {
		key := name + "@" + version
		mu.Lock()
		defer mu.Unlock()
		if going[key] {
			return errors.New("it is being fetched")
		}
		if time.Since(failedAt[key]) < time.Minute {
			return errors.New("fetching it failed a moment ago: the server's log says why, and it tries again")
		}
		going[key] = true
		go func() {
			ctx, cancel := context.WithTimeout(life, 15*time.Minute)
			defer cancel()
			say := logWriter{log, []any{"toolchain", name, "version", version}}
			log.Info("fetching a toolchain that an app needs", say.what...)
			var uv *toolchain.Installed
			var err error
			if name == "python" {
				var found toolchain.Installed
				if found, err = toolchains.Ensure(ctx, "uv", "", nil, say); err == nil {
					uv = &found
				}
			}
			if err == nil {
				_, err = toolchains.Ensure(ctx, name, version, uv, say)
			}
			mu.Lock()
			defer mu.Unlock()
			delete(going, key)
			if err != nil {
				failedAt[key] = time.Now()
				log.Error("a toolchain that an app needs could not be fetched", "toolchain", name, "version", version, "err", err)
			}
		}()
		return errors.New("it is being fetched")
	}
}

// keepHostsBlock has the block in the hosts file name what is deployed now, if there is one and this server may write
// to it, as one in an elevated terminal may and the service does not. Nothing else keeps it up to date on Windows.
func keepHostsBlock(ctx context.Context, cfg config.Config, db *store.DB, log *slog.Logger) {
	if goruntime.GOOS != "windows" || cfg.Domain != "localhost" {
		return
	}
	path := filepath.Join(os.Getenv("SystemRoot"), "System32", "drivers", "etc", "hosts")
	if !localdns.HasHostsBlock(path) {
		return
	}
	routes, err := db.Routes(ctx)
	if err != nil {
		return
	}
	names := []string{cfg.ConsoleHost}
	for _, r := range routes {
		names = append(names, cfg.EnvHost(r.App, r.Env))
	}
	if _, err := localdns.WriteHostsBlock(path, names); err != nil && !errors.Is(err, fs.ErrPermission) {
		log.Warn("keep the hosts file up to date", "err", err)
	}
}

// trustsThisMachine finds a trusted range that holds an address of this machine.
func trustsThisMachine(trusted []netip.Prefix) (netip.Prefix, bool) {
	own := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				own = append(own, p.Addr().Unmap())
			}
		}
	}
	for _, p := range trusted {
		for _, a := range own {
			if p.Contains(a) {
				return p, true
			}
		}
	}
	return netip.Prefix{}, false
}

// Addrs are the addresses the listeners are bound to; HTTPS is nil when
// Moorage does not terminate TLS.
type Addrs struct {
	HTTP  net.Addr
	HTTPS net.Addr
	// SSH is nil when git pushes over SSH are off.
	SSH net.Addr
	// DNS is nil when the DNS responder is off.
	DNS net.Addr
}

// Options adjust Run.
type Options struct {
	// ConsoleDir, if set, is where the console's templates and assets are read from,
	// for each request, in place of those in the binary.
	ConsoleDir string
	// Ready, if set, is called once the server accepts connections.
	Ready func(Addrs)
	// Executable runs this program's other modes (`moor shim`, `moor sh`);
	// it defaults to the running executable. ShimEnv and ShellEnv are added
	// to those modes' environments (tests use them).
	Executable string
	ShimEnv    []string
	ShellEnv   []string
	// BackoffMin, if set, overrides the first delay before an app is
	// restarted (tests shorten it).
	BackoffMin time.Duration
	// Tick, if set, overrides how often dead instances are looked for.
	Tick time.Duration
	// GCMinAge, if set, overrides how old a build must be before garbage
	// collection may remove it (tests shorten it).
	GCMinAge time.Duration
	// MetricsEvery, if set, overrides how often metrics are sampled (tests).
	MetricsEvery time.Duration
	// BackupTick, if set, overrides how often the schedule of backups is looked at (tests).
	BackupTick time.Duration
	// ForgeTick, if set, overrides how often forges that have to be looked at are (tests).
	ForgeTick time.Duration
	// TelemetryTick, TelemetryMetricsEvery and ThresholdTick, if set, override how often log files are read, metrics are
	// sent and thresholds are looked at (tests).
	TelemetryTick, TelemetryMetricsEvery, ThresholdTick, ThresholdMinute time.Duration
	// NotifyRetry, if set, overrides how long a notification waits before trying again (tests).
	NotifyRetry time.Duration
	// CronTick, if set, overrides how often cron jobs are looked at (tests).
	CronTick time.Duration
	// GateRecheck, if set, overrides how often gates look again at whom they let in (tests).
	GateRecheck time.Duration
	// EngineKept, if set, overrides how long an answer about the container engine stands (tests).
	EngineKept time.Duration
	// Resolver, if set, looks up what domains point to, instead of the system's (tests).
	Resolver deploy.DomainResolver
	// EnvironmentSweep, if set, overrides how often environments are looked at, to see which
	// have expired or are idle (tests shorten it).
	EnvironmentSweep time.Duration
}

// authorizeGit decides a Smart HTTP request by the API token in its password:
// fetching needs the read scope, pushing the deploy scope, and a token for one
// app reaches only that app.
func authorizeGit(r *http.Request, authn *auth.Service, password, app string, push bool) (string, error) {
	p, err := authn.Token(r.Context(), password, authn.ClientIP(r))
	if err != nil {
		return "", gitserver.ErrBadCredentials
	}
	if err := allowGit(r.Context(), authn, p, app, push); err != nil {
		return "", err
	}
	return p.Name, nil
}

// allowGit says whether someone may reach an app's repository: to read it, or to push,
// which deploys.
func allowGit(ctx context.Context, authn *auth.Service, p auth.Principal, app string, push bool) error {
	access, err := authn.AppAccess(ctx, p, app)
	if err != nil {
		return err
	}
	if !access.See {
		return gitserver.ErrNoRepository
	}
	// A repository holds every branch of an app, and a push can go to any of them. A token limited to previews deploys by uploading.
	if p.Envs != "" || push && !access.Deploy {
		return gitserver.ErrForbidden
	}
	return nil
}

// pruneAudit keeps the audit log to its retention period, until ctx ends.
func pruneAudit(ctx context.Context, db *store.DB, log *slog.Logger) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		if _, err := db.PruneAudit(ctx, time.Now().Add(-auth.AuditRetention)); err != nil && ctx.Err() == nil {
			log.Warn("prune the audit log", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// auditedPushes keeps a record of every push.
type auditedPushes struct {
	gitserver.Receiver
	auth *auth.Service
}

func (a auditedPushes) Receive(ctx context.Context, p *gitserver.Push) error {
	err := a.Receiver.Receive(ctx, p)
	result := "ok"
	if err != nil {
		result = "failed"
	}
	a.auth.Audit(ctx, store.AuditEntry{Actor: p.By, Action: "git.push", Target: p.App, Result: result})
	return err
}

// Run serves until ctx is cancelled, then shuts down gracefully. The
// configuration must already be validated.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger, opts Options) error {
	layout := NewLayout(cfg.DataDir)
	if err := layout.Prepare(); err != nil {
		return fmt.Errorf("prepare the data directory %s: %w", cfg.DataDir, err)
	}
	lock, err := platform.LockFile(layout.LockFile)
	if errors.Is(err, platform.ErrLocked) {
		return fmt.Errorf("another moor server is already using %s", cfg.DataDir)
	}
	if err != nil {
		return fmt.Errorf("lock the data directory: %w", err)
	}
	defer lock.Unlock()

	db, err := store.Open(ctx, layout.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	migrated, err := db.Migrate(ctx, layout.Backups)
	if err != nil {
		return err
	}
	if migrated.Snapshot != "" {
		log.Info("database snapshot taken before migrating", "path", migrated.Snapshot)
	}
	for _, m := range migrated.Applied {
		log.Info("database migrated", "version", m.Version, "migration", m.Name)
	}
	if err := deploy.CleanPartial(cfg.DataDir); err != nil {
		log.Warn("could not remove half-written builds", "err", err)
	}
	if n, err := db.InterruptDeployments(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("deployments interrupted by the last stop were marked failed", "count", n)
	}

	tokenHash, created, err := auth.BootstrapToken(ctx, db, layout.AdminToken)
	if err != nil {
		return err
	}
	if created {
		log.Info("bootstrap admin token written; the CLI on this machine finds it, other machines pass it with --token", "file", layout.AdminToken)
	}

	// The master key is created with the first secret's data key, never
	// once secrets exist: they could not be read with a new key.
	sealed, err := db.HasSealed(ctx)
	if err != nil {
		return err
	}
	masterKey, err := secrets.LoadMasterKey(layout.MasterKey, os.Getenv, !sealed)
	if err != nil {
		return err
	}
	if masterKey.Created {
		log.Info("generated the master key that encrypts secrets; back it up apart from the database", "file", masterKey.Path)
	}
	keyring, err := secrets.Open(ctx, masterKey, db)
	if err != nil {
		return err
	}

	exe := opts.Executable
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	router := ingress.NewRouter(cfg, db, cfg.DataDir, log)
	defer router.Close()
	metricsStore := metrics.New(db, log)
	router.SetMetrics(metricsStore)
	toolchains := toolchain.New(filepath.Join(cfg.DataDir, "toolchains"), filepath.Join(cfg.DataDir, "cache"), toolchain.Sources{
		Go: cfg.Toolchains.GoURL, Node: cfg.Toolchains.NodeURL, UV: cfg.Toolchains.UVURL, Python: cfg.Toolchains.PythonURL,
		SignaturesOnlyWarn: cfg.Toolchains.Signatures == "warn",
	})
	shell := []string{exe, "sh", "-c"}
	uidBase := cfg.Isolation.UIDBase
	if uidBase == 0 {
		uidBase = isolate.DefaultUIDBase
	}
	isolation, err := isolate.New(isolate.Config{Level: cfg.Isolation.Level, UIDBase: uidBase, DataDir: cfg.DataDir, Binary: exe}, func(ctx context.Context, app string) (uint32, error) {
		return db.AppUID(ctx, app, uidBase)
	})
	if err != nil {
		return err
	}
	log.Info("isolation", "level", isolation.Level, "limits_enforced", isolation.Enforced(), "landlock", isolation.Landlock)
	for _, note := range isolation.Notes {
		log.Warn("isolation: " + note)
	}
	containers := containerSource(ctx, cfg, db, keyring, opts.EngineKept, log)
	// Once at start, off the main path, so that a hung daemon delays nothing. Only what needs an engine asks again.
	go containers.Status(ctx)
	bus := events.New(db, log)
	defer bus.Close()
	go bus.Prune(ctx, 24*time.Hour)
	proxyKey := masterKey.Derive("proxy tokens v1")
	proxyToken := func(envID string) string {
		mac := hmac.New(sha256.New, proxyKey)
		mac.Write([]byte(envID))
		return hex.EncodeToString(mac.Sum(nil))
	}
	router.SetProxyToken(proxyToken)
	// It takes its ports from the runtime's allocator, which counts its ports in turn.
	var runtime *reconcile.Manager
	backing := services.New(services.Options{
		DB: db, Keyring: keyring, Engines: containers, Events: bus, Log: log, Tick: opts.Tick, DataDir: cfg.DataDir, GateFor: cfg.GateFor,
		AllocPort:   func(ctx context.Context) (int, error) { return runtime.AllocPort(ctx) },
		ReleasePort: func(port int) { runtime.ReleasePort(port) },
	})
	runtime = reconcile.New(db, cfg, reconcile.Options{
		Services:        backing,
		ProxyToken:      proxyToken,
		DataDir:         cfg.DataDir,
		Isolation:       isolation,
		ShimCmd:         []string{exe, "shim"},
		ShimEnv:         opts.ShimEnv,
		Shell:           shell,
		ShellEnv:        opts.ShellEnv,
		BackoffMin:      opts.BackoffMin,
		Tick:            opts.Tick,
		Trusted:         router.Trusted(),
		ToolchainBins:   toolchains.BinDirs,
		EnsureToolchain: fetchToolchains(ctx, toolchains, log),
		Secrets:         keyring,
		Containers:      containers,
		Events:          bus,
	}, log)
	defer runtime.Shutdown()
	router.SetPools(runtime)
	// Instances that kept running while the server was down are taken over
	// before the listeners open, so their traffic resumes at once.
	if err := runtime.Adopt(ctx); err != nil {
		return err
	}
	if err := router.Reload(ctx); err != nil {
		return err
	}
	go runtime.Run(ctx)
	go backing.Run(ctx)
	go pruneAudit(ctx, db, log)
	repos := gitserver.NewRepos(layout.Repos)
	deployer := deploy.New(db, deploy.Options{
		DataDir:  cfg.DataDir,
		Builder:  &build.Builder{Isolation: isolation, DataDir: cfg.DataDir, Toolchains: toolchains, Shell: shell, ShellEnv: opts.ShellEnv, Engines: containers},
		Runtime:  runtime,
		Services: backing,
		Reload: func(ctx context.Context) error {
			err := router.Reload(ctx)
			keepHostsBlock(ctx, cfg, db, log)
			return err
		},
		URL: func(app, env string) string {
			return cfg.EnvURL(app, env)
		},
		GCMinAge:   opts.GCMinAge,
		Secrets:    keyring,
		Events:     bus,
		Repos:      repos,
		ForgetApp:  router.ForgetApp,
		LastActive: router.LastActive,
		Busy:       router.Busy,

		Config:   cfg,
		Resolver: opts.Resolver,
	}, log)
	router.SetSleeper(deployer)
	// Running builds are stopped (and recorded as failed) before the
	// database closes.
	defer deployer.Shutdown()
	sweep := opts.EnvironmentSweep
	if sweep == 0 {
		sweep = time.Minute
	}
	go deployer.Sweep(ctx, sweep)
	collector := &metrics.Collector{Store: metricsStore, Log: log, DataDir: cfg.DataDir, OverLimit: runtime.OverMemory,
		Instances: func(ctx context.Context) []metrics.Instance {
			return append(runtime.MetricsInstances(ctx), backing.MetricsInstances(ctx)...)
		},
		// Side by side, as either can take as long as an engine is allowed, and what comes next waits for them.
		Containers: func(ctx context.Context) map[string]metrics.Usage {
			forServices := make(chan map[string]metrics.Usage, 1)
			go func() { forServices <- backing.MetricsContainers(ctx) }()
			usage := map[string]metrics.Usage{}
			maps.Copy(usage, runtime.MetricsContainers(ctx))
			maps.Copy(usage, <-forServices)
			return usage
		},
	}
	go collector.Run(ctx, opts.MetricsEvery)
	cronTick := opts.CronTick
	if cronTick == 0 {
		cronTick = 10 * time.Second
	}
	go deployer.RunCron(ctx, cronTick)
	allowed, err := cfg.NotificationsAllowed()
	if err != nil {
		return err
	}
	notifier := &notify.Service{DB: db, Bus: bus, Keyring: keyring, Log: log, ConsoleURL: cfg.PublicURL(cfg.ConsoleHost), Allow: allowed, DataDir: cfg.DataDir, RetryAfter: opts.NotifyRetry,
		DiskPercent: cfg.Notifications.DiskPercent, Metrics: metricsStore, Instances: runtime.MetricsInstances, ThresholdTick: opts.ThresholdTick, ThresholdMinute: opts.ThresholdMinute}
	notifyCtx, stopNotifying := context.WithCancel(ctx)
	go notifier.Run(notifyCtx)
	defer func() {
		stopNotifying()
		notifier.Wait()
	}()
	if cfg.Auth.TrustedHeader != "" {
		if own, ok := trustsThisMachine(router.Trusted()); ok {
			log.Warn("auth.trusted_header is believed from this machine's own addresses, and apps deployed here can use those too: they could say that they are anyone. Run the proxy on another machine, and trust only its address", "trusted", own.String())
		}
	}
	authn := &auth.Service{
		Sealer: keyring, SecondFactorKey: masterKey.Derive("second factor v1"),
		TrustedHeader: cfg.Auth.TrustedHeader, TrustedNameHeader: cfg.Auth.TrustedNameHeader, TrustedProvision: cfg.Auth.TrustedProvision,
		DB: db, LocalToken: tokenHash, Trusted: router.Trusted(), Secure: cfg.Proxy.ExternalScheme == "https",
		ClaimFile: layout.ClaimCode, Limits: auth.NewLimiter(), Log: log,
	}
	router.SetGatekeeper(&ingress.Gatekeeper{Auth: authn, DB: db, Key: masterKey.Derive("gate cookies v1"), Secure: authn.Secure, ConsoleURL: cfg.PublicURL(cfg.ConsoleHost), Recheck: opts.GateRecheck})
	claimCode, err := authn.PrepareClaim(ctx)
	if err != nil {
		return fmt.Errorf("prepare the first-run claim: %w", err)
	}
	if claimCode != "" {
		log.Info("this server has no accounts yet: claim it with the setup link",
			"url", cfg.PublicURL(cfg.ConsoleHost)+"/setup?code="+claimCode)
	}

	// The git server: Smart HTTP on the console's hostname, SSH on its own
	// port.
	git := &gitserver.Server{Repos: repos, Receiver: auditedPushes{&deploy.Pushes{Deployer: deployer}, authn}, Log: log}
	console := http.NewServeMux()
	backups := &backup.Service{Before: backing.DumpAll, DB: db, Bus: bus, Keyring: keyring, Keys: masterKey, Log: log, DataDir: cfg.DataDir, Version: buildinfo.Version, Domain: cfg.Domain, KeyFingerprint: backup.Fingerprint(masterKey), Tick: opts.BackupTick}
	backupCtx, stopBackups := context.WithCancel(ctx)
	go backups.Run(backupCtx)
	defer func() {
		stopBackups()
		backups.Wait()
	}()
	forgeCtx, stopForges := context.WithCancel(ctx)
	forges := &forge.Service{DB: db, Repos: repos, Pushes: &deploy.Pushes{Deployer: deployer}, Keys: keyring, Log: log, Life: forgeCtx, Tick: opts.ForgeTick,
		Client: notify.GuardedClient(allowed, 20*time.Second), Fetcher: notify.GuardedClient(allowed, 0),
		EnvURL: cfg.EnvURL,
		AppURL: func(app string) string { return cfg.PublicURL(cfg.ConsoleHost) + "/apps/" + url.PathEscape(app) },
		Audit: func(ctx context.Context, by, action, target, result string) {
			authn.Audit(ctx, store.AuditEntry{Actor: by, Action: action, Target: target, Result: result})
		},
	}
	go forges.Run()
	defer func() {
		stopForges()
		forges.Wait()
	}()
	telemetryCtx, stopTelemetry := context.WithCancel(ctx)
	serverName, _ := os.Hostname()
	sinks := &telemetry.Service{DB: db, Keys: keyring, Log: log, Life: telemetryCtx, Tick: opts.TelemetryTick, MetricsEvery: opts.TelemetryMetricsEvery,
		Client: notify.GuardedClient(allowed, 15*time.Second), Dial: notify.GuardedDial(allowed), Server: serverName, Version: buildinfo.Version,
		Sources: func(ctx context.Context) ([]telemetry.Source, error) {
			running, err := runtime.LogSources(ctx)
			if err != nil {
				return nil, err
			}
			var out []telemetry.Source
			for _, l := range running {
				out = append(out, telemetry.Source(l))
			}
			for _, l := range deployer.RunLogSources() {
				out = append(out, telemetry.Source(l))
			}
			return out, nil
		},
		Metrics: func(ctx context.Context) metrics.Export {
			all, err := db.AllEnvironments(ctx)
			if err != nil {
				return metrics.Export{}
			}
			envs := make([]metrics.PromEnv, 0, len(all))
			for _, e := range all {
				envs = append(envs, metrics.PromEnv{App: e.App, Env: e.Name, EnvID: e.ID})
			}
			return metricsStore.ExportAll(envs, true)
		},
	}
	go sinks.Run()
	defer func() {
		stopTelemetry()
		sinks.Wait()
	}()
	signIn := &sso.Service{DB: db, Auth: authn, Keyring: keyring, Log: log, ConsoleURL: cfg.PublicURL(cfg.ConsoleHost), StateKey: masterKey.Derive("sso state v1")}
	authn.PasswordsOff = signIn.PasswordsOff
	console.Handle("/api/", api.New(db, cfg, cfg.DataDir, authn, runtime, deployer, keyring, bus, metricsStore, notifier, backups, signIn, forges, sinks, opts.GateRecheck, log))
	console.Handle("/metrics", api.Prometheus(db, authn, metricsStore))
	console.Handle("/git/", &gitserver.HTTP{Git: git, Authorize: func(r *http.Request, password, app string, push bool) (string, error) {
		return authorizeGit(r, authn, password, app, push)
	}})
	pages, err := consolepages.New(consolepages.Options{Auth: authn, DB: db, Config: cfg, Log: log, Deployer: deployer, Notifier: notifier, Backups: backups, Forges: forges, Telemetry: sinks, SSO: signIn, Runtime: runtime, Keyring: keyring, Dir: opts.ConsoleDir, GateRoute: router.RouteFor, Metrics: metricsStore})
	if err != nil {
		return fmt.Errorf("the console: %w", err)
	}
	console.Handle("/", pages)
	router.SetConsole(console)

	var tlsConfig *tls.Config
	var acmeCerts *ingress.ACME
	switch cfg.TLS.Mode {
	case "files":
		certs, err := ingress.NewFileCertificates(cfg.TLS.Certificates, log)
		if err != nil {
			return err
		}
		go certs.Watch(ctx, time.Minute)
		tlsConfig = ingress.TLSConfig(certs)
	case "internal":
		ca, created, err := ingress.LoadInternalCA(layout.Certs, cfg, router.AllowCertificate)
		if err != nil {
			return err
		}
		if created {
			log.Info("generated the internal CA; browsers trust Moorage's certificates once it is installed",
				"certificate", filepath.Join(layout.Certs, "ca.crt"), "download", cfg.PublicURL(cfg.ConsoleHost)+"/.moorage/ca.crt")
		}
		router.SetCACert(ca.PEM())
		log.Info("the internal CA; moor trust installs it on other machines", "fingerprint", "SHA-256 "+truststore.Fingerprint(ca.Certificate()))
		tlsConfig = ingress.TLSConfig(ca)
	case "acme":
		acmeCerts, err = ingress.NewACME(ingress.ACMEOptions{Dir: filepath.Join(layout.Certs, ingress.ACMEDir), Config: cfg, Allow: router.AllowIssuing, Log: log,
			Told: func(failed bool, name, problem string) {
				kind := events.CertificateOK
				if failed {
					kind = events.CertificateFailed
				}
				bus.Publish(context.Background(), store.Event{Type: kind, Data: map[string]any{"name": name, "error": problem}})
			}})
		if err != nil {
			return err
		}
		tlsConfig = acmeCerts.TLSConfig()
	}

	httpLn, err := net.Listen("tcp", cfg.Listen.HTTP)
	if err != nil {
		return listenError(cfg.Listen.HTTP, err)
	}
	addrs := Addrs{HTTP: httpLn.Addr()}
	errorLog := slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	var httpHandler http.Handler = router
	servers := []*http.Server{}
	served := make(chan error, 3)

	if tlsConfig != nil {
		httpsLn, err := net.Listen("tcp", cfg.Listen.HTTPS)
		if err != nil {
			httpLn.Close()
			return listenError(cfg.Listen.HTTPS, err)
		}
		addrs.HTTPS = httpsLn.Addr()
		_, port, _ := net.SplitHostPort(httpsLn.Addr().String())
		httpHandler = router.RedirectToHTTPS(port)
		if acmeCerts != nil {
			httpHandler = acmeCerts.HTTPChallenges(httpHandler)
		}
		var tlsHandler http.Handler = router
		if cfg.TLS.HSTS {
			tlsHandler = ingress.HSTS(router)
		}
		httpsSrv := newHTTPServer(tlsHandler, errorLog)
		httpsSrv.TLSConfig = tlsConfig
		servers = append(servers, httpsSrv)
		go func() { served <- httpsSrv.ServeTLS(httpsLn, "", "") }()
	}
	httpSrv := newHTTPServer(httpHandler, errorLog)
	// With no TLS, gRPC and others speak HTTP/2 straight away: a client on this computer, or a proxy that has dealt with TLS.
	httpSrv.Protocols = new(http.Protocols)
	httpSrv.Protocols.SetHTTP1(true)
	httpSrv.Protocols.SetUnencryptedHTTP2(true)
	servers = append(servers, httpSrv)
	go func() { served <- httpSrv.Serve(httpLn) }()
	if acmeCerts != nil {
		if err := acmeCerts.Start(ctx); err != nil {
			log.Warn("could not start asking for certificates", "err", err)
		}
	}

	var sshSrv *gitserver.SSH
	if cfg.Listen.SSH != "" {
		hostKey, err := gitserver.LoadHostKey(layout.SSHHostKey)
		if err != nil {
			return fmt.Errorf("the SSH host key: %w", err)
		}
		sshLn, err := net.Listen("tcp", cfg.Listen.SSH)
		if err != nil {
			for _, s := range servers {
				s.Close()
			}
			return listenError(cfg.Listen.SSH, err)
		}
		addrs.SSH = sshLn.Addr()
		sshSrv = &gitserver.SSH{
			Git: git, HostKey: hostKey, Attempts: authn.Limits,
			Key: func(ctx context.Context, key ssh.PublicKey) (string, bool) {
				p, _, err := authn.Key(ctx, ssh.FingerprintSHA256(key))
				return p.Name, err == nil
			},
			Authorize: func(ctx context.Context, fingerprint, app string, push bool) error {
				p, _, err := authn.Key(ctx, fingerprint)
				if err != nil {
					return gitserver.ErrForbidden
				}
				return allowGit(ctx, authn, p, app, push)
			},
			Used: func(fingerprint string) {
				if k, err := db.SSHKeyByFingerprint(ctx, fingerprint); err == nil {
					db.TouchSSHKey(ctx, k.ID)
				}
			},
		}
		go func() {
			if err := sshSrv.Serve(sshLn); err != nil {
				served <- err
			}
		}()
	}

	if cfg.Listen.DNS != "" {
		dnsConn, err := net.ListenPacket("udp", cfg.Listen.DNS)
		if err != nil {
			for _, s := range servers {
				s.Close()
			}
			return listenError(cfg.Listen.DNS, err)
		}
		defer dnsConn.Close()
		addrs.DNS = dnsConn.LocalAddr()
		go func() {
			if err := localdns.Serve(ctx, dnsConn, cfg.Domain, log); err != nil {
				log.Error("the DNS responder stopped", "err", err)
			}
		}()
		log.Info("answering DNS queries for the domain, on loopback", "dns", addrs.DNS.String(), "domain", cfg.Domain)
	}

	info := buildinfo.Get()
	log.Info("moorage server started",
		"version", info.Version, "commit", info.Commit,
		"config", cfg.Path, "data_dir", cfg.DataDir, "domain", cfg.Domain,
		"http", addrs.HTTP.String(), "https", addrString(addrs.HTTPS), "tls", cfg.TLS.Mode,
		"console", cfg.PublicURL(cfg.ConsoleHost))
	if addrs.SSH != nil {
		log.Info("git pushes over SSH", "ssh", addrs.SSH.String(), "host_key", ssh.FingerprintSHA256(sshSrv.HostKey.PublicKey()))
	}
	bus.Publish(ctx, store.Event{Type: events.ServerStarted, Data: map[string]any{"version": info.Version}})
	if opts.Ready != nil {
		opts.Ready(addrs)
	}

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-served:
		runErr = fmt.Errorf("HTTP server: %w", err)
	}
	log.Info("moorage server stopping")
	bus.Close() // those following it would otherwise hold up the shutdown
	if sshSrv != nil {
		sshSrv.Close()
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(shutdown); err != nil {
			log.Warn("a listener did not shut down cleanly", "err", err)
		}
	}
	log.Info("moorage server stopped")
	return runErr
}

func newHTTPServer(h http.Handler, errorLog *stdlog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          errorLog,
	}
}

func addrString(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}

func listenError(addr string, err error) error {
	msg := fmt.Sprintf("listen on %s: %v", addr, err)
	switch {
	case platform.AccessDenied(err):
		msg += "\n  ports below 1024 need extra privileges: run as root or grant CAP_NET_BIND_SERVICE (Linux), or use a port such as 8080"
	case platform.AddressInUse(err):
		msg += "\n  another program is already listening there"
	}
	return errors.New(msg)
}

// containerSource is where this server gets its container engine from, when something wants one. Without one, apps that need
// containers cannot be deployed, and everything else goes on.
func containerSource(ctx context.Context, cfg config.Config, db *store.DB, keyring *secrets.Keyring, kept time.Duration, log *slog.Logger) *container.Source {
	prefer := recordedEngine(ctx, db, log)
	return container.NewSource(container.SourceOptions{
		Setting: cfg.Containers.Engine, Prefer: prefer, Kept: kept, Log: log,
		Prepare: func(engine *container.Engine) {
			engine.TmpDir = filepath.Join(cfg.DataDir, "tmp")
			engine.Auth = func(ctx context.Context) ([]byte, error) {
				registries, err := db.Registries(ctx)
				if err != nil {
					return nil, err
				}
				creds := make([]container.Credential, 0, len(registries))
				for _, r := range registries {
					password, err := keyring.OpenRegistry(r.Host, r.Sealed)
					if err != nil {
						return nil, err
					}
					creds = append(creds, container.Credential{Host: r.Host, Username: r.Username, Password: password})
				}
				return container.AuthFile(creds), nil
			}
		},
		NewDriver: func(engine *container.Engine) *container.Driver {
			return container.New(engine, ContainerLabel(cfg), log)
		},
		Pin: func(bin string) {
			if err := db.PutSetting(context.WithoutCancel(ctx), settingContainerEngine, bin, "moorage"); err != nil {
				log.Warn("record which container engine this server uses", "err", err)
			}
		},
	})
}

// recordedEngine is the engine that what is here was made with. It counts only while something still needs it, as an image is in the store of the engine that made
// it. Otherwise the server is free to take whichever answers.
func recordedEngine(ctx context.Context, db *store.DB, log *slog.Logger) string {
	wanted, err := deploy.RunsInContainers(ctx, db)
	if err != nil {
		log.Warn("read what runs in containers", "err", err)
		return ""
	}
	if !wanted {
		return ""
	}
	var prefer string
	if _, err := db.Setting(ctx, settingContainerEngine, &prefer); err != nil {
		log.Warn("read which container engine this server used", "err", err)
	}
	return prefer
}

// settingContainerEngine records the engine that a server's containers run under, as they stay with it.
const settingContainerEngine = "container_engine"

// ContainerLabel tells this server's containers from any other's on the same
// engine: each manages only its own.
func ContainerLabel(cfg config.Config) string {
	sum := sha256.Sum256([]byte(cfg.DataDir))
	return hex.EncodeToString(sum[:6])
}
