// Package server runs a Moorage server: it prepares the data directory,
// takes the directory's lock, opens and migrates the database, and serves
// apps, platform endpoints and the API until its context is cancelled.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/robert-mcdermott/moorage/internal/api"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/build"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
	consolepages "github.com/robert-mcdermott/moorage/internal/console"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/gitserver"
	"github.com/robert-mcdermott/moorage/internal/ingress"
	"github.com/robert-mcdermott/moorage/internal/layout"
	"github.com/robert-mcdermott/moorage/internal/localdns"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/secrets"
	"github.com/robert-mcdermott/moorage/internal/store"
	"github.com/robert-mcdermott/moorage/internal/toolchain"
	"github.com/robert-mcdermott/moorage/internal/truststore"
)

// Layout names the files and directories inside a data directory.
type Layout = layout.Layout

// NewLayout returns the layout of the data directory root.
func NewLayout(root string) Layout { return layout.New(root) }

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
	if push && !access.Deploy || !push && !access.See {
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
	if cfg.Routing == "path" {
		return errors.New(`routing = "path" is not available yet (it arrives with path routing in wave 4); use routing = "host"`)
	}
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
	toolchains := toolchain.New(filepath.Join(cfg.DataDir, "toolchains"), filepath.Join(cfg.DataDir, "cache"), toolchain.Sources{
		Go: cfg.Toolchains.GoURL, Node: cfg.Toolchains.NodeURL, UV: cfg.Toolchains.UVURL, Python: cfg.Toolchains.PythonURL,
	})
	shell := []string{exe, "sh", "-c"}
	engine, containers := findContainers(cfg, db, keyring, log)
	bus := events.New(db, log)
	defer bus.Close()
	go bus.Prune(ctx, 24*time.Hour)
	runtime := reconcile.New(db, cfg, reconcile.Options{
		DataDir:       cfg.DataDir,
		ShimCmd:       []string{exe, "shim"},
		ShimEnv:       opts.ShimEnv,
		Shell:         shell,
		ShellEnv:      opts.ShellEnv,
		BackoffMin:    opts.BackoffMin,
		Tick:          opts.Tick,
		Trusted:       router.Trusted(),
		ToolchainBins: toolchains.BinDirs,
		Secrets:       keyring,
		Containers:    containers,
		Events:        bus,
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
	go pruneAudit(ctx, db, log)
	repos := gitserver.NewRepos(layout.Repos)
	deployer := deploy.New(db, deploy.Options{
		DataDir: cfg.DataDir,
		Builder: &build.Builder{DataDir: cfg.DataDir, Toolchains: toolchains, Shell: shell, ShellEnv: opts.ShellEnv, Engine: engine},
		Runtime: runtime,
		Reload:  router.Reload,
		URL: func(app, env string) string {
			return cfg.PublicURL(app + "." + cfg.Domain)
		},
		GCMinAge:  opts.GCMinAge,
		Secrets:   keyring,
		Events:    bus,
		Repos:     repos,
		ForgetApp: router.ForgetApp,
	}, log)
	// Running builds are stopped (and recorded as failed) before the
	// database closes.
	defer deployer.Shutdown()
	authn := &auth.Service{
		DB: db, LocalToken: tokenHash, Trusted: router.Trusted(), Secure: cfg.Proxy.ExternalScheme == "https",
		ClaimFile: layout.ClaimCode, Limits: auth.NewLimiter(), Log: log,
	}
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
	console.Handle("/api/", api.New(db, cfg, cfg.DataDir, authn, runtime, deployer, keyring, bus, log))
	console.Handle("/git/", &gitserver.HTTP{Git: git, Authorize: func(r *http.Request, password, app string, push bool) (string, error) {
		return authorizeGit(r, authn, password, app, push)
	}})
	pages, err := consolepages.New(consolepages.Options{Auth: authn, DB: db, Config: cfg, Log: log, Deployer: deployer, Runtime: runtime, Keyring: keyring, Dir: opts.ConsoleDir})
	if err != nil {
		return fmt.Errorf("the console: %w", err)
	}
	console.Handle("/", pages)
	router.SetConsole(console)

	var tlsConfig *tls.Config
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
		httpsSrv := newHTTPServer(router, errorLog)
		httpsSrv.TLSConfig = tlsConfig
		servers = append(servers, httpsSrv)
		go func() { served <- httpsSrv.ServeTLS(httpsLn, "", "") }()
	}
	httpSrv := newHTTPServer(httpHandler, errorLog)
	servers = append(servers, httpSrv)
	go func() { served <- httpSrv.Serve(httpLn) }()

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

// findContainers finds the container engine, if the server has one. Without one
// apps that need containers cannot be deployed, and everything else goes on.
func findContainers(cfg config.Config, db *store.DB, keyring *secrets.Keyring, log *slog.Logger) (*container.Engine, *container.Driver) {
	engine, err := container.Find(cfg.Containers.Engine)
	if errors.Is(err, container.ErrNoEngine) {
		log.Info("no container engine: apps can run natively, not in containers")
		return nil, nil
	}
	if err != nil {
		log.Warn("apps cannot run in containers", "err", err)
		return nil, nil
	}
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
	log.Info("apps can run in containers", "engine", engine.Kind, "path", engine.Bin)
	return engine, container.New(engine, ContainerLabel(cfg), log)
}

// ContainerLabel tells this server's containers from any other's on the same
// engine: each manages only its own.
func ContainerLabel(cfg config.Config) string {
	sum := sha256.Sum256([]byte(cfg.DataDir))
	return hex.EncodeToString(sum[:6])
}
