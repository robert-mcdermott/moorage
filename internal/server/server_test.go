package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Preset("local", config.PresetOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen.HTTP = "127.0.0.1:0" // let the OS choose a free port
	return cfg
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// start runs a server and returns its HTTP address and a function that
// stops it and returns Run's error.
func start(t *testing.T, cfg config.Config) (string, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, quiet, Options{Ready: func(a net.Addr) { ready <- a }})
	}()
	select {
	case addr := <-ready:
		stop := func() error {
			cancel()
			select {
			case err := <-done:
				return err
			case <-time.After(15 * time.Second):
				t.Fatal("server did not stop")
				return nil
			}
		}
		return addr.String(), stop
	case err := <-done:
		cancel()
		t.Fatalf("server did not start: %v", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("server did not start in time")
	}
	return "", nil
}

func TestRunServesHealth(t *testing.T) {
	cfg := testConfig(t)
	addr, stop := start(t, cfg)

	res, err := http.Get("http://" + addr + "/.moorage/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("health status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var h health
	if err := json.NewDecoder(res.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Version != buildinfo.Version {
		t.Errorf("health = %+v", h)
	}

	// Health answers on any hostname; other paths are not found.
	req, _ := http.NewRequest("GET", "http://"+addr+"/.moorage/health", nil)
	req.Host = "anything.example.com"
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusOK {
		t.Errorf("health on another hostname: %v %v", res, err)
	} else {
		res.Body.Close()
	}
	if res, err := http.Get("http://" + addr + "/"); err != nil || res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /: %v %v", res, err)
	} else {
		res.Body.Close()
	}

	if err := stop(); err != nil {
		t.Errorf("Run returned %v after cancel", err)
	}
}

func TestRunPreparesDataDirectory(t *testing.T) {
	cfg := testConfig(t)
	_, stop := start(t, cfg)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	l := NewLayout(cfg.DataDir)
	for _, p := range []string{l.Database, l.Keys, l.Repos, l.Builds, l.AppData, l.Logs, l.Toolchains, l.Cache, l.Certs, l.Run, l.Tmp, l.Backups} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{l.Keys, l.Certs, l.Run, l.Backups} {
			fi, err := os.Stat(p)
			if err == nil && fi.Mode().Perm() != 0o700 {
				t.Errorf("%s has mode %v, want 0700", p, fi.Mode().Perm())
			}
		}
	}
}

func TestRunRefusesSecondServer(t *testing.T) {
	cfg := testConfig(t)
	_, stop := start(t, cfg)
	defer stop()
	err := Run(context.Background(), cfg, quiet, Options{})
	if err == nil || !strings.Contains(err.Error(), "another moor server") {
		t.Errorf("second server on the same data directory: %v", err)
	}
}

func TestRunRestartKeepsDatabase(t *testing.T) {
	cfg := testConfig(t)
	_, stop := start(t, cfg)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	// A second start finds the schema in place and needs no snapshot.
	_, stop = start(t, cfg)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(NewLayout(cfg.DataDir).Backups)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("restart without new migrations wrote snapshots: %v", entries)
	}
}

func TestRunReportsBusyPort(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg := testConfig(t)
	cfg.Listen.HTTP = busy.Addr().String()
	err = Run(context.Background(), cfg, quiet, Options{})
	if err == nil || !strings.Contains(err.Error(), "listen on "+cfg.Listen.HTTP) {
		t.Errorf("Run on a busy port: %v", err)
	}
}
