// Package server runs a Moorage server: it prepares the data directory,
// takes the directory's lock, opens and migrates the database, and serves
// HTTP until its context is cancelled.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Layout names the files and directories inside a data directory
// (docs/02-architecture.md, "On-disk layout").
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

// Options adjust Run.
type Options struct {
	// Ready, if set, is called once the server accepts connections, with the
	// address its HTTP listener is bound to.
	Ready func(httpAddr net.Addr)
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

	ln, err := net.Listen("tcp", cfg.Listen.HTTP)
	if err != nil {
		return listenError(cfg.Listen.HTTP, err)
	}
	srv := &http.Server{
		Handler:           newHandler(db),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	info := buildinfo.Get()
	log.Info("moorage server started",
		"version", info.Version, "commit", info.Commit,
		"config", cfg.Path, "data_dir", cfg.DataDir,
		"http", ln.Addr().String(), "domain", cfg.Domain)
	if cfg.Listen.HTTPS != "" || cfg.Listen.SSH != "" {
		log.Warn("this development build serves only the HTTP listener; HTTPS and SSH come in later slices",
			"https", cfg.Listen.HTTPS, "ssh", cfg.Listen.SSH)
	}
	if opts.Ready != nil {
		opts.Ready(ln.Addr())
	}

	select {
	case <-ctx.Done():
	case err := <-served:
		return fmt.Errorf("HTTP server: %w", err)
	}
	log.Info("moorage server stopping")
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		log.Warn("HTTP server did not shut down cleanly", "err", err)
	}
	log.Info("moorage server stopped")
	return nil
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

type health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

func newHandler(db *store.DB) http.Handler {
	mux := http.NewServeMux()
	// Health checks for load balancers and proxies answer on every hostname.
	mux.HandleFunc("GET /.moorage/health", func(w http.ResponseWriter, r *http.Request) {
		h, code := health{Status: "ok", Version: buildinfo.Version}, http.StatusOK
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Reader.PingContext(ctx); err != nil {
			h.Status, code = "database unavailable", http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(h)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Nothing is deployed at this address yet.", http.StatusNotFound)
	})
	return mux
}
