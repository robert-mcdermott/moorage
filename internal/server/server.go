// Package server runs a Moorage server: it prepares the data directory,
// takes the directory's lock, opens and migrates the database, and serves
// apps, platform endpoints and the API until its context is cancelled.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/robert-mcdermott/moorage/internal/api"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/build"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/ingress"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/secrets"
	"github.com/robert-mcdermott/moorage/internal/store"
	"github.com/robert-mcdermott/moorage/internal/toolchain"
)

// Layout names the files and directories inside a data directory
// (spec/02-architecture.md, "On-disk layout").
type Layout struct {
	Root       string
	Database   string
	Keys       string
	Repos      string
	Builds     string
	AppData    string
	Logs       string
	Toolchains string
	Cache      string
	Certs      string
	Run        string
	Tmp        string
	Backups    string
	LockFile   string
	// AdminToken holds the bootstrap admin token, readable only by the
	// server's user.
	AdminToken string
	// MasterKey seals the data keys of secrets (unless the key is given
	// elsewhere: see secrets.LoadMasterKey).
	MasterKey string
}

// NewLayout returns the layout of the data directory root.
func NewLayout(root string) Layout {
	at := func(elem ...string) string { return filepath.Join(append([]string{root}, elem...)...) }
	return Layout{
		Root:       root,
		Database:   at("moorage.db"),
		Keys:       at("keys"),
		Repos:      at("repos"),
		Builds:     at("builds"),
		AppData:    at("data"),
		Logs:       at("logs"),
		Toolchains: at("toolchains"),
		Cache:      at("cache"),
		Certs:      at("certs"),
		Run:        at("run"),
		Tmp:        at("tmp"),
		Backups:    at("backups"),
		LockFile:   at("run", "moor.lock"),
		AdminToken: at("keys", "admin-token"),
		MasterKey:  at("keys", "master.key"),
	}
}

// Prepare creates the data directory and its subdirectories. Those holding
// keys, certificates, runtime state, temporary files and backups are private
// to the server's user.
func (l Layout) Prepare() error {
	dirs := []struct {
		path string
		mode os.FileMode
	}{
		{l.Root, 0o755},
		{l.Repos, 0o755},
		{l.Builds, 0o755},
		{l.AppData, 0o755},
		{l.Logs, 0o755},
		{l.Toolchains, 0o755},
		{l.Cache, 0o755},
		{l.Keys, 0o700},
		{l.Certs, 0o700},
		{l.Run, 0o700},
		{l.Tmp, 0o700},
		{l.Backups, 0o700},
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
		if d.mode == 0o700 {
			// MkdirAll leaves an existing directory's mode alone.
			if err := os.Chmod(d.path, d.mode); err != nil {
				return err
			}
		}
	}
	return nil
}

// Addrs are the addresses the listeners are bound to; HTTPS is nil when
// Moorage does not terminate TLS.
type Addrs struct {
	HTTP  net.Addr
	HTTPS net.Addr
}

// Options adjust Run.
type Options struct {
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
	sealed, err := db.HasDataKeys(ctx)
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
	deployer := deploy.New(db, deploy.Options{
		DataDir: cfg.DataDir,
		Builder: &build.Builder{DataDir: cfg.DataDir, Toolchains: toolchains, Shell: shell, ShellEnv: opts.ShellEnv},
		Runtime: runtime,
		Reload:  router.Reload,
		URL: func(app, env string) string {
			return cfg.PublicURL(app + "." + cfg.Domain)
		},
		GCMinAge: opts.GCMinAge,
		Secrets:  keyring,
	}, log)
	// Running builds are stopped (and recorded as failed) before the
	// database closes.
	defer deployer.Shutdown()
	console := http.NewServeMux()
	console.Handle("/api/", api.New(db, cfg, cfg.DataDir, tokenHash, runtime, deployer, log))
	console.Handle("/", ingress.Placeholder())
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
	served := make(chan error, 2)

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

	info := buildinfo.Get()
	log.Info("moorage server started",
		"version", info.Version, "commit", info.Commit,
		"config", cfg.Path, "data_dir", cfg.DataDir, "domain", cfg.Domain,
		"http", addrs.HTTP.String(), "https", addrString(addrs.HTTPS), "tls", cfg.TLS.Mode,
		"console", cfg.PublicURL(cfg.ConsoleHost))
	if cfg.Listen.SSH != "" {
		log.Warn("git pushes over SSH arrive in wave 2; listen.ssh is not served yet", "ssh", cfg.Listen.SSH)
	}
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
	case errors.Is(err, syscall.EACCES), errors.Is(err, os.ErrPermission):
		msg += "\n  ports below 1024 need extra privileges: run as root or grant CAP_NET_BIND_SERVICE (Linux), or use a port such as 8080"
	case errors.Is(err, syscall.EADDRINUSE):
		msg += "\n  another program is already listening there"
	}
	return errors.New(msg)
}
