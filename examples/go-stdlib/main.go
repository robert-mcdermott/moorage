// Links is a small link shortener: a page to add links, and short
// addresses that redirect to them and count the visits. It uses only Go's
// standard library.
package main

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// The page and its stylesheet are built into the binary.
//
//go:embed templates static
var files embed.FS

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("links stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// Moorage sets HOST and PORT for the web process, and MOORAGE_DATA_DIR
	// for data that survives deploys. Locally: 127.0.0.1:8080 and ./data.
	addr := net.JoinHostPort(env("HOST", "127.0.0.1"), env("PORT", "8080"))
	store, err := openStore(filepath.Join(env("MOORAGE_DATA_DIR", "data"), "links.jsonl"))
	if err != nil {
		return err
	}
	static, _ := fs.Sub(files, "static")
	a := &app{
		store:   store,
		logger:  logger,
		page:    template.Must(template.ParseFS(files, "templates/index.html")),
		static:  http.FileServerFS(static),
		release: env("MOORAGE_RELEASE", "local"),
	}
	srv := &http.Server{Addr: addr, Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}

	// Moorage stops an instance with SIGTERM once traffic has moved to the
	// new release, and kills it if it is still running after stop_timeout
	// (30 seconds): finish the requests in flight, then exit.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	logger.Info("listening", "addr", addr, "release", a.release)

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	logger.Info("stopping")
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
