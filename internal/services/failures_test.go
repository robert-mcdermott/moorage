// SPDX-License-Identifier: Apache-2.0

package services

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// patchEngine adds cases to the fixture's engine.
func (f *fixture) patchEngine(extra string) {
	f.t.Helper()
	bin := filepath.Join(f.dir, "podman")
	data, err := os.ReadFile(bin)
	if err != nil {
		f.t.Fatal(err)
	}
	text := strings.Replace(string(data), "case \"$1\" in\n  --version)", extra+"\ncase \"$1\" in\n  --version)", 1)
	if err := os.WriteFile(bin, []byte(text), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) makeProductionWithDump() store.Service {
	f.t.Helper()
	ctx := context.Background()
	f.m.o.DataDir = f.dir
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", postgresPlan(), io.Discard); err != nil {
		f.t.Fatal(err)
	}
	if problems := f.m.DumpAll(ctx); len(problems) != 0 {
		f.t.Fatal(problems)
	}
	rec, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		f.t.Fatal(err)
	}
	return rec
}

func (f *fixture) volumesLike(part string) []string {
	f.t.Helper()
	list, _ := filepath.Glob(filepath.Join(f.dir, "volume-*"+part+"*"))
	return list
}

// A preview that asks for a seed before any backup has run, which is the usual first case, starts empty, and so needs a bucket.
func TestASeedWithNoDumpStillMakesABucket(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.o.DataDir = f.dir
	f.m.o.GateFor = func(set, asked, kind string) (string, string) { return "login", "setting" }
	var made []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { made = append(made, r.Method+" "+r.URL.Path) }))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	f.m.o.AllocPort = func(context.Context) (int, error) { return port, nil }
	if _, err := f.db.CreateEnvironment(ctx, "shop", "topic", store.KindPreview, "topic"); err != nil {
		t.Fatal(err)
	}
	plan := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{"files": {Type: manifest.ServiceS3, Env: "S3_ENDPOINT", Previews: "own", Seed: "production", Sanitize: "none"}}}
	var log bytes.Buffer
	if err := f.m.Ensure(ctx, "shop", "topic", store.KindPreview, "me", "", plan, &log); err != nil {
		t.Fatal(err)
	}
	t.Logf("deployment log:\n%s", log.String())
	rec, err := f.db.Service(ctx, "shop", "topic", "files")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded=%v requests to the gateway=%v", rec.Seeded, made)
	if len(made) == 0 {
		t.Errorf("nothing was copied and no bucket was made: the app is told S3_BUCKET=app, which does not exist")
	}
	if rec.Seeded {
		t.Errorf("on record as holding a copy of production's data, when it started empty")
	}
}

// The same with PostgreSQL: it holds nothing of production's, so that a later commit is not refused for holding a copy.
func TestASeedWithNoDumpIsNotACopy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.o.DataDir = f.dir
	f.m.o.GateFor = func(set, asked, kind string) (string, string) { return "login", "setting" }
	if _, err := f.db.CreateEnvironment(ctx, "shop", "topic", store.KindPreview, "topic"); err != nil {
		t.Fatal(err)
	}
	plan := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{"db": {Type: manifest.ServicePostgres, Version: "18", Env: "DATABASE_URL", Previews: "own", Seed: "production", Sanitize: "none"}}}
	var log bytes.Buffer
	if err := f.m.Ensure(ctx, "shop", "topic", store.KindPreview, "me", "", plan, &log); err != nil {
		t.Fatal(err)
	}
	t.Logf("deployment log:\n%s", log.String())
	if !strings.Contains(log.String(), "it starts empty") {
		t.Fatalf("expected it to start empty")
	}
	f.m.o.GateFor = func(set, asked, kind string) (string, string) { return "public", "manifest" }
	err := f.m.Ensure(ctx, "shop", "topic", store.KindPreview, "me", "", plan, io.Discard)
	t.Logf("the later commit: %v", err)
	if err != nil && strings.Contains(err.Error(), "a copy of production's data") {
		t.Errorf("refused for holding a copy of production's data, when nothing was ever copied into it")
	}
}

