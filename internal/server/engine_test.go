// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// recordingEngine is a program that writes down everything it is asked, and answers as a daemon that is down does if down exists.
func recordingEngine(t *testing.T) (bin, down, calls string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	dir := t.TempDir()
	bin, down, calls = filepath.Join(dir, "podman"), filepath.Join(dir, "down"), filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >> "` + calls + `"
case "$1" in
  --version) echo "podman version 5" ;;
  *) if [ -e "` + down + `" ]; then echo "Cannot connect to Podman" >&2; exit 125; fi ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, down, calls
}

// TestAnAppThatNeedsNoEngineAsksForNone is the promise: Podman and Docker are for those who use them. A server asks once as it starts, and after that an app
// with no containers never causes it to be asked again, whatever is done to the app.
func TestAnAppThatNeedsNoEngineAsksForNone(t *testing.T) {
	bin, _, calls := recordingEngine(t)
	cfg := testConfig(t)
	cfg.Containers.Engine = bin
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	asked := func() string {
		data, _ := os.ReadFile(calls)
		return string(data)
	}
	// As it starts, it asks how the engine stands, and what is left of this server's containers.
	waitUntil(t, "what it asks as it starts", func() bool { return strings.Contains(asked(), "ps -aq") })
	atStart := asked()

	// All of what an app goes through.
	deployedSource(t, c, "shop", appManifest(""))
	if _, err := c.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "X", Value: "y"}}}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the second release", func() bool {
		rels, err := c.Releases(ctx, "shop", "production")
		return err == nil && len(rels) >= 2
	})
	if _, err := c.Restart(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rollback(ctx, "shop", "production", 1); err != nil {
		t.Fatal(err)
	}
	get(t, addrs.HTTP, "shop.localhost", "/")
	if _, err := c.Instances(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	deploySite(t, c, "docs")
	time.Sleep(time.Second) // several rounds of what keeps instances running
	if got := asked(); got != atStart {
		t.Errorf("it asked the engine for an app with no containers:\n%s", strings.TrimPrefix(got, atStart))
	}
}

func TestWhatNeedsAnEngineSaysWhatToDo(t *testing.T) {
	bin, down, calls := recordingEngine(t)
	if err := os.WriteFile(down, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Containers.Engine = bin
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()

	// A server with a stopped engine goes on, and so does an app that needs none.
	deployedSource(t, c, "shop", appManifest(""))
	waitUntil(t, "the state to be known", func() bool {
		info, err := c.ServerInfo(ctx)
		return err == nil && info.Containers.State == "not answering"
	})
	info, err := c.ServerInfo(ctx)
	if err != nil || info.Containers.Kind != "podman" || !strings.Contains(info.Containers.Problem, "podman machine start") || !strings.Contains(info.Containers.Reason, "Cannot connect") {
		t.Errorf("%+v %v", info.Containers, err)
	}

	// What needs it fails before it builds anything, and says why.
	d, _ := deploySource(t, c, "boxed", map[string]string{"Containerfile": "FROM x\n"})
	if d.Status != "failed" || !strings.Contains(d.Error, "not answering") || !strings.Contains(d.Error, "podman machine start") {
		t.Errorf("a Containerfile: %+v", d)
	}
	if data, _ := os.ReadFile(calls); strings.Contains(string(data), "build") {
		t.Errorf("it tried to build:\n%s", data)
	}
	_, _, err = c.DeployImage(ctx, "boxed", "production", "example.com/a:1", t.TempDir(), nil)
	if status(err) != http.StatusConflict || !strings.Contains(err.Error(), "not answering") {
		t.Errorf("an image: %v", err)
	}

	// Starting it needs no restart, and it says so as soon as it next looks.
	if err := os.Remove(down); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "it to notice", func() bool {
		info, err := c.ServerInfo(ctx)
		return err == nil && info.Containers.State == "ready" && info.Containers.Problem == ""
	})
}

func TestNoEngineAtAll(t *testing.T) {
	cfg := testConfig(t)
	cfg.Containers.Engine = "no-such-engine"
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	info, err := c.ServerInfo(ctx)
	if err != nil || info.Containers.State != "absent" || !strings.Contains(info.Containers.Problem, "Nothing else needs one") {
		t.Errorf("%+v %v", info.Containers, err)
	}
	_, _, err = c.DeployImage(ctx, "boxed", "production", "example.com/a:1", t.TempDir(), nil)
	if status(err) != http.StatusConflict {
		t.Errorf("an image: %v", err)
	}
	deployedSource(t, c, "shop", appManifest(""))
	owner := signIn(t, cfg, addrs)
	_, page := owner.get("/apps/shop/deploy")
	contains(t, "the deploy page", page, "no container engine", "Nothing else")
	if strings.Contains(page, `action="/apps/shop/deploy/image`) {
		t.Errorf("it offers to deploy an image:\n%s", page)
	}
}

// TestWhichEngineAServerKeepsTo: what was recorded counts while something here needs it, which includes a current release with nothing running, as when it is asleep.
func TestWhichEngineAServerKeepsTo(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	apiClient(t, cfg, addrs)
	db := testAuth(t, cfg).DB
	log := slog.New(slog.DiscardHandler)
	if err := db.PutSetting(ctx, settingContainerEngine, "/opt/bin/docker", "moorage"); err != nil {
		t.Fatal(err)
	}
	if got := recordedEngine(ctx, db, log); got != "" {
		t.Errorf("with nothing here needing it: %q", got)
	}
	appID, _, err := db.EnsureApp(ctx, "boxed")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBuild(ctx, store.Build{ID: "bld_boxed", AppID: appID, SourceType: "test", ArtifactKind: "image", ArtifactRef: "example/app", Plan: json.RawMessage(`{"kind":"process","container":{"image":"example/app"}}`), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Publish(ctx, "boxed", "production", "bld_boxed", "deploy", "admin"); err != nil {
		t.Fatal(err)
	}
	if got := recordedEngine(ctx, db, log); got != "/opt/bin/docker" {
		t.Errorf("with a current release in a container: %q", got)
	}
}

// TestServicesWithAnEngineThatIsDown goes through what a service looks like to someone, and how it fails, without needing an engine to run one.
func TestServicesWithAnEngineThatIsDown(t *testing.T) {
	bin, down, calls := recordingEngine(t)
	cfg := testConfig(t)
	cfg.Containers.Engine = bin
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(""))
	b := signIn(t, cfg, addrs)

	// With none, there is no tab for them, and the page says how to make one.
	_, page := b.get("/apps/shop")
	if strings.Contains(page, "/apps/shop/services") {
		t.Error("a tab for services that it does not have")
	}
	_, page = b.get("/apps/shop/services")
	contains(t, "the page with none", page, "[services.db]", "has no services")
	if list, err := c.Services(ctx, "shop", "production"); err != nil || len(list) != 0 {
		t.Errorf("%+v %v", list, err)
	}

	// A manifest that asks for one, with an engine that does not answer, says so before anything is built.
	if err := os.WriteFile(down, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d, log := deploySource(t, c, "shop", appManifest("\n[services.db]\ntype = \"postgres\"\n"))
	if d.Status != "failed" || !strings.Contains(d.Error, "asks for db under [services]") || !strings.Contains(d.Error, "not answering") {
		t.Errorf("%+v\n%s", d, log)
	}
	if strings.Contains(log, "▸ build") {
		t.Errorf("it built something first:\n%s", log)
	}
	// A variable of that name on production itself needs nothing of the engine.
	if _, err := c.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "DATABASE_URL", Value: "postgres://real.example.com/db"}}}); err != nil {
		t.Fatal(err)
	}
	d, log = deploySource(t, c, "shop", appManifest("\n[services.db]\ntype = \"postgres\"\n"))
	if d.Status != "live" {
		t.Errorf("with its own database: %+v\n%s", d, log)
	}
	if _, got := get(t, addrs.HTTP, "shop."+cfg.Domain, "/env?name=DATABASE_URL"); !strings.Contains(got, "real.example.com") {
		t.Errorf("its own: %q", got)
	}

	// A preview of a branch with no services takes nothing of the sort from production, as what production names says what that is.
	if _, err := testAuth(t, cfg).DB.CreateEnvironment(ctx, "shop", "old-branch", "preview", "old-branch"); err != nil {
		t.Fatal(err)
	}
	branch, _, err := c.DeploySource(ctx, "shop", "old-branch", writeSite(t, appManifest("# from before\n")), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the branch", func() bool {
		d, err := c.Deployment(ctx, branch.ID)
		return err == nil && d.Finished()
	})
	if _, got := get(t, addrs.HTTP, "shop--old-branch."+cfg.Domain, "/env?name=DATABASE_URL"); strings.Contains(got, "real.example.com") {
		t.Errorf("a preview with production's database: %q", got)
	}

	// One on record, with the engine down.
	db := testAuth(t, cfg).DB
	if err := db.CreateService(ctx, store.Service{
		ID: "svc_test", App: "shop", Env: "production", Name: "stats", Type: "postgres", Version: "18", Image: "postgres:18", Engine: bin,
		Volume: "a-volume", Port: 45999, Sealed: []byte("x"), EnvVar: "STATS_URL", State: store.ServiceRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	list, err := c.Services(ctx, "shop", "production")
	if err != nil || len(list) != 1 || !list[0].Unknown || !list[0].Unused || list[0].Variable != "STATS_URL" {
		t.Errorf("%+v %v", list, err)
	}
	_, page = b.get("/apps/shop/services")
	contains(t, "the page", page, "stats", "postgres 18", "STATS_URL", "The engine is not answering", "The manifest no longer names it", "no replica")
	_, page = b.get("/apps/shop")
	contains(t, "its tab", page, "/apps/shop/services")
	if _, err := c.ServiceLog(ctx, "shop", "production", "stats", 10); status(err) != http.StatusConflict {
		t.Errorf("its log: %v", err)
	}
	if err := c.RemoveService(ctx, "shop", "production", "stats"); status(err) != http.StatusConflict {
		t.Errorf("removing it: %v", err)
	}
	if err := c.DeleteApp(ctx, "shop", false); status(err) != http.StatusConflict {
		t.Errorf("deleting its app: %v", err)
	}
	if err := c.RemoveService(ctx, "shop", "production", "nothing"); status(err) != http.StatusNotFound {
		t.Errorf("one that is not there: %v", err)
	}
	if _, err := db.Service(ctx, "shop", "production", "stats"); err != nil {
		t.Errorf("its record: %v", err)
	}

	// The doctor says so, and a limited token has no business with any of it.
	api := caller{t, cfg, addrs}
	admin, err := os.ReadFile(filepath.Join(cfg.DataDir, "keys", "admin-token"))
	if err != nil {
		t.Fatal(err)
	}
	var checks []apitypes.Check
	if err := json.Unmarshal([]byte(api.read(strings.TrimSpace(string(admin)), "/server/doctor")), &checks); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, check := range checks {
		if check.Name == "services" {
			found = true
			if check.Status != "warn" || !strings.Contains(check.Detail, "not answering") {
				t.Errorf("%+v", check)
			}
		}
	}
	if !found {
		t.Errorf("the doctor: %+v", checks)
	}
	made, err := c.CreateToken(ctx, apitypes.NewToken{Name: "agent", Scopes: []string{"deploy"}, App: "shop", Envs: "previews"})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := api.do(made.Secret, "GET", "/api/v1/apps/shop/envs/production/services", ""); code != http.StatusForbidden {
		t.Errorf("a limited token: %d %s", code, body)
	}

	// Once it answers, the server looks after it again.
	os.Remove(down)
	waitUntil(t, "it to be looked at", func() bool {
		data, _ := os.ReadFile(calls)
		return strings.Contains(string(data), "moorage-svc-test")
	})
}
