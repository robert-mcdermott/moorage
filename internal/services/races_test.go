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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func reviewTar(t *testing.T, name, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg, ModTime: time.Unix(1700000000, 0)}); err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(content))
	w.Close()
	return b.Bytes()
}

// An engine in trouble in a way whose words happen to include "not found", as Podman with its OCI runtime gone during a package upgrade, is taken for "no volume".
func TestEngineTroubleIsNotANoVolume(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	f.patchEngine(`if [ -e "` + f.dir + `/broken" ] && [ "$1" != --version ] && [ "$1" != ps ]; then echo 'Error: default OCI runtime "crun" not found: invalid argument' >&2; exit 125; fi`)
	f.flag("broken", true)
	f.m.keepUp(ctx, rec)
	got, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("during the trouble: pending=%v; container still running=%v; volume still there=%v", got.PendingLoad, f.running(rec), len(f.volumesLike("-data")) == 1)
	// The trouble passes.
	f.flag("broken", false)
	os.Remove(f.calls)
	f.m.keepUp(ctx, got)
	asked := f.asked()
	removed := strings.Contains(asked, "volume rm "+rec.Volume)
	loaded, _ := os.ReadFile(filepath.Join(f.dir, "loaded"))
	t.Logf("after: volume removed=%v; a dump loaded=%v", removed, strings.Contains(string(loaded), "ALTER ROLE"))
	if got.PendingLoad || removed {
		t.Errorf("a passing trouble with the engine made it remove a good volume, and go back to its dump: pending=%v, removed=%v\n%s", got.PendingLoad, removed, asked)
	}
}

// A dump of what is asleep that had to wait behind a restore goes by what it read before it waited: the volume that the restore has just moved away from.
func TestADumpBehindARestoreReadsWhatItNowHolds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	f.patchEngine(`if [ "$1" = exec ] && [ "$2" = -i ]; then sleep 2; fi`)
	restored := make(chan error, 1)
	go func() { restored <- f.m.Restore(ctx, "shop", "production", "db") }()
	time.Sleep(700 * time.Millisecond)
	done := make(chan []string, 1)
	go func() { done <- f.m.DumpAll(ctx) }()
	if err := <-restored; err != nil {
		t.Fatal(err)
	}
	problems := <-done
	now, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("record: volume=%s old=%s; problems: %v", now.Volume, now.OldVolume, problems)
	var last string
	for _, line := range strings.Split(f.asked(), "\n") {
		if strings.HasPrefix(line, "run ") && strings.Contains(line, "--name "+container.ServiceName(rec.ID)+" ") {
			last = line
		}
	}
	t.Logf("what the dump started: %s", last)
	if strings.Contains(last, rec.Volume+":") && now.OldVolume == rec.Volume {
		t.Errorf("the dump that follows a restore started the old volume, which it then took for what the service holds, and that replaced the dump")
	}
}