// A restore that cannot start leaves no volume, under a name that nothing would use again.
func TestARestoreThatCannotRunLeavesNoVolumes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.makeProductionWithDump()
	f.flag("no-run", true)
	for i := 0; i < 3; i++ {
		if err := f.m.Restore(ctx, "shop", "production", "db"); err == nil {
			t.Fatal("expected it to fail")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if left := f.volumesLike("-restored-"); len(left) != 0 {
		t.Errorf("%d volumes left behind by a restore that failed: %v", len(left), left)
	}
}

// What a server stopping during a restore or an upgrade leaves: what runs over a copy stops at the next start, the doctor speaks of the volumes, and removing the service takes them.
func TestWhatACrashLeavesOfATemporary(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	main := container.ServiceName(rec.ID)
	left := []string{
		"running-" + main + "-restore", "running-" + main + "-upgrade",
		"volume-" + container.ServiceRestoredVolume(rec.ID, 1234), "volume-" + container.ServiceUpgradeVolume(rec.ID, "19"),
	}
	for _, name := range left {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte("true true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.m.clearUp(ctx)
	// What runs over a copy of what it holds stops, and volumes stay, as one that the record does not name may be newer than the record.
	for i, name := range left {
		_, err := os.Stat(filepath.Join(f.dir, name))
		if i < 2 && err == nil {
			t.Errorf("still running after the next start: %s", name)
		}
		if i >= 2 && err != nil {
			t.Errorf("gone after the next start: %s", name)
		}
	}
	if _, problems := f.m.Problems(ctx); len(problems) != 1 || !strings.Contains(problems[0], "lets go of it") {
		t.Errorf("what the doctor says: %v", problems)
	}
	if err := f.m.Remove(ctx, "shop", "production", "db", "me"); err != nil {
		t.Fatal(err)
	}
	var stayed []string
	for _, name := range left {
		if _, err := os.Stat(filepath.Join(f.dir, name)); err == nil {
			stayed = append(stayed, name)
		}
	}
	if len(stayed) != 0 {
		t.Errorf("moor services remove, which promises to take what it holds, leaves: %v", stayed)
	}
}

// An upgrade's temporary container reads from the old one by a name that only their network resolves.
func TestAnUpgradesTemporaryIsOnTheNetwork(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	f.image(19)
	u, err := f.m.PrepareUpgrade(ctx, "shop", "production", "db", "19", "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Upgrade(ctx, u, io.Discard); err != nil {
		t.Fatal(err)
	}
	main := container.ServiceName(rec.ID)
	var runLine, dumpLine string
	for _, line := range strings.Split(f.asked(), "\n") {
		if strings.HasPrefix(line, "run -d --name "+main+"-upgrade") {
			runLine = line
		}
		if strings.Contains(line, "pg_dumpall") && strings.Contains(line, "-h") {
			dumpLine = line
		}
	}
	t.Logf("its temporary: %s", runLine)
	t.Logf("what it runs:  %s", dumpLine)
	if !strings.Contains(dumpLine, "-h "+main) {
		t.Fatalf("it no longer goes by name: %s", dumpLine)
	}
	if !strings.Contains(runLine, "--network") {
		t.Errorf("the temporary is on the engine's default network, and %s is only known on its environment's: pg_dumpall cannot resolve it", main)
	}
}

// What can refuse a restore does so before anything stops, including an engine that is down.
func TestCheckRestoreLooksAtTheEngine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.makeProductionWithDump()
	f.flag("down", true)
	f.source.Status(ctx)
	if err := f.m.CheckRestore(ctx, "shop", "production", "db"); err == nil {
		t.Errorf("CheckRestore passes with the engine down, so the deployer stops the app and cancels its commands for a restore that then fails")
	}
	err := f.m.Restore(ctx, "shop", "production", "db")
	t.Logf("and the restore itself: %v", err)
}

// One service's container being gone does not take away what is known of the others, as asking by name would.
func TestOneServiceGoneLeavesTheMetricsOfOthers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.o.DataDir = f.dir
	plan := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{
		"db":    {Type: manifest.ServicePostgres, Version: "18", Env: "DATABASE_URL", Previews: "own"},
		"other": {Type: manifest.ServicePostgres, Version: "18", Env: "OTHER_URL", Previews: "own"},
	}}
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", plan, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := f.m.MetricsContainers(ctx); len(got) != 2 {
		t.Fatalf("both running: %v", got)
	}
	rec, _ := f.db.Service(ctx, "shop", "production", "other")
	os.Remove(filepath.Join(f.dir, "running-"+container.ServiceName(rec.ID)))
	got := f.m.MetricsContainers(ctx)
	t.Logf("with one gone: %v", got)
	if len(got) != 1 {
		t.Errorf("with %s gone, nothing is known of the one that is still running", rec.Name)
	}
}

