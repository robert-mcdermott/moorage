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
