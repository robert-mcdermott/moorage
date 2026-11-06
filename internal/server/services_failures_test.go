// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func r8Patch(t *testing.T, bin, extra string) {
	t.Helper()
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(data), "for last; do :; done\n", "for last; do :; done\n"+extra+"\n", 1)
	if err := os.WriteFile(bin, []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
}

// An environment asleep, and a commit that is refused: it is awake from when a release begins, whatever comes of that, and what is running connects to its services.
func TestARefusedDeployOfWhatIsAsleepWakesItsServices(t *testing.T) {
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
	m := manifestWithService("", "0s")
	m["moorage.toml"] = strings.Replace(m["moorage.toml"], `type = "postgres"`, "type = \"postgres\"\nversion = \"17\"", 1)
	d, _ := deploySource(t, client, "shop", m)
	t.Logf("the deployment: %s: %s", d.Status, d.Error)
	if d.Status != apitypes.StatusFailed {
		t.Fatalf("expected it to be refused")
	}
	// Plenty of ticks of what keeps things running (200ms), and of what starts instances.
	time.Sleep(4 * time.Second)
	env, err := client.Environment(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	instances, _ := client.Instances(ctx, "shop", "production")
	now, _ := db.Service(ctx, "shop", "production", "db")
	_, there := os.Stat(file)
	t.Logf("asleep=%v instances=%d service state=%s container there=%v", env.Asleep, len(instances), now.State, there == nil)
	if !env.Asleep && there != nil {
		t.Errorf("it is awake, with %d instances, and its service is on record as %s and is not running, and nothing will start it", len(instances), now.State)
	}
	// moor wake, the first thing anyone would try, does nothing.
	if _, err := client.WakeEnvironment(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if _, there := os.Stat(file); there != nil && !env.Asleep {
		t.Errorf("moor wake does not start it either")
	}
}

// A server stopping during a command in an environment put to sleep by hand, whatever its kind, and with or without a sleep_after: what it left stops.
func TestWhatACommandLeftStopsInProductionAsleep(t *testing.T) {
	for _, c := range []struct {
		name string
		env  string
	}{{"production, asleep by hand", "production"}, {"a preview, for comparison", "topic"}} {
		t.Run(c.name, func(t *testing.T) {
			bin, dir := serviceEngine(t)
			cfg := testConfig(t)
			cfg.Containers.Engine = bin
			addrs, stop := start(t, cfg)
			client := apiClient(t, cfg, addrs)
			ctx := context.Background()
			db := testAuth(t, cfg).DB
			if c.env != "production" {
				deployedSource(t, client, "shop", manifestWithService("", "0s"))
				if _, err := db.CreateEnvironment(ctx, "shop", c.env, store.KindPreview, c.env); err != nil {
					t.Fatal(err)
				}
			}
			queued, _, err := client.DeploySource(ctx, "shop", c.env, writeSite(t, manifestWithService("", "0s")), nil)
			if err != nil {
				t.Fatal(err)
			}
			waitUntil(t, "the deployment", func() bool {
				d, err := client.Deployment(ctx, queued.ID)
				return err == nil && d.Finished()
			})
			rec, err := db.Service(ctx, "shop", c.env, "db")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.SleepEnvironment(ctx, "shop", c.env); err != nil {
				t.Fatal(err)
			}
			if _, err := client.StartRun(ctx, "shop", c.env, apitypes.NewRun{Command: "sleep 60"}); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "running-"+container.ServiceName(rec.ID))
			waitUntil(t, "it to start for the command", func() bool {
				_, there := os.Stat(file)
				return there == nil
			})
			time.Sleep(500 * time.Millisecond)
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			addrs, stop = start(t, cfg)
			t.Cleanup(func() { stop() })
			client = apiClient(t, cfg, addrs)
			db = testAuth(t, cfg).DB
			// Plenty of sweeps: they come every 100ms here.
			time.Sleep(5 * time.Second)
			env, err := client.Environment(ctx, "shop", c.env)
			if err != nil {
				t.Fatal(err)
			}
			now, _ := db.Service(ctx, "shop", c.env, "db")
			_, there := os.Stat(file)
			t.Logf("asleep=%v state=%s container there=%v", env.Asleep, now.State, there == nil)
			if env.Asleep && there == nil {
				t.Errorf("asleep, and its service runs on, and goes on being restarted, with no command using it")
			}
		})
	}
}

// Stopping a server does not wait for a restore, which can take hours, because a command that it cancelled wants its turn afterwards.
func TestShuttingDownDuringARestore(t *testing.T) {
	for _, c := range []struct {
		name    string
		command bool
	}{{"with a command when it began", true}, {"without", false}} {
		t.Run(c.name, func(t *testing.T) {
			bin, dir := serviceEngine(t)
			r8Patch(t, bin, `if [ "$1" = exec ] && [ "$2" = -i ] && [ -e "`+dir+`/hang-load" ]; then sleep 40; fi`)
			cfg := testConfig(t)
			cfg.Containers.Engine = bin
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan Addrs, 1)
			done := make(chan error, 1)
			log, logDone := testLogger(t)
			defer logDone()
			go func() { done <- Run(ctx, cfg, log, testOptions(func(a Addrs) { ready <- a })) }()
			addrs := <-ready
			client := apiClient(t, cfg, addrs)
			deployedSource(t, client, "shop", manifestWithService("", "0s"))
			dump := filepath.Join(cfg.DataDir, "data", "shop", ".services", "production", "db.sql")
			os.MkdirAll(filepath.Dir(dump), 0o700)
			os.WriteFile(dump, []byte("x\n-- PostgreSQL database cluster dump complete\n--\n\n"), 0o600)
			if c.command {
				if _, err := client.StartRun(context.Background(), "shop", "production", apitypes.NewRun{Command: "sleep 60"}); err != nil {
					t.Fatal(err)
				}
				time.Sleep(500 * time.Millisecond)
			}
			os.WriteFile(filepath.Join(dir, "hang-load"), nil, 0o644)
			go client.RestoreService(context.Background(), "shop", "production", "db")
			time.Sleep(3 * time.Second)
			began := time.Now()
			cancel()
			select {
			case <-done:
				t.Logf("the server stopped in %s", time.Since(began).Round(time.Second))
			case <-time.After(60 * time.Second):
				t.Logf("the server had not stopped after 60s")
			}
			if took := time.Since(began); took > 20*time.Second {
				t.Errorf("stopping the server took %s: it waits for the restore", took.Round(time.Second))
			}
		})
	}
}