// A sample does not wait for long for an engine that has hung, against a tick of ten seconds.
func TestAHungEngineDoesNotHoldUpASample(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 20s")
	}
	f := newFixture(t)
	ctx := context.Background()
	f.makeProductionWithDump()
	f.patchEngine(`if [ "$1" = ps ] && [ -e "` + f.dir + `/hang" ]; then sleep 22; fi`)
	// As the server has it, rather than the fixture's way of asking every time.
	bin := filepath.Join(f.dir, "podman")
	source := container.NewSource(container.SourceOptions{Setting: bin, NewDriver: func(e *container.Engine) *container.Driver {
		return container.New(e, "test", slog.New(slog.DiscardHandler))
	}})
	f.m.o.Engines = source
	f.flag("hang", true)
	store2 := metrics.New(f.db, slog.New(slog.DiscardHandler))
	c := &metrics.Collector{Store: store2, Log: slog.New(slog.DiscardHandler), Instances: f.m.MetricsInstances, Containers: f.m.MetricsContainers}
	began := time.Now()
	c.Sample(ctx, time.Now())
	took := time.Since(began)
	t.Logf("one sample took %s, for every app on the server, against a tick of %s", took, metrics.SampleEvery)
	if took > metrics.SampleEvery {
		t.Errorf("the collector's one goroutine waits for the engine's status: %s", took)
	}
}

// A restore done while a load was pending ends that, or the next look would wipe what was restored and what the app has written to it.
func TestARestoreClearsWhatWasPending(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	if err := f.db.SetServicePendingLoad(ctx, rec.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Restore(ctx, "shop", "production", "db"); err != nil {
		t.Fatal(err)
	}
	got, _ := f.db.Service(ctx, "shop", "production", "db")
	t.Logf("after the restore: volume=%s old=%s pending=%v running=%v", got.Volume, got.OldVolume, got.PendingLoad, f.running(got))
	data, _ := os.ReadFile(filepath.Join(f.dir, "running-"+container.ServiceName(rec.ID)))
	t.Logf("main says: %q (an app can reach it)", data)
	before := f.asked()
	f.m.keepUp(ctx, got)
	after := strings.TrimPrefix(f.asked(), before)
	if strings.Contains(after, "volume rm "+got.Volume) {
		t.Errorf("moor services restore returned success with the app connecting, and the next look removed the volume that it restored: %s", got.Volume)
	}
}

// Update with a load pending would start it where an app can reach it, on what has not been put back.
func TestUpdateRefusesWhatIsBeingPutBack(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	if err := f.db.SetServicePendingLoad(ctx, rec.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetServiceDigest(ctx, rec.ID, "older"); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(f.dir, "running-"+container.ServiceName(rec.ID)))
	if _, err := f.m.Update(ctx, "shop", "production", "db", io.Discard); err == nil || !strings.Contains(err.Error(), "not been put back") {
		t.Errorf("%v", err)
	}
	data, _ := os.ReadFile(filepath.Join(f.dir, "running-"+container.ServiceName(rec.ID)))
	if strings.TrimSpace(string(data)) == "true" {
		t.Errorf("with its dump not yet put back, moor services update started it where an app can reach it, on a port, on the network: an app connecting now finds an empty or half-loaded database")
	}
}

// What a server left is looked at once the engine answers, if it did not as the server started.
func TestWhatAServerLeftIsLookedAtUntilItCanBe(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	file := filepath.Join(f.dir, "running-"+container.ServiceName(rec.ID))
	if err := f.db.SetServiceState(ctx, rec.ID, store.ServiceStopped); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("true true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.flag("down", true)
	if f.m.clearUp(ctx) {
		t.Error("it says that it looked, with the engine not answering")
	}
	f.flag("down", false)
	if !f.m.clearUp(ctx) {
		t.Error("it says that it could not look")
	}
	if _, err := os.Stat(file); err == nil {
		t.Errorf("a private server over its volume stays up with nothing to stop it, while the record says stopped")
	}
}

// Asking how something stands does not change which engine it is on record as held by, if its program cannot be found for that moment.
func TestAMomentWithoutTheProgramMovesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	if err := f.db.SetServiceEngine(ctx, rec.ID, filepath.Join(f.dir, "docker-for-a-moment-gone")); err != nil {
		t.Fatal(err)
	}
	f.m.MetricsContainers(ctx)
	got, _ := f.db.Service(ctx, "shop", "production", "db")
	t.Logf("engine on record after one metrics tick: %s", got.Engine)
	if got.Engine != filepath.Join(f.dir, "docker-for-a-moment-gone") {
		t.Errorf("a metrics tick, which only reads, moved this service to another engine for good; on that engine it has no volume, and so it comes up from its last dump or empty")
	}
}

// A seed that fails with the engine unable to remove the volume stays on record, or an unsanitized copy would stay with nothing to say that it is there.
func TestAFailedSeedKeepsTrackOfWhatItCouldNotRemove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.makeProductionWithDump()
	f.m.o.GateFor = func(set, asked, kind string) (string, string) { return "login", "setting" }
	if _, err := f.db.CreateEnvironment(ctx, "shop", "topic", store.KindPreview, "topic"); err != nil {
		t.Fatal(err)
	}
	f.patchEngine(`if [ "$1" = volume ] && [ "$2" = rm ] && [ -e "` + f.dir + `/no-volume-rm" ]; then echo "volume is in use" >&2; exit 1; fi`)
	f.flag("no-volume-rm", true)
	plan := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{"db": {Type: manifest.ServicePostgres, Version: "18", Env: "DATABASE_URL", Previews: "own", Seed: "production", Sanitize: "scripts/not-there.sql"}}}
	before, _ := filepath.Glob(filepath.Join(f.dir, "volume-*"))
	err := f.m.Ensure(ctx, "shop", "topic", store.KindPreview, "me", t.TempDir(), plan, io.Discard)
	t.Logf("the deployment: %v", err)
	after, _ := filepath.Glob(filepath.Join(f.dir, "volume-*"))
	_, recErr := f.db.Service(ctx, "shop", "topic", "db")
	if len(after) > len(before) && recErr != nil {
		t.Errorf("a volume with an unsanitized copy of production's data is left, and the record of it is gone: %v -> %v", before, after)
	}
}

