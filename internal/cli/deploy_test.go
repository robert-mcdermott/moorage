package cli

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/server"
	"github.com/robert-mcdermott/moorage/internal/testroles"
)

// The test binary plays the shims, the shell and the apps.
func TestMain(m *testing.M) {
	testroles.Run()
	os.Exit(m.Run())
}

// startLocalServer runs a server configured as the one "on this machine"
// (through MOORAGE_CONFIG) and returns its HTTP address.
func startLocalServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Preset("local", config.PresetOptions{DataDir: filepath.Join(dir, "data")})
	if err != nil {
		t.Fatal(err)
	}
	// The CLI finds the server through its configuration, so the port must
	// be known before it starts.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfg.Listen.HTTP = addr
	path := filepath.Join(dir, "server.toml")
	if err := config.Write(path, cfg, "local", false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOORAGE_CONFIG", path)
	for _, v := range []string{"MOOR_SERVER", "MOOR_TOKEN", "MOOR_APP", "MOOR_CA_CERT"} {
		t.Setenv(v, "")
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), server.Options{
			Ready:    func(server.Addrs) { close(ready) },
			ShimEnv:  testroles.ShimEnv,
			ShellEnv: testroles.ShellEnv,
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("server did not start: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("server did not start in time")
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return addr
}

func fetch(t *testing.T, addr, host, path string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	req.Host = host
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return string(body)
}

func TestDeployStaticEndToEnd(t *testing.T) {
	addr := startLocalServer(t)
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, "index.html"), []byte("version one"), 0o644)

	code, out, errOut := run(t, "deploy", "--static", site, "--app", "docs")
	if code != 0 {
		t.Fatalf("deploy: %s", errOut)
	}
	for _, want := range []string{"Created app docs.", "Deployed docs v1: 1 files", "http://docs.localhost:"} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy output lacks %q:\n%s", want, out)
		}
	}
	if body := fetch(t, addr, "docs.localhost", "/"); body != "version one" {
		t.Errorf("served %q", body)
	}

	os.WriteFile(filepath.Join(site, "index.html"), []byte("version two"), 0o644)
	if code, out, errOut = run(t, "deploy", "--static", site, "--app", "docs"); code != 0 || !strings.Contains(out, "Deployed docs v2") {
		t.Fatalf("second deploy: %d %s %s", code, out, errOut)
	}

	code, out, _ = run(t, "apps")
	if code != 0 || !strings.Contains(out, "docs") || !strings.Contains(out, "v2") {
		t.Errorf("apps: %d\n%s", code, out)
	}
	code, out, _ = run(t, "releases", "--app", "docs")
	if code != 0 || !strings.Contains(out, "*   v2") || !strings.Contains(out, "v1") {
		t.Errorf("releases: %d\n%s", code, out)
	}

	if code, out, errOut = run(t, "rollback", "--app", "docs"); code != 0 || !strings.Contains(out, "docs now serves v3") {
		t.Fatalf("rollback: %d %s %s", code, out, errOut)
	}
	if body := fetch(t, addr, "docs.localhost", "/"); body != "version one" {
		t.Errorf("after rollback served %q", body)
	}
	if code, _, errOut = run(t, "rollback", "v42", "--app", "docs"); code != exitError || !strings.Contains(errOut, "no release v42") {
		t.Errorf("rollback to a missing release: %d %s", code, errOut)
	}
}

func TestDeployNeedsStaticAndApp(t *testing.T) {
	startLocalServer(t)
	if code, _, errOut := run(t, "deploy", t.TempDir(), "--app", "docs"); code != exitUsage || !strings.Contains(errOut, "--static") {
		t.Errorf("deploy without --static: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "deploy", "--static", t.TempDir()); code != exitError || !strings.Contains(errOut, "which app") {
		t.Errorf("deploy without an app: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "apps", "--server", "http://127.0.0.1:1", "--token", "moor_x"); code != exitError || !strings.Contains(errOut, "cannot reach") {
		t.Errorf("unreachable server: %d %s", code, errOut)
	}
}

func TestDeployArtifactEndToEnd(t *testing.T) {
	addr := startLocalServer(t)
	project := t.TempDir()
	command := strings.NewReplacer(`\\`, `\\\\`, `"`, `\\"`).Replace(testroles.AppCommand())
	manifestText := "[app]\nname = \"shop\"\n\n[processes.web]\ncommand = \"" + command + "\"\nhealth = { path = \"/health\" }\n"
	os.WriteFile(filepath.Join(project, "moorage.toml"), []byte(manifestText), 0o644)
	os.WriteFile(filepath.Join(project, "README"), []byte("the app is the test binary"), 0o644)
	t.Chdir(project)

	code, out, errOut := run(t, "deploy", "--artifact")
	if code != 0 || !strings.Contains(out, "Deployed shop v1") {
		t.Fatalf("deploy --artifact: %d %s %s", code, out, errOut)
	}
	if body := fetch(t, addr, "shop.localhost", "/"); body != "hello from v1" {
		t.Errorf("served %q", body)
	}
	code, out, _ = run(t, "ps")
	if code != 0 || !strings.Contains(out, "web.1") || !strings.Contains(out, "running") || !strings.Contains(out, "healthy") {
		t.Errorf("ps: %d\n%s", code, out)
	}
	code, out, _ = run(t, "logs", "-n", "20")
	if code != 0 || !strings.Contains(out, "web.1   listening on") || !strings.Contains(out, "web.1 * started (pid") {
		t.Errorf("logs: %d\n%s", code, out)
	}
	code, out, errOut = run(t, "restart")
	if code != 0 || !strings.Contains(out, "Restarted shop.") || !strings.Contains(out, "web.1") {
		t.Errorf("restart: %d %s %s", code, out, errOut)
	}
	if code, _, errOut = run(t, "deploy", "--artifact", "--static"); code != exitUsage {
		t.Errorf("deploy with both modes: %d %s", code, errOut)
	}
}