// As R2, for an object store whose restore fails halfway: what it left is a part, and a dump of that replaces the only good dump.
func TestADumpBehindAFailedRestoreLeavesTheLastDump(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.o.DataDir = f.dir
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	f.m.o.AllocPort = func(context.Context) (int, error) { return port, nil }
	plan := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{"files": {Type: manifest.ServiceS3, Env: "S3", Previews: "own"}}}
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", plan, io.Discard); err != nil {
		t.Fatal(err)
	}
	rec, err := f.db.Service(ctx, "shop", "production", "files")
	if err != nil {
		t.Fatal(err)
	}
	good := reviewTar(t, "s3/app/all-of-it", "everything it held")
	dump := f.m.dumpPath(rec)
	os.MkdirAll(filepath.Dir(dump), 0o700)
	if err := os.WriteFile(dump, good, 0o600); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(f.dir, "part.tar")
	if err := os.WriteFile(part, reviewTar(t, "s3/app/a-part", "a part"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Loading takes a while, and fails. A dump gives what the container holds, which is then what was loaded so far.
	f.patchEngine(`if [ "$1" = exec ] && [ "$2" = -i ]; then cat > /dev/null; sleep 2; echo "tar: write error" >&2; exit 1; fi
if [ "$1" = exec ] && echo "$*" | grep -q "tar -C /data -cf"; then cat "` + part + `"; exit 0; fi
if [ "$1" = stop ] && [ -e "` + f.dir + `/slow-stop" ]; then sleep 2; fi`)
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	// What comes before its pending load is on record: the check of a large dump, and asking the engine to stop what is there.
	f.flag("slow-stop", true)
	restored := make(chan error, 1)
	go func() { restored <- f.m.Restore(ctx, "shop", "production", "files") }()
	time.Sleep(700 * time.Millisecond)
	done := make(chan []string, 1)
	go func() { done <- f.m.DumpAll(ctx) }()
	restoreErr := <-restored
	problems := <-done
	now, _ := f.db.Service(ctx, "shop", "production", "files")
	have, _ := os.ReadFile(dump)
	t.Logf("restore: %v; dump problems: %v; pending=%v", restoreErr, problems, now.PendingLoad)
	if !bytes.Equal(have, good) {
		t.Errorf("a dump taken of what a failed restore left replaced the only good dump: it now holds: %q", strings.TrimRight(string(have[:60]), "\x00"))
	}
}

// Going back lets go of what it has held since before it knows that what it goes back to will start.
func TestGoingBackKeepsWhatItHasUntilItKnows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	next := rec
	next.Version, next.Image, next.Digest, next.Mount, next.Volume = "19", "docker.io/library/postgres:19-trixie", "", "/somewhere", container.ServiceUpgradeVolume(rec.ID, "19")
	f.flag("volume-"+next.Volume, true)
	if err := f.db.SetServiceMoved(ctx, rec.ID, next, rec); err != nil {
		t.Fatal(err)
	}
	// The old image has gone and cannot be fetched, or the old volume will not start.
	f.flag("no-run", true)
	err := f.m.Back(ctx, "shop", "production", "db")
	_, stat := os.Stat(filepath.Join(f.dir, "volume-"+next.Volume))
	got, _ := f.db.Service(ctx, "shop", "production", "db")
	t.Logf("back: %v; record on %s, keeps %q; what it held since is there: %v", err, got.Volume, got.OldVolume, stat == nil)
	if err != nil && stat != nil {
		t.Errorf("it failed, and what it held since has already gone")
	}
}

// A failed load on the way to a deployment, as after a restore to a new machine: the words of PostgreSQL go in the deployment's error, as they no longer do for a seed.
func TestNoRowInTheErrorOfADeployment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	f.patchEngine(`if [ "$1" = exec ] && [ "$2" = -i ]; then cat > /dev/null; echo 'ERROR:  invalid input syntax. CONTEXT:  COPY people, line 1: "Ada	ada@example.com"' >&2; exit 3; fi`)
	if err := f.db.SetServicePendingLoad(ctx, rec.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.m.driverForStop(ctx, rec); err != nil {
		t.Fatal(err)
	}
	err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", postgresPlan(), io.Discard)
	t.Logf("the deployment's error: %v", err)
	if err != nil && strings.Contains(err.Error(), "ada@example.com") {
		t.Errorf("a row of what it holds is in a deployment's error")
	}
}

func (m *Manager) driverForStop(ctx context.Context, rec store.Service) error {
	driver, err := m.driverFor(ctx, rec)
	if err != nil {
		return err
	}
	return driver.StopService(container.ServiceName(rec.ID), time.Second)
}

// What is being made as a copy is not yet on record as one.
func TestASeedUnderWayCountsAsACopy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	if err := f.db.SetServiceState(ctx, rec.ID, store.ServiceSeeding); err != nil {
		t.Fatal(err)
	}
	if holds, err := f.m.HoldsCopy(ctx, "shop", "production"); err != nil || !holds {
		t.Errorf("a service being seeded right now is not counted, so that moor access can open the gate under it, and the seed then finishes behind no gate")
	}
}

