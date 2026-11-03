package cli

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
		testroles.StopInstances(cfg.DataDir) // the server leaves its apps running
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

func TestDeployChecksFlagsAndApp(t *testing.T) {
	startLocalServer(t)
	if code, _, errOut := run(t, "deploy", t.TempDir(), "--static", "--artifact", "--app", "docs"); code != exitUsage || !strings.Contains(errOut, "not both") {
		t.Errorf("deploy --static --artifact: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "deploy", t.TempDir(), "--app", "docs"); code != exitError || !strings.Contains(errOut, "no files") {
		t.Errorf("deploy of an empty directory: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "deploy", t.TempDir()); code != exitError || !strings.Contains(errOut, "which app") {
		t.Errorf("source deploy without an app: %d %s", code, errOut)
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
	command := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(testroles.AppCommand())
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

func TestDeploySourceEndToEnd(t *testing.T) {
	addr := startLocalServer(t)
	src := t.TempDir()
	quote := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace
	manifest := `[app]
name = "builder"
runtime = "command"

[build]
command = 'echo built > built.txt'

[processes.web]
command = "` + quote(testroles.AppCommand()) + `"
health = { path = "/health", interval = "100ms" }

[env]
TESTAPP_BODY = "built from source"
`
	os.WriteFile(filepath.Join(src, "moorage.toml"), []byte(manifest), 0o644)
	os.WriteFile(filepath.Join(src, ".gitignore"), []byte("secret.txt\n"), 0o644)
	os.WriteFile(filepath.Join(src, "secret.txt"), []byte("not for the server"), 0o644)
	os.MkdirAll(filepath.Join(src, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(src, "node_modules", "x", "index.js"), []byte("left out"), 0o644)

	code, out, errOut := run(t, "deploy", src)
	if code != 0 {
		t.Fatalf("deploy: %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"Created app builder.", "Uploaded 2 files", "▸ build      echo built > built.txt", "✔ live       v1"} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy output lacks %q:\n%s", want, out)
		}
	}
	if body := fetch(t, addr, "builder.localhost", "/"); body != "built from source" {
		t.Errorf("served %q", body)
	}

	code, out, _ = run(t, "deployments", "--app", "builder")
	if code != 0 || !strings.Contains(out, "source-upload") || !strings.Contains(out, "live") {
		t.Errorf("deployments: %d\n%s", code, out)
	}
	id := strings.Fields(strings.Split(out, "\n")[1])[0]
	code, out, _ = run(t, "deployments", "show", id)
	if code != 0 || !regexp.MustCompile(`Status +live`).MatchString(out) || !regexp.MustCompile(`Release +v1`).MatchString(out) {
		t.Errorf("deployments show: %d\n%s", code, out)
	}
	code, out, _ = run(t, "deployments", "follow", id)
	if code != 0 || !strings.Contains(out, "✔ live") {
		t.Errorf("deployments follow: %d\n%s", code, out)
	}

	// A failing build fails the command, with the build's output on screen.
	os.WriteFile(filepath.Join(src, "moorage.toml"), []byte(strings.Replace(manifest, "echo built > built.txt", "echo oops && exit 7", 1)), 0o644)
	code, out, errOut = run(t, "deploy", src)
	if code != 1 || !strings.Contains(out, "             oops") || !strings.Contains(errOut, "exited with status 7") {
		t.Errorf("failed deploy: %d\n%s\n%s", code, out, errOut)
	}

	code, out, _ = run(t, "deploy", src, "--detach")
	if code != 0 || !strings.Contains(out, "moor deployments follow dep_") {
		t.Errorf("detached deploy: %d\n%s", code, out)
	}
}

func TestManifestCommands(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/hello\n\ngo 1.26.1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)

	code, out, _ := run(t, "manifest", "validate", dir)
	if code != 0 || !strings.Contains(out, "There is no") {
		t.Errorf("validate without a manifest: %d %s", code, out)
	}
	code, out, _ = run(t, "manifest", "explain", dir, "--os", "linux")
	for _, want := range []string{"runtime     go", "go 1.26  (go.mod go line)", `-o "$MOORAGE_BIN/hello" .`, `web: "$MOORAGE_BIN/hello"`} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("explain lacks %q: %d\n%s", want, code, out)
		}
	}

	os.WriteFile(filepath.Join(dir, "moorage.toml"), []byte("[env]\nAPI_TOKEN = \"abc\"\n[[cron]]\nname = \"x\"\nschedule = \"@daily\"\ncommand = \"true\"\n"), 0o644)
	code, out, errOut := run(t, "manifest", "validate", dir)
	if code != 0 || !strings.Contains(out, "valid (2 warnings)") || !strings.Contains(errOut, "env.API_TOKEN looks like a credential") {
		t.Errorf("validate with warnings: %d\n%s\n%s", code, out, errOut)
	}
	os.WriteFile(filepath.Join(dir, "moorage.toml"), []byte("[app]\nruntime = \"cobol\"\n"), 0o644)
	if code, _, errOut = run(t, "manifest", "validate", dir); code != 1 || !strings.Contains(errOut, "app.runtime") {
		t.Errorf("invalid manifest: %d %s", code, errOut)
	}

	code, out, _ = run(t, "manifest", "schema")
	if code != 0 || !strings.Contains(out, `"title": "moorage.toml"`) {
		t.Errorf("schema: %d", code)
	}
}
