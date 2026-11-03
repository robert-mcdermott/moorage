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
	"github.com/robert-mcdermott/moorage/internal/testroles"
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

// The test binary plays the shims, the shell and the apps.
func TestMain(m *testing.M) {
	testroles.Run()
	os.Exit(m.Run())
}

// testOptions makes the server run its helpers from the test binary, with
// short delays.
func testOptions(ready func(Addrs)) Options {
	return Options{
		Ready:      ready,
		ShimEnv:    testroles.ShimEnv,
		ShellEnv:   testroles.ShellEnv,
		BackoffMin: 50 * time.Millisecond,
		Tick:       200 * time.Millisecond,
	}
}

// start runs a server and returns its addresses and a function that stops
// it and returns Run's error.
func start(t *testing.T, cfg config.Config) (Addrs, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan Addrs, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, quiet, testOptions(func(a Addrs) { ready <- a }))
	}()
	select {
	case addrs := <-ready:
		var stopped bool
		var stopErr error
		stop := func() error {
			if stopped {
				return stopErr
			}
			stopped = true
			cancel()
			select {
			case stopErr = <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("server did not stop")
			}
			return stopErr
		}
		t.Cleanup(func() { stop() })
		return addrs, stop
	case err := <-done:
		cancel()
		t.Fatalf("server did not start: %v", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("server did not start in time")
	}
	return Addrs{}, nil
}

// get requests path from addr with the given Host header.
func get(t *testing.T, addr net.Addr, host, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr.String()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

func TestRunServesHealthAndPages(t *testing.T) {
	cfg := testConfig(t)
	addrs, stop := start(t, cfg)

	res, body := get(t, addrs.HTTP, "anything.example.com", "/.moorage/health")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" {
		t.Errorf("health: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var h struct{ Status, Version string }
	if err := json.Unmarshal([]byte(body), &h); err != nil || h.Status != "ok" || h.Version != buildinfo.Version {
		t.Errorf("health = %q (%v)", body, err)
	}

	cases := []struct {
		host, want string
		code       int
	}{
		{"moorage.localhost", "Moorage is running", 200},
		{"127.0.0.1", "Moorage is running", 200},
		{"nope.localhost", "There is no app named", 404},
		{"example.org", "Not found", 404},
	}
	for _, c := range cases {
		res, body := get(t, addrs.HTTP, c.host, "/")
		if res.StatusCode != c.code || !strings.Contains(body, c.want) {
			t.Errorf("Host %s: %d, body lacks %q", c.host, res.StatusCode, c.want)
		}
	}

	res, body = get(t, addrs.HTTP, "127.0.0.1", "/api/v1/apps")
	if res.StatusCode != http.StatusUnauthorized || res.Header.Get("Content-Type") != "application/problem+json" || !strings.Contains(body, "token") {
		t.Errorf("API without a token: %d %q %s", res.StatusCode, res.Header.Get("Content-Type"), body)
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
	for _, p := range []string{l.Database, l.Keys, l.Repos, l.Builds, l.AppData, l.Logs, l.Toolchains, l.Cache, l.Certs, l.Run, l.Tmp, l.Backups, l.AdminToken} {
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
		if fi, err := os.Stat(l.AdminToken); err == nil && fi.Mode().Perm() != 0o600 {
			t.Errorf("admin token has mode %v", fi.Mode().Perm())
		}
	}
}

func TestRunRefusesSecondServer(t *testing.T) {
	cfg := testConfig(t)
	start(t, cfg)
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
	token1, _ := os.ReadFile(NewLayout(cfg.DataDir).AdminToken)
	// A second start finds the schema and the token in place.
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
	if token2, _ := os.ReadFile(NewLayout(cfg.DataDir).AdminToken); string(token1) != string(token2) {
		t.Error("the admin token changed on restart")
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

func TestRunRefusesPathRouting(t *testing.T) {
	cfg := testConfig(t)
	cfg.Routing = "path"
	if err := Run(context.Background(), cfg, quiet, Options{}); err == nil || !strings.Contains(err.Error(), "path") {
		t.Errorf("path routing: %v", err)
	}
}