// A restore that the engine being down will refuse stops neither the app nor its commands.
func TestARestoreWithTheEngineDownLeavesTheAppAlone(t *testing.T) {
	bin, dir := serviceEngine(t)
	down := filepath.Join(dir, "down")
	r8Patch(t, bin, `if [ -e "`+down+`" ] && [ "$1" != "--version" ]; then echo "Cannot connect to Podman" >&2; exit 125; fi`)
	cfg := testConfig(t)
	cfg.Containers.Engine = bin
	addrs, _ := start(t, cfg)
	client := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, client, "shop", manifestWithService("", "0s"))
	dump := filepath.Join(cfg.DataDir, "data", "shop", ".services", "production", "db.sql")
	os.MkdirAll(filepath.Dir(dump), 0o700)
	os.WriteFile(dump, []byte("x\n-- PostgreSQL database cluster dump complete\n--\n\n"), 0o600)
	run, err := client.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "sleep 60"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	before, err := client.Instances(ctx, "shop", "production")
	if err != nil || len(before) == 0 {
		t.Fatalf("%v %v", before, err)
	}
	os.WriteFile(down, nil, 0o644)
	time.Sleep(300 * time.Millisecond) // what is kept of how it stands goes stale
	err = client.RestoreService(ctx, "shop", "production", "db")
	t.Logf("the restore: %v", err)
	time.Sleep(time.Second)
	after, _ := client.Instances(ctx, "shop", "production")
	r, _ := client.Run(ctx, "shop", run.ID)
	t.Logf("instance %s -> %d now; the command: %s", before[0].ID, len(after), r.Status)
	if len(after) == 0 || after[0].ID != before[0].ID {
		t.Errorf("the app's instance was stopped for a restore that could only fail")
	}
	if r.Status != "running" {
		t.Errorf("its command was cancelled for a restore that could only fail: %s", r.Status)
	}
}

