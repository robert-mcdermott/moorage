package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/testroles"
)

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// sourceManifest is a command-runtime app whose build and release commands
// use only the embedded shell's builtins, so they run on every OS, and
// whose web process is the test binary.
func sourceManifest(body, build, release string) string {
	return `[app]
runtime = "command"

[build]
command = ` + tomlString(build) + `
exclude = ["tests/"]

[processes.web]
command = ` + tomlString(testroles.AppCommand()) + `
health = { path = "/health", interval = "100ms", startup = "30s" }

[env]
TESTAPP_BODY = ` + tomlString(body) + `

[release]
command = ` + tomlString(release) + `
on_create = 'echo seeded > "$MOORAGE_DATA_DIR/seeded"'

[deploy]
drain = "1s"
keep_releases = 1
`
}

const (
	okBuild   = `echo "built $MOORAGE_BUILD_ID" > built.txt`
	okRelease = `echo migrated > "$MOORAGE_DATA_DIR/migrated-$MOORAGE_RELEASE"`
)

// deploySource uploads a source tree and follows its deployment to the end.
func deploySource(t *testing.T, c *client.Client, app string, files map[string]string) (apitypes.Deployment, string) {
	t.Helper()
	dir := writeSite(t, files)
	queued, _, err := c.DeploySource(context.Background(), app, "production", dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != "queued" && queued.Status != "building" || queued.ID == "" {
		t.Errorf("queued deployment: %+v", queued)
	}
	var log strings.Builder
	d, err := c.FollowDeployment(context.Background(), queued.ID, func(l apitypes.LogLine) error {
		log.WriteString(l.Stream + " " + l.Text + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, log.String()
}

func TestSourceDeploy(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	dataDir := filepath.Join(cfg.DataDir, "data", "shop", "production")

	d, log := deploySource(t, c, "shop", map[string]string{
		"moorage.toml": sourceManifest("source v1", okBuild, okRelease),
		"tests/x.txt":  "left out of the build",
	})
	if d.Status != "live" || d.Release == nil || d.Release.Version != 1 {
		t.Fatalf("deployment = %+v\n%s", d, log)
	}
	for _, want := range []string{"M ▸ detected   command", "M ▸ build      echo", "M ▸ release    v1: echo migrated", "M ▸ rollout    web ×1", "M ✔ live       v1"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "source v1" {
		t.Errorf("served %q", body)
	}
	appDir := filepath.Join(cfg.DataDir, "builds", "shop", d.Build, "app")
	built, err := os.ReadFile(filepath.Join(appDir, "built.txt"))
	if err != nil || !strings.Contains(string(built), d.Build) {
		t.Errorf("built.txt: %q %v", built, err)
	}
	if fi, err := os.Stat(filepath.Join(appDir, "built.txt")); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm()&0o222 != 0 {
		t.Errorf("the build is writable: %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(appDir, "tests")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("build.exclude did not remove tests/: %v", err)
	}
	for _, f := range []string{"migrated-v1", "seeded"} {
		if _, err := os.Stat(filepath.Join(dataDir, f)); err != nil {
			t.Errorf("the release commands did not run: %v", err)
		}
	}
	os.Remove(filepath.Join(dataDir, "seeded"))

	// A second deploy: the release command runs again, on_create does not;
	// with keep_releases = 1 the first build is removed.
	d2, log := deploySource(t, c, "shop", map[string]string{"moorage.toml": sourceManifest("source v2", okBuild, okRelease)})
	if d2.Status != "live" || d2.Release.Version != 2 {
		t.Fatalf("second deployment = %+v\n%s", d2, log)
	}
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "source v2" {
		t.Errorf("served %q", body)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "migrated-v2")); err != nil {
		t.Errorf("the release command did not run again: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "seeded")); err == nil {
		t.Error("on_create ran again")
	}
	if _, err := os.Stat(appDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the first build was not collected: %v", err)
	}
	_, err = c.Rollback(context.Background(), "shop", "production", 1)
	if err == nil || !strings.Contains(err.Error(), "removed") {
		t.Errorf("rollback to a collected build: %v", err)
	}

	// A failing build and a failing release command change nothing.
	for _, tc := range []struct{ build, release, want string }{
		{"echo compiling && exit 3", okRelease, "exited with status 3"},
		{okBuild, "exit 4", "the release command failed"},
	} {
		d, log := deploySource(t, c, "shop", map[string]string{"moorage.toml": sourceManifest("broken", tc.build, tc.release)})
		if d.Status != "failed" || !strings.Contains(d.Error, tc.want) {
			t.Errorf("deployment = %+v, want a failure mentioning %q\n%s", d, tc.want, log)
		}
		if !strings.Contains(log, "M ✘ failed") {
			t.Errorf("the log does not say it failed:\n%s", log)
		}
		if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "source v2" {
			t.Errorf("after a failed deploy, served %q", body)
		}
	}
	rels, err := c.Releases(context.Background(), "shop", "production")
	if err != nil || len(rels) != 2 || !rels[0].Current || rels[0].Version != 2 {
		t.Errorf("releases after failures: %+v %v", rels, err)
	}

	// Nothing to go on: the deployment fails and says what to add.
	d, _ = deploySource(t, c, "shop", map[string]string{"notes.txt": "hello"})
	if d.Status != "failed" || !strings.Contains(d.Error, "cannot tell how to build") {
		t.Errorf("undetectable app: %+v", d)
	}

	deps, err := c.Deployments(context.Background(), "shop", "production", 10)
	if err != nil || len(deps) != 5 {
		t.Fatalf("deployments: %d %v", len(deps), err)
	}
	if deps[0].Status != "failed" || deps[4].Status != "live" || deps[4].Source != "source-upload" {
		t.Errorf("deployments: %+v", deps)
	}
	// The failed build's record keeps its last output.
	got, err := c.Deployment(context.Background(), deps[2].ID)
	if err != nil || len(got.Output) != 1 || got.Output[0] != "compiling" {
		t.Errorf("deployment %s: %+v %v", deps[2].ID, got, err)
	}

	// An empty upload is refused before anything is created.
	if _, _, err := c.DeploySource(context.Background(), "empty-app", "production", writeSite(t, map[string]string{}), nil); err == nil {
		t.Error("an empty upload was accepted")
	}
	if apps, _ := c.Apps(context.Background()); len(apps) != 1 {
		t.Errorf("apps: %+v", apps)
	}
}

func TestNewerDeploySupersedesQueued(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	submit := func(body string, build string) apitypes.Deployment {
		t.Helper()
		dir := writeSite(t, map[string]string{"moorage.toml": sourceManifest(body, build, okRelease)})
		d, _, err := c.DeploySource(context.Background(), "queue", "production", dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	first := submit("first", testroles.SleepCommand(time.Second))
	second := submit("second", okBuild)
	third := submit("third", okBuild)

	wait := func(d apitypes.Deployment) apitypes.Deployment {
		t.Helper()
		got, err := c.FollowDeployment(context.Background(), d.ID, func(apitypes.LogLine) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if d := wait(first); d.Status != "live" {
		t.Errorf("first: %+v", d)
	}
	if d := wait(second); d.Status != "superseded" || !strings.Contains(d.Error, third.ID) {
		t.Errorf("second: %+v", d)
	}
	if d := wait(third); d.Status != "live" || d.Release.Version != 2 {
		t.Errorf("third: %+v", d)
	}
	if _, body := get(t, addrs.HTTP, "queue.localhost", "/"); body != "third" {
		t.Errorf("served %q", body)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "builds", "queue", second.Build)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the superseded upload was kept: %v", err)
	}
}