// A row that looks like a heading ends what the filter knows of being among rows. Only a row of the very form of what it removes is at stake.
func TestARowThatLooksLikeAHeadingStays(t *testing.T) {
	in := strings.Join([]string{
		`--`,
		`-- Database "app" dump`,
		`--`,
		`CREATE DATABASE app WITH TEMPLATE = template0;`,
		`\connect app`,
		`COPY public.lines (text) FROM stdin;`,
		`-- Database "app" dump`,
		`CREATE DATABASE app WITH TEMPLATE = template0;`,
		`\.`,
		``,
	}, "\n")
	out, err := io.ReadAll(withoutWhatIsThere(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", out)
	if strings.Count(string(out), "CREATE DATABASE app") != 1 {
		t.Errorf("a row was removed from a table")
	}
}

// Doctor's way out of a stray, as it is said, on a service that keeps an old volume.
func TestFinishNeverForcesARemoval(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	stray := "moorage-svc-" + strings.ToLower(strings.TrimPrefix(rec.ID, "svc_")) + "-restored-1"
	f.flag("volume-"+stray, true)
	next := rec
	next.Volume = container.ServiceRestoredVolume(rec.ID, 2)
	f.flag("volume-"+next.Volume, true)
	if err := f.db.SetServiceMoved(ctx, rec.ID, next, rec); err != nil {
		t.Fatal(err)
	}
	_, problems := f.m.Problems(ctx)
	t.Logf("doctor: %v", problems)
	if err := f.m.FinishUpgrade(ctx, "shop", "production", "db"); err != nil {
		t.Fatal(err)
	}
	_, oldThere := os.Stat(filepath.Join(f.dir, "volume-"+rec.Volume))
	t.Logf("the volume kept to go back to is there: %v", oldThere == nil)
	// With -f, Podman would remove what is using it.
	if strings.Contains(f.asked(), "volume rm -f") || !strings.Contains(f.asked(), "volume rm "+stray) {
		t.Errorf("%s", f.asked())
	}
}

// Filter on what pg_dumpall of 17.6, 18 write, as far as its source says.
func TestTheFilterOnWhatADumpLooksLike(t *testing.T) {
	in := `--
-- PostgreSQL database cluster dump
--

\restrict abc

SET default_transaction_read_only = off;

SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;

--
-- Roles
--

CREATE ROLE app;
ALTER ROLE app WITH SUPERUSER INHERIT CREATEROLE CREATEDB LOGIN REPLICATION BYPASSRLS PASSWORD 'SCRAM-SHA-256$x';
CREATE ROLE other;
ALTER ROLE other WITH NOSUPERUSER;

--
-- User Configurations
--

\unrestrict abc

--
-- Databases
--

--
-- Database "template1" dump
--

\connect template1

--
-- PostgreSQL database dump
--

\restrict d1

SET statement_timeout = 0;

--
-- PostgreSQL database dump complete
--

\unrestrict d1

--
-- Database "app" dump
--

--
-- PostgreSQL database dump
--

\restrict d2

SET statement_timeout = 0;

--
-- Name: app; Type: DATABASE; Schema: -; Owner: app
--

CREATE DATABASE app WITH TEMPLATE = template0 ENCODING = 'UTF8' LOCALE_PROVIDER = libc LOCALE = 'en_US.utf8';


ALTER DATABASE app OWNER TO app;

\unrestrict d2
\connect app
\restrict d2

SET statement_timeout = 0;

COPY public.people (name, email) FROM stdin;
CREATE ROLE app;
\.

--
-- PostgreSQL database dump complete
--

\unrestrict d2

--
-- Database "other" dump
--

\restrict d3

CREATE DATABASE other WITH TEMPLATE = template0 ENCODING = 'UTF8' LOCALE_PROVIDER = libc LOCALE = 'en_US.utf8';


ALTER DATABASE other OWNER TO app;

\unrestrict d3
\connect other
\restrict d3

CREATE TABLE a (b text);

\unrestrict d3

--
-- Database "postgres" dump
--

\connect postgres

\restrict d4

\unrestrict d4

--
-- PostgreSQL database cluster dump complete
--

`
	out, err := io.ReadAll(withoutWhatIsThere(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	want := strings.Replace(in, "CREATE ROLE app;\nALTER ROLE app", "ALTER ROLE app", 1)
	want = strings.Replace(want, "CREATE DATABASE app WITH TEMPLATE = template0 ENCODING = 'UTF8' LOCALE_PROVIDER = libc LOCALE = 'en_US.utf8';\n", "", 1)
	if got != want {
		t.Errorf("it changed what it should not:\n%s", got)
	}
}

// A backup waits for the lock of what is asleep with no regard for the server stopping, and a server stopping waits for a backup.
func TestABackupBehindARestoreDoesNotHoldUpStopping(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.makeProductionWithDump()
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	unlock := f.m.lock("shop/production") // a restore that has hours to go, whose context the server stopping does not end
	done := make(chan struct{})
	go func() {
		f.m.DumpAll(ctx)
		close(done)
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Errorf("three seconds after the server began to stop, the backup is still waiting for the lock")
	}
	unlock()
	<-done
}

// New in this commit, as a dump of what is asleep no longer holds the lock: one still waiting for what it started to answer finds what a restore has started in its place, of the same name, with a part of its dump in it. The restore goes well, and the part replaces the good dump, with nothing said.
func TestADumpOfWhatARestoreIsLoadingIsNotKept(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.o.DataDir = f.dir
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	f.m.o.AllocPort = func(context.Context) (int, error) { return port, nil }
	plan := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{"files": {Type: manifest.ServiceS3, Env: "S3", Previews: "own"}}}
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", plan, io.Discard); err != nil {
		t.Fatal(err)
	}
	rec, err := f.db.Service(ctx, "shop", "production", "files")
	if err != nil {
		t.Fatal(err)
	}
	good := reviewTar(t, "s3/app/all-of-it", "everything it held")
	dump := f.m.dumpPath(rec)
	os.MkdirAll(filepath.Dir(dump), 0o700)
	if err := os.WriteFile(dump, good, 0o600); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(f.dir, "part.tar")
	if err := os.WriteFile(part, reviewTar(t, "s3/app/a-part", "a part"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.patchEngine(`if [ "$1" = exec ] && [ "$2" = -i ]; then cat > /dev/null; sleep 3; exit 0; fi
if [ "$1" = exec ] && echo "$*" | grep -q "tar -C /data -cf"; then cat "` + part + `"; exit 0; fi
if [ "$1" = exec ] && echo "$*" | grep -q wget && [ -e "` + f.dir + `/slow-ready" ]; then rm -f "` + f.dir + `/slow-ready"; sleep 2; exit 0; fi`)
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	f.flag("slow-ready", true)
	f.m.o.ReadyLimit = 10 * time.Second
	done := make(chan []string, 1)
	go func() { done <- f.m.DumpAll(ctx) }()
	time.Sleep(800 * time.Millisecond)
	restoreErr := f.m.Restore(ctx, "shop", "production", "files")
	problems := <-done
	now, _ := f.db.Service(ctx, "shop", "production", "files")
	have, _ := os.ReadFile(dump)
	t.Logf("restore: %v; dump problems: %v; pending=%v", restoreErr, problems, now.PendingLoad)
	if !bytes.Equal(have, good) {
		t.Errorf("a part of what was being put back replaced the good dump, and the backup goes on with it: %q", strings.TrimRight(string(have[:60]), "\x00"))
	}
}

// What the doctor says of a preview's stray, run as it is said: the command line's --env is production unless told otherwise, so that this is what the command does.
func TestWhatTheDoctorSaysToRunNamesItsEnvironment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.makeProductionWithDump()
	f.m.o.GateFor = func(set, asked, kind string) (string, string) { return "login", "setting" }
	if _, err := f.db.CreateEnvironment(ctx, "shop", "topic", store.KindPreview, "topic"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Ensure(ctx, "shop", "topic", store.KindPreview, "me", "", postgresPlan(), io.Discard); err != nil {
		t.Fatal(err)
	}
	preview, err := f.db.Service(ctx, "shop", "topic", "db")
	if err != nil {
		t.Fatal(err)
	}
	// Production has been upgraded, and keeps what it held to go back to.
	next := rec
	next.Volume = container.ServiceUpgradeVolume(rec.ID, "19")
	f.flag("volume-"+next.Volume, true)
	if err := f.db.SetServiceMoved(ctx, rec.ID, next, rec); err != nil {
		t.Fatal(err)
	}
	// The preview has a stray.
	stray := container.ServiceRestoredVolume(preview.ID, 1)
	f.flag("volume-"+stray, true)
	_, problems := f.m.Problems(ctx)
	if !slices.ContainsFunc(problems, func(p string) bool {
		return strings.Contains(p, stray) && strings.Contains(p, "moor services upgrade db --app shop --env topic --finish --yes")
	}) {
		t.Errorf("what the doctor says to run: %v", problems)
	}
}