// What is asleep has said nothing, as its container has gone with what it said, and a restore that fails says why.
func TestLogsOfWhatIsAsleepAndWhyARestoreFailed(t *testing.T) {
	bin, dir := serviceEngine(t)
	r8Patch(t, bin, `if [ "$1" = logs ] && [ ! -e "`+dir+`/running-$last" ]; then echo "Error: no such container $last" >&2; exit 1; fi
if [ "$1" = exec ] && [ "$2" = -i ] && [ -e "`+dir+`/bad-load" ]; then cat > /dev/null; echo "psql: ERROR: it will not load" >&2; exit 3; fi`)
	cfg := testConfig(t)
	cfg.Containers.Engine = bin
	addrs, _ := start(t, cfg)
	client := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, client, "shop", manifestWithService("", "0s"))
	dump := filepath.Join(cfg.DataDir, "data", "shop", ".services", "production", "db.sql")
	os.MkdirAll(filepath.Dir(dump), 0o700)
	os.WriteFile(dump, []byte("x\n-- PostgreSQL database cluster dump complete\n--\n\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "bad-load"), nil, 0o644)
	err := client.RestoreService(ctx, "shop", "production", "db")
	t.Logf("a restore that would not load tells the user: %v", err)
	if err == nil || !strings.Contains(err.Error(), "load") {
		t.Errorf("why it failed is only in the server's log")
	}
	if _, err := client.SleepEnvironment(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	text, err := client.ServiceLog(ctx, "shop", "production", "db", 10)
	t.Logf("moor logs --service of what is asleep: %q, %v", text, err)
	if err != nil {
		t.Errorf("reading the logs of a service that is asleep is an internal error of the server")
	}
}

// A restore of what is asleep leaves it asleep, the whole way through the deployer.
func TestARestoreOfWhatIsAsleepThroughTheDeployer(t *testing.T) {
	bin, dir := serviceEngine(t)
	cfg := testConfig(t)
	cfg.Containers.Engine = bin
	addrs, _ := start(t, cfg)
	client := apiClient(t, cfg, addrs)
	ctx := context.Background()
	db := testAuth(t, cfg).DB
	deployedSource(t, client, "shop", manifestWithService("", "0s"))
	rec, _ := db.Service(ctx, "shop", "production", "db")
	dump := filepath.Join(cfg.DataDir, "data", "shop", ".services", "production", "db.sql")
	os.MkdirAll(filepath.Dir(dump), 0o700)
	os.WriteFile(dump, []byte("x\n-- PostgreSQL database cluster dump complete\n--\n\n"), 0o600)
	if _, err := client.SleepEnvironment(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if err := client.RestoreService(ctx, "shop", "production", "db"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	env, _ := client.Environment(ctx, "shop", "production")
	instances, _ := client.Instances(ctx, "shop", "production")
	now, _ := db.Service(ctx, "shop", "production", "db")
	_, there := os.Stat(filepath.Join(dir, "running-"+container.ServiceName(rec.ID)))
	t.Logf("asleep=%v instances=%d state=%s running=%v old=%s", env.Asleep, len(instances), now.State, there == nil, now.OldVolume)
	if !env.Asleep || len(instances) != 0 || now.State != store.ServiceStopped || there == nil || now.OldVolume != rec.Volume {
		t.Errorf("it woke")
	}
	if _, err := client.WakeEnvironment(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if _, there := os.Stat(filepath.Join(dir, "running-"+container.ServiceName(rec.ID))); there != nil {
		t.Errorf("it did not start on waking")
	}
}

// What a copy of production's data was made behind stays between it and everyone else, and it goes with its environment.
func TestACopyOfProductionsData(t *testing.T) {
	bin, _ := serviceEngine(t)
	cfg := testConfig(t)
	cfg.Containers.Engine = bin
	addrs, _ := start(t, cfg)
	client := apiClient(t, cfg, addrs)
	ctx := context.Background()
	db := testAuth(t, cfg).DB
	deployedSource(t, client, "shop", manifestWithService("", "0s"))
	if _, err := db.CreateEnvironment(ctx, "shop", "topic", store.KindPreview, "topic"); err != nil {
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
	if _, err := client.SetAccess(ctx, "shop", "topic", apitypes.AccessChange{Mode: "public"}); err == nil || !strings.Contains(err.Error(), "copy of production's data") {
		t.Errorf("opening its gate: %v", err)
	}
	if _, err := client.SetAccess(ctx, "shop", "topic", apitypes.AccessChange{Mode: "login"}); err != nil {
		t.Errorf("another gate: %v", err)
	}
	dumps := filepath.Join(cfg.DataDir, "data", "shop", ".services", "topic")
	if err := os.MkdirAll(dumps, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dumps, "db.sql.12345"), []byte("part of a dump"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteEnvironment(ctx, "shop", "topic"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dumps); err == nil {
		t.Error("its dumps stayed")
	}
}
