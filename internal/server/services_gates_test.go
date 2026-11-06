// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// S1. moor access will not open a gate over a copy, by saying public. Saying default does, where the server's own default for previews is public.
func TestLeavingAGateToTheServerDoesNotOpenIt(t *testing.T) {
	bin, _ := serviceEngine(t)
	cfg := testConfig(t)
	cfg.Containers.Engine = bin
	cfg.Access.Preview = "public"
	addrs, _ := start(t, cfg)
	client := apiClient(t, cfg, addrs)
	ctx := context.Background()
	db := testAuth(t, cfg).DB
	deployedSource(t, client, "shop", manifestWithService("", "0s"))
	if _, err := db.CreateEnvironment(ctx, "shop", "topic", store.KindPreview, "topic"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SetAccess(ctx, "shop", "topic", apitypes.AccessChange{Mode: "login"}); err != nil {
		t.Fatal(err)
	}
	made, _, err := client.DeploySource(ctx, "shop", "topic", writeSite(t, manifestWithService("", "0s")), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the preview", func() bool {
		d, err := client.Deployment(ctx, made.ID)
		return err == nil && d.Finished()
	})
	rec, err := db.Service(ctx, "shop", "topic", "db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetServiceSeeded(ctx, rec.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SetAccess(ctx, "shop", "topic", apitypes.AccessChange{Mode: "public"}); err == nil {
		t.Errorf("public was let through")
	}
	a, err := client.SetAccess(ctx, "shop", "topic", apitypes.AccessChange{Mode: ""})
	t.Logf("default: %+v, %v", a, err)
	if err == nil && a.Mode == "public" {
		t.Errorf("moor access set default opened the gate over a copy of production's data: mode is now %s", a.Mode)
	}
}

// S2. A static site over what is asleep starts a database that it has no use for, and which nothing can now put to sleep.
func TestAStaticSiteDoesNotStartWhatIsAsleep(t *testing.T) {
	bin, dir := serviceEngine(t)
	cfg := testConfig(t)
	cfg.Containers.Engine = bin
	addrs, _ := start(t, cfg)
	client := apiClient(t, cfg, addrs)
	ctx := context.Background()
	db := testAuth(t, cfg).DB
	deployedSource(t, client, "shop", manifestWithService("", "0s"))
	rec, err := db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "running-"+container.ServiceName(rec.ID))
	if _, err := client.SleepEnvironment(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "it to stop", func() bool {
		_, there := os.Stat(file)
		return there != nil
	})
	d, _ := deploySource(t, client, "shop", map[string]string{"index.html": "hello"})
	t.Logf("the static deployment: %s %s", d.Status, d.Error)
	time.Sleep(time.Second)
	now, _ := db.Service(ctx, "shop", "production", "db")
	_, there := os.Stat(file)
	t.Logf("service state=%s container there=%v", now.State, there == nil)
	_, sleepErr := client.SleepEnvironment(ctx, "shop", "production")
	t.Logf("putting it to sleep: %v", sleepErr)
}