// What tar really writes: formats a real tar may use, none of which should be refused.
func TestTarsThatMustBeWhole(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, format tar.Format, entries map[string][]byte, trailerPad int) string {
		path := filepath.Join(dir, name)
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		for entry, content := range entries {
			h := &tar.Header{Name: entry, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg, Format: format, ModTime: time.Unix(1700000000, 0)}
			if err := w.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			w.Write(content)
		}
		w.Close()
		b.Write(make([]byte, trailerPad))
		if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	long := strings.Repeat("deep/", 60) + "object-名前"
	for name, path := range map[string]string{
		// BusyBox: two blocks of nothing and no padding.
		"gnu, as BusyBox ends one":   write("a", tar.FormatGNU, map[string][]byte{"s3/app/a": []byte("x"), long: []byte("y")}, 0),
		"gnu, padded to 10k":         write("b", tar.FormatGNU, map[string][]byte{"s3/app/a": []byte("x"), long: []byte("y")}, 10240-(512*4+1024)%10240),
		"pax":                        write("c", tar.FormatPAX, map[string][]byte{"s3/app/a": []byte("x"), long: []byte("y")}, 0),
		"ending in an all-zero file": write("d", tar.FormatGNU, map[string][]byte{"s3/app/z": make([]byte, 4096)}, 0),
	} {
		if err := dumpWhole(path, manifest.ServiceS3); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// One cut short inside an all-zero file ends in zeros.
	whole, _ := os.ReadFile(write("e", tar.FormatGNU, map[string][]byte{"s3/app/z": make([]byte, 40960)}, 0))
	cut := filepath.Join(dir, "cut")
	os.WriteFile(cut, whole[:5120], 0o600)
	if err := dumpWhole(cut, manifest.ServiceS3); err == nil {
		t.Error("a tar cut short inside a file of zeros passes")
	}
	// The system's own tar, here libarchive's.
	tree := t.TempDir()
	os.MkdirAll(filepath.Join(tree, "s3", "app"), 0o755)
	os.MkdirAll(filepath.Join(tree, "meta", "app"), 0o755)
	os.WriteFile(filepath.Join(tree, "s3", "app", "a"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(tree, "s3", "app", strings.Repeat("n", 200)), []byte("y"), 0o644)
	made := filepath.Join(dir, "sys.tar")
	cmd := exec.Command("/usr/bin/tar", "-C", tree, "-cf", made, "s3", "meta")
	if out, err := cmd.CombinedOutput(); err == nil {
		if err := dumpWhole(made, manifest.ServiceS3); err != nil {
			t.Errorf("the system's tar: %v", err)
		}
	} else {
		t.Logf("no system tar: %s", out)
	}
	// What pg_dumpall ends with.
	sql := filepath.Join(dir, "d.sql")
	os.WriteFile(sql, []byte("\\unrestrict abc\n\n--\n-- PostgreSQL database cluster dump complete\n--\n\n"), 0o600)
	if err := dumpWhole(sql, manifest.ServicePostgres); err != nil {
		t.Errorf("pg_dumpall's end: %v", err)
	}
}

// What the filter leaves out, against text in a function that looks like what it looks for.
func TestTheFilterAndTextThatLooksLikeIt(t *testing.T) {
	in := strings.Join([]string{
		"CREATE ROLE app;",
		"--",
		"-- Database \"analytics\" dump",
		"--",
		"\\connect analytics",
		"CREATE FUNCTION a.notes() RETURNS text LANGUAGE sql AS $$ select '",
		"CREATE ROLE app;",
		"' $$;",
		"CREATE FUNCTION a.doc() RETURNS text LANGUAGE sql AS $$ select '",
		"COPY people FROM stdin;",
		"' $$;",
		"--",
		"-- Database \"app\" dump",
		"--",
		"CREATE DATABASE app WITH TEMPLATE = template0 ENCODING = 'UTF8';",
		"\\connect app",
		"",
	}, "\n")
	out, err := io.ReadAll(withoutWhatIsThere(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", out)
	if strings.Count(string(out), "CREATE ROLE app;") != 1 {
		t.Errorf("a line of text inside a function body was removed from the function")
	}
	if !strings.Contains(string(out), "CREATE DATABASE app") {
		t.Logf("the statement it should remove is gone")
	} else {
		t.Errorf("text that looks like a COPY leaves it thinking that it is among rows, and CREATE DATABASE app goes through, which stops the load with ON_ERROR_STOP")
	}
}

// A wake during a backup's dump of what is asleep goes first, and that dump waits for the next backup.
func TestAWakeDoesNotWaitForADumpOfWhatIsAsleep(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	f.patchEngine(`if [ "$1" = exec ] && echo "$*" | grep -q pg_dumpall && [ -e "` + f.dir + `/slow-dump" ]; then sleep 6; fi`)
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	f.flag("slow-dump", true)
	done := make(chan []string, 1)
	go func() { done <- f.m.DumpAll(ctx) }()
	time.Sleep(1500 * time.Millisecond)
	limited, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	began := time.Now()
	err := f.m.Wake(limited, "shop", "production")
	took := time.Since(began)
	t.Logf("a wake limited to 1s took %s: %v", took.Round(100*time.Millisecond), err)
	problems := <-done
	now, _ := f.db.Service(ctx, "shop", "production", rec.Name)
	t.Logf("the dump: %v; state %s", problems, now.State)
	if took > 3*time.Second {
		t.Errorf("a wake, holding the deployer's lock of the environment, cannot leave until the dump ends, which may take two hours")
	}
}

// A manifest that now says previews = "none", over a preview that has a service.
func TestPreviewsNoneGivesNoAddress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.o.DataDir = f.dir
	f.m.o.GateFor = func(set, asked, kind string) (string, string) { return "login", "setting" }
	if _, err := f.db.CreateEnvironment(ctx, "shop", "topic", store.KindPreview, "topic"); err != nil {
		t.Fatal(err)
	}
	f.makeProductionWithDump()
	seeded := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{"db": {Type: manifest.ServicePostgres, Version: "18", Env: "DATABASE_URL", Previews: "own", Seed: "production", Sanitize: "none"}}}
	if err := f.m.Ensure(ctx, "shop", "topic", store.KindPreview, "me", "", seeded, io.Discard); err != nil {
		t.Fatal(err)
	}
	none := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{"db": {Type: manifest.ServicePostgres, Version: "18", Env: "DATABASE_URL", Previews: "none"}}}
	f.m.o.GateFor = func(set, asked, kind string) (string, string) { return "public", "manifest" }
	err := f.m.Ensure(ctx, "shop", "topic", store.KindPreview, "me", "", none, io.Discard)
	secret, _, _, aerr := f.m.Addresses(ctx, "shop", "topic", none, false)
	t.Logf("deployment with an open gate: %v; address given: %v (%v)", err, secret["DATABASE_URL"] != "", aerr)
	if err == nil && secret["DATABASE_URL"] != "" {
		t.Errorf("the refusal is passed over by saying previews = none, and the app is still given the seeded copy, behind no gate")
	}
}

// What psql says of a row that will not load stays out of the deployment's error.
func TestNoRowInTheErrorOfASeed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.makeProductionWithDump()
	f.m.o.GateFor = func(set, asked, kind string) (string, string) { return "login", "setting" }
	if _, err := f.db.CreateEnvironment(ctx, "shop", "topic", store.KindPreview, "topic"); err != nil {
		t.Fatal(err)
	}
	f.patchEngine(`if [ "$1" = exec ] && [ "$2" = -i ]; then cat >/dev/null; echo 'psql:<stdin>:88: ERROR: value too long for type character varying(5)' >&2; echo 'CONTEXT: COPY people, line 1, column email: "ada@example.com"' >&2; exit 3; fi`)
	plan := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{"db": {Type: manifest.ServicePostgres, Version: "18", Env: "DATABASE_URL", Previews: "own", Seed: "production", Sanitize: "none"}}}
	err := f.m.Ensure(ctx, "shop", "topic", store.KindPreview, "me", "", plan, io.Discard)
	t.Logf("what the deployment fails with: %v", err)
	if err != nil && strings.Contains(err.Error(), "ada@example.com") {
		t.Errorf("a value from an unsanitized row of production's is in the deployment's error, which goes to its log, to events, to notifications and to what comments on a pull request")
	}
}

// A restore of what is asleep, and what comes after it.
func TestARestoreOfWhatIsAsleepAndAfter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Restore(ctx, "shop", "production", "db"); err != nil {
		t.Fatal(err)
	}
	got, _ := f.db.Service(ctx, "shop", "production", "db")
	if f.running(got) || got.State != store.ServiceStopped || got.OldVolume != rec.Volume || got.Volume == rec.Volume {
		t.Errorf("%+v", got)
	}
	if entries, _ := filepath.Glob(filepath.Join(f.dir, "running-*")); len(entries) != 0 {
		t.Errorf("left running: %v", entries)
	}
	if problems := f.m.DumpAll(ctx); len(problems) != 0 {
		t.Errorf("%v", problems)
	}
	if err := f.m.Wake(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.asked(), got.Volume+":") {
		t.Error("it never ran on the new volume")
	}
	if err := f.m.CheckRestore(ctx, "shop", "production", "db"); err == nil {
		t.Error("a second restore should wait for --finish")
	}
	if err := f.m.Remove(ctx, "shop", "production", "db", "me"); err != nil {
		t.Fatal(err)
	}
	if left := f.volumesLike(""); len(left) != 0 {
		t.Errorf("left: %v", left)
	}
}

// Going back to what it held before takes it off what it has held since, and an upgrade's record has what ran on it.
func TestGoingBack(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	if err := f.m.CheckBack(ctx, "shop", "production", "db"); err == nil || !strings.Contains(err.Error(), "nothing from before") {
		t.Errorf("with nothing to go back to: %v", err)
	}
	next := rec
	next.Version, next.Image, next.Digest, next.Mount, next.Volume = "19", "docker.io/library/postgres:19-trixie", "", "/somewhere", container.ServiceUpgradeVolume(rec.ID, "19")
	f.flag("volume-"+next.Volume, true)
	if err := f.db.SetServiceMoved(ctx, rec.ID, next, rec); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Back(ctx, "shop", "production", "db"); err != nil {
		t.Fatal(err)
	}
	got, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != rec.Version || got.Image != rec.Image || got.Mount != rec.Mount || got.Volume != rec.Volume || got.OldVolume != "" || got.OldImage != "" {
		t.Errorf("%+v", got)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "volume-"+next.Volume)); err == nil {
		t.Error("what it held since is still there")
	}
	if !f.running(rec) {
		t.Error("it did not start again")
	}
	// With what it held gone, it says so, and changes nothing.
	if err := f.db.SetServiceMoved(ctx, rec.ID, next, rec); err != nil {
		t.Fatal(err)
	}
	f.flag("volume-"+rec.Volume, false)
	if err := f.m.Back(ctx, "shop", "production", "db"); err == nil || !strings.Contains(err.Error(), "gone from its engine") {
		t.Errorf("%v", err)
	}
	if got, _ := f.db.Service(ctx, "shop", "production", "db"); got.Volume != next.Volume {
		t.Errorf("%+v", got)
	}
}

// A server moves what has lost its engine as it starts, as after a restore to another machine, and only then.
func TestAServerStartingMovesWhatHasLostItsEngine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	gone := filepath.Join(f.dir, "gone", "docker")
	if err := f.db.SetServiceEngine(ctx, rec.ID, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.List(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.db.Service(ctx, "shop", "production", "db"); got.Engine != gone {
		t.Errorf("reading it moved it: %s", got.Engine)
	}
	if !f.m.clearUp(ctx) {
		t.Fatal("it could not look")
	}
	got, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	if want := f.source.Status(ctx).Bin; got.Engine != want {
		t.Errorf("%s, and not %s", got.Engine, want)
	}
	// One whose program is still here holds what it held, and that is not to be left behind.
	if err := f.db.SetServiceEngine(ctx, rec.ID, "/bin/sh"); err != nil {
		t.Fatal(err)
	}
	f.m.clearUp(ctx)
	if got, _ := f.db.Service(ctx, "shop", "production", "db"); got.Engine != "/bin/sh" {
		t.Errorf("it moved one with its program here: %s", got.Engine)
	}
}

// What a server left of a dump does not stay in every backup, and goes with its service.
func TestUnfinishedDumps(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	dump := f.m.dumpPath(rec)
	left := dump + ".12345"
	if err := os.WriteFile(left, []byte("part of it"), 0o600); err != nil {
		t.Fatal(err)
	}
	if problems := f.m.DumpAll(ctx); len(problems) != 0 {
		t.Fatal(problems)
	}
	if _, err := os.Stat(left); err == nil {
		t.Error("the next dump left it")
	}
	if err := os.WriteFile(left, []byte("part of it"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Remove(ctx, "shop", "production", "db", "me"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{dump, left} {
		if _, err := os.Stat(name); err == nil {
			t.Errorf("removing the service left %s", name)
		}
	}
}

// A seed runs the version that production has moved to, if the manifest does not say, as it could not load it otherwise.
func TestASeedRunsWhatProductionRuns(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.o.DataDir = f.dir
	f.m.o.GateFor = func(set, asked, kind string) (string, string) { return "login", "setting" }
	f.image(17)
	plan := postgresPlan()
	db := plan.Services["db"]
	db.Version, db.Seed, db.Sanitize = "", "production", manifest.SanitizeNone
	plan.Services["db"] = db
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{"db": {Type: manifest.ServicePostgres, Version: "17", Env: "DATABASE_URL", Previews: "own"}}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateEnvironment(ctx, "shop", "topic", store.KindPreview, "topic"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Ensure(ctx, "shop", "topic", store.KindPreview, "me", "", plan, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := f.db.Service(ctx, "shop", "topic", "db")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "17" || !strings.Contains(got.Image, "postgres:17") {
		t.Errorf("%s %s", got.Version, got.Image)
	}
}

// What no app is to reach yet has no network at all, and an engine would otherwise give it one that other containers are on.
func TestWhatNoAppIsToReachHasNoNetwork(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.makeProductionWithDump()
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	os.Remove(f.calls)
	if problems := f.m.DumpAll(ctx); len(problems) != 0 {
		t.Fatal(problems)
	}
	if !strings.Contains(f.asked(), "--network none") {
		t.Errorf("%s", f.asked())
	}
}
