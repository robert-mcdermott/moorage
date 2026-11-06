// SPDX-License-Identifier: Apache-2.0

package services

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// plainKeyring is no keyring at all, which is enough to see that a password goes through one.
type plainKeyring struct{}

func (plainKeyring) Seal(_ context.Context, _, _, _, value string) ([]byte, error) {
	return []byte("sealed:" + value), nil
}

func (plainKeyring) Open(_ context.Context, _, _, _ string, sealed []byte) (string, error) {
	return strings.TrimPrefix(string(sealed), "sealed:"), nil
}

// fixture is a Manager over a real database, with a program that answers as an engine does, and writes down what it is asked and what it was given.
type fixture struct {
	t      *testing.T
	m      *Manager
	db     *store.DB
	dir    string
	calls  string
	source *container.Source
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.EnsureApp(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, db: db, dir: dir, calls: filepath.Join(dir, "calls")}
	bin := filepath.Join(dir, "podman")
	script := `#!/bin/sh
echo "$*" >> "` + f.calls + `"
echo "POSTGRES_PASSWORD=$POSTGRES_PASSWORD" >> "` + f.calls + `.env"
if [ -e "` + dir + `/down" ] && [ "$1" != "--version" ]; then echo "Cannot connect to Podman" >&2; exit 125; fi
# The last word of a command is the container that it is about, but for run and exec.
for last; do :; done
case "$1" in
  --version) echo "podman version 5" ;;
  image) cat "` + dir + `/image.json" ;;
  container) if [ -e "` + dir + `/running-$last" ]; then cat "` + dir + `/running-$last"; else echo "Error: no such container" >&2; exit 1; fi ;;
  run)
    if [ -e "` + dir + `/no-run" ]; then echo "it would not run" >&2; exit 1; fi
    state="true"
    case "$*" in *io.moorage.private=true*) state="true true" ;; esac
    while [ $# -gt 0 ]; do if [ "$1" = --name ]; then echo "$state" > "` + dir + `/running-$2"; fi; shift; done
    ;;
  rm|stop) rm -f "` + dir + `/running-$last" ;;
  stats) for file in "` + dir + `"/running-*; do [ -e "$file" ] && printf '%s\t1.50%%\t30MiB / 1GiB\n' "${file##*/running-}"; done; exit 0 ;;
  exec)
    if [ -e "` + dir + `/never-ready" ]; then exit 2; fi
    if [ -e "` + dir + `/no-dump" ] && echo "$*" | grep -q pg_dump; then echo "pg_dump: it went wrong" >&2; exit 1; fi
    if [ "$2" = -i ]; then cat >> "` + dir + `/loaded"; echo "--- $*" >> "` + dir + `/loaded"; fi
    case "$*" in
      *pg_dump*) echo "a dump of it"; if [ ! -e "` + dir + `/cut-short" ]; then echo "-- PostgreSQL database cluster dump complete"; fi ;;
      *"tar -C /data -cf"*) echo "a tar of it" ;;
      *query_to_xml*) echo "3 rows in 1 tables, of databases app, postgres" ;;
    esac
    ;;
  logs) echo "what it said" ;;
  volume|network)
    if [ "$2" = ls ]; then
      id=$(printf '%s' "${last##*=svc_}" | tr 'A-Z' 'a-z')
      for file in "` + dir + `"/volume-*"$id"*; do [ -e "$file" ] && echo "${file##*/volume-}"; done
      exit 0
    fi
    if [ "$2" = inspect ] && [ -e "` + dir + `/odd-$1" ]; then echo "something odd happened" >&2; exit 1; fi
    if [ "$2" = inspect ] && [ ! -e "` + dir + `/$1-$last" ]; then echo "no such $1" >&2; exit 1; fi
    if [ "$2" = inspect ]; then echo "$last"; fi
    if [ "$2" = create ]; then touch "` + dir + `/$1-$last"; fi
    if [ "$2" = rm ]; then rm -f "` + dir + `/$1-$last"; fi
    ;;
esac
`
	f.image(18)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.source = container.NewSource(container.SourceOptions{Setting: bin, Kept: time.Nanosecond, NewDriver: func(e *container.Engine) *container.Driver {
		return container.New(e, "test", slog.New(slog.DiscardHandler))
	}})
	port := 40000
	f.m = New(Options{
		DB: db, Keyring: plainKeyring{}, Engines: f.source, Log: slog.New(slog.DiscardHandler), ReadyLimit: 2 * time.Second,
		AllocPort:   func(context.Context) (int, error) { port++; return port, nil },
		ReleasePort: func(int) {},
	})
	return f
}

// image says what the engine says of any image.
func (f *fixture) image(major int) {
	f.t.Helper()
	pgdata, volume := fmt.Sprintf("/var/lib/postgresql/%d/docker", major), "/var/lib/postgresql"
	if major < 18 {
		pgdata, volume = "/var/lib/postgresql/data", "/var/lib/postgresql/data"
	}
	doc := fmt.Sprintf(`[{"Id":"abc","RepoDigests":["docker.io/library/postgres@sha256:abc%d"],"Size":1,"Config":{"Env":["PATH=/x","PG_MAJOR=%d","PGDATA=%s"],"Volumes":{"%s":{}}}}]`, major, major, pgdata, volume)
	if err := os.WriteFile(filepath.Join(f.dir, "image.json"), []byte(doc), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// running says whether a service's own container is running, as an app would find it.
func (f *fixture) running(rec store.Service) bool {
	_, err := os.Stat(filepath.Join(f.dir, "running-"+container.ServiceName(rec.ID)))
	return err == nil
}

func (f *fixture) flag(name string, on bool) {
	f.t.Helper()
	path := filepath.Join(f.dir, name)
	if !on {
		os.Remove(path)
		return
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) asked() string {
	data, _ := os.ReadFile(f.calls)
	return string(data)
}

func postgresPlan() manifest.Plan {
	return manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{
		"db": {Type: manifest.ServicePostgres, Version: "18", Env: "DATABASE_URL", Previews: "own"},
	}}
}

func TestAPlanWithNoServicesAsksNothingOfAnEngine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plain := manifest.Plan{Kind: manifest.KindProcess}
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", plain, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := f.m.CheckEngine(ctx, "shop", "production", store.KindProduction, plain); err != nil {
		t.Fatal(err)
	}
	addresses, _, network, err := f.m.Addresses(ctx, "shop", "production", plain, false)
	if err != nil || len(addresses) != 0 || network != "" {
		t.Errorf("%v %q %v", addresses, network, err)
	}
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Wake(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Blocks(ctx, "shop", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.m.RemoveApp(ctx, "shop", false); err != nil {
		t.Fatal(err)
	}
	f.m.look(ctx)
	if got := f.asked(); got != "" {
		t.Errorf("it asked:\n%s", got)
	}
	// And so does none at all.
	var none *Manager
	if err := none.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", postgresPlan(), io.Discard); err != nil || none.Any(ctx) {
		t.Errorf("no manager: %v", err)
	}
}

func TestMakingAService(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plan := postgresPlan()
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", plan, io.Discard); err != nil {
		t.Fatal(err)
	}
	rec, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Version != "18" || rec.Digest != "docker.io/library/postgres@sha256:abc18" || rec.State != store.ServiceRunning || rec.EnvVar != "DATABASE_URL" || rec.Port != 40001 || rec.Mount != "/var/lib/postgresql" {
		t.Errorf("%+v", rec)
	}
	asked := f.asked()
	for _, want := range []string{
		"pull docker.io/library/postgres:18-trixie",
		"volume create",
		"network create",
		"--restart=no",
		"--stop-signal SIGINT",
		"--stop-timeout 120",
		"-p 127.0.0.1:40001:5432",
	} {
		if !strings.Contains(asked, want) {
			t.Errorf("no %q in:\n%s", want, asked)
		}
	}
	if !strings.Contains(asked, ":/var/lib/postgresql ") {
		t.Errorf("not on the path for 18:\n%s", asked)
	}
	// Its password goes through the environment, and not on a command line that anyone on the machine can read.
	password := strings.TrimPrefix(string(rec.Sealed), "sealed:")
	if len(password) < 24 {
		t.Errorf("a short password: %q", password)
	}
	if strings.Contains(asked, password) {
		t.Errorf("its password is on a command line:\n%s", asked)
	}
	env, _ := os.ReadFile(f.calls + ".env")
	if !strings.Contains(string(env), "POSTGRES_PASSWORD="+password) {
		t.Errorf("it did not get its password:\n%s", env)
	}

	// A native app gets the loopback address, and one in a container a name on a network.
	addresses, _, network, err := f.m.Addresses(ctx, "shop", "production", plan, false)
	if err != nil || network != "" || addresses["DATABASE_URL"] != "postgres://app:"+password+"@127.0.0.1:40001/app?sslmode=disable" {
		t.Errorf("native: %v %q %v", addresses, network, err)
	}
	addresses, _, network, err = f.m.Addresses(ctx, "shop", "production", plan, true)
	if err != nil || !strings.HasPrefix(network, "moorage-env-") || addresses["DATABASE_URL"] != "postgres://app:"+password+"@"+container.ServiceName(rec.ID)+":5432/app?sslmode=disable" {
		t.Errorf("in a container: %v %q %v", addresses, network, err)
	}
	// A release that does not name it does not get it, so that going back behaves.
	if addresses, _, _, _ := f.m.Addresses(ctx, "shop", "production", manifest.Plan{Services: map[string]manifest.ServicePlan{"other": {Type: manifest.ServicePostgres, Env: "OTHER_URL"}}}, false); len(addresses) != 0 {
		t.Errorf("%v", addresses)
	}

	// Deploying again makes nothing, and pulls nothing: an update is something that you ask for.
	before := f.asked()
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", plan, io.Discard); err != nil {
		t.Fatal(err)
	}
	if again := strings.TrimPrefix(f.asked(), before); strings.Contains(again, "pull") || strings.Contains(again, "volume create") {
		t.Errorf("deploying again:\n%s", again)
	}
	if list, _ := f.db.Services(ctx, "shop", "production"); len(list) != 1 {
		t.Errorf("%+v", list)
	}
	// A major version is not something to change under its data.
	newer := postgresPlan()
	newer.Services["db"] = manifest.ServicePlan{Type: manifest.ServicePostgres, Version: "19", Env: "DATABASE_URL"}
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", newer, io.Discard); err == nil || !strings.Contains(err.Error(), "holds data of PostgreSQL 18") {
		t.Errorf("a newer major: %v", err)
	}

	// With what it needs gone, it says so, and leaves things as they are.
	f.flag("down", true)
	if err := f.m.Blocks(ctx, "shop", ""); err == nil {
		t.Error("removing it with the engine down")
	}
	if err := f.m.Sleep(ctx, "shop", "production"); err == nil {
		t.Error("stopping it with the engine down")
	}
	if got, _ := f.db.Service(ctx, "shop", "production", "db"); got.State != store.ServiceRunning {
		t.Errorf("%+v", got)
	}
	f.m.look(ctx)
	f.flag("down", false)

	// Sleep stops it, and keeps what it holds, and waking starts it.
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.db.Service(ctx, "shop", "production", "db"); got.State != store.ServiceStopped {
		t.Errorf("%+v", got)
	}
	if strings.Contains(f.asked(), "volume rm") {
		t.Error("going to sleep removed its volume")
	}
	f.m.keepUp(ctx, rec)
	if f.running(rec) {
		t.Error("it started what should be asleep")
	}
	if err := f.m.Wake(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.db.Service(ctx, "shop", "production", "db"); got.State != store.ServiceRunning {
		t.Errorf("%+v", got)
	}

	// One that has stopped by itself is started again.
	os.Remove(filepath.Join(f.dir, "running-"+container.ServiceName(rec.ID)))
	f.m.keepUp(ctx, rec)
	if !f.running(rec) {
		t.Error("it was not started again")
	}

	// What was read before it went to sleep, or went, does not bring it back.
	stale, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	f.m.keepUp(ctx, stale)
	if f.running(stale) {
		t.Error("it started what had just gone to sleep")
	}
	if err := f.m.Wake(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}

	// Removing it goes with what it holds.
	if err := f.m.Remove(ctx, "shop", "production", "db", "me"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.asked(), "volume rm "+rec.Volume) {
		t.Errorf("%s", f.asked())
	}
	if list, _ := f.db.Services(ctx, "shop", "production"); len(list) != 0 {
		t.Errorf("%+v", list)
	}
	f.m.keepUp(ctx, stale)
	if f.running(stale) {
		t.Error("it started what had just been removed")
	}
}

func TestAServiceThatDoesNotComeUpLeavesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flag("never-ready", true)
	err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", postgresPlan(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "did not become ready") || !strings.Contains(err.Error(), "what it said") {
		t.Errorf("%v", err)
	}
	if list, _ := f.db.Services(ctx, "shop", "production"); len(list) != 0 {
		t.Errorf("%+v", list)
	}
	if !strings.Contains(f.asked(), "volume rm") {
		t.Errorf("it left its volume:\n%s", f.asked())
	}
}

func TestWhatAnEnvironmentGets(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plan := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{
		"db":    {Type: manifest.ServicePostgres, Version: "18", Env: "DATABASE_URL", Previews: "own"},
		"stats": {Type: manifest.ServicePostgres, Version: "16", Env: "STATS_URL", Previews: "none"},
		"files": {Type: manifest.ServiceS3, Env: "S3_ENDPOINT"},
	}}
	got := func(env, kind string) string {
		t.Helper()
		wanted, err := f.m.Wanted(ctx, "shop", env, kind, plan)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(sortedNames(wanted), " ")
	}
	if have := got("production", store.KindProduction); have != "db files stats" {
		t.Error(have)
	}
	// A preview has what is not for production alone.
	if _, err := f.db.CreateEnvironment(ctx, "shop", "topic", store.KindPreview, "topic"); err != nil {
		t.Fatal(err)
	}
	if have := got("topic", store.KindPreview); have != "db files" {
		t.Error(have)
	}
	// Production can use a real database by setting its own address, and then needs no engine.
	if _, err := f.db.ChangeVars(ctx, "shop", "production", store.VarChange{By: "me", Set: []store.Var{{Name: "DATABASE_URL", Value: "postgres://elsewhere"}}}); err != nil {
		t.Fatal(err)
	}
	if have := got("production", store.KindProduction); have != "files stats" {
		t.Error(have)
	}
	// Set for every environment, it is weaker than a service.
	if _, err := f.db.ChangeVars(ctx, "shop", "production", store.VarChange{AllEnvs: true, By: "me", Set: []store.Var{{Name: "STATS_URL", Value: "postgres://elsewhere"}}}); err != nil {
		t.Fatal(err)
	}
	if have := got("production", store.KindProduction); have != "files stats" {
		t.Error(have)
	}
}

func TestAServiceOfS3(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// What stands in for it, on the port that it will publish.
	var made []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=moorage/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		made = append(made, r.Method+" "+r.URL.Path)
	}))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	f.m.o.AllocPort = func(context.Context) (int, error) { return port, nil }
	plan := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{"files": {Type: manifest.ServiceS3, Env: "S3_ENDPOINT", Previews: "own"}}}
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", plan, io.Discard); err != nil {
		t.Fatal(err)
	}
	rec, err := f.db.Service(ctx, "shop", "production", "files")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Version != "1.10.0" || rec.Type != manifest.ServiceS3 {
		t.Errorf("%+v", rec)
	}
	if strings.Join(made, " ") != "PUT /app" {
		t.Errorf("what it asked of it: %v", made)
	}
	asked := f.asked()
	for _, want := range []string{
		"pull docker.io/versity/versitygw:v1.10.0", "VGW_META_SIDECAR=/data/meta", "VGW_BACKEND_ARGS=/data/s3", "VGW_HEALTH=/health", ":/data/s3 ", ":/data/meta ",
		fmt.Sprintf("-p 127.0.0.1:%d:7070", port), "/health",
	} {
		if !strings.Contains(asked, want) {
			t.Errorf("no %q in:\n%s", want, asked)
		}
	}
	password := strings.TrimPrefix(string(rec.Sealed), "sealed:")
	if strings.Contains(asked, password) {
		t.Errorf("its secret is on a command line:\n%s", asked)
	}

	// What is secret is kept apart from what is not, and what is not goes under the names that clients look for.
	secret, plain, _, err := f.m.Addresses(ctx, "shop", "production", plan, false)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	if plain["S3_ENDPOINT"] != endpoint || plain["S3_BUCKET"] != "app" || plain["S3_ACCESS_KEY_ID"] != "moorage" || plain["AWS_ENDPOINT_URL"] != endpoint || plain["AWS_REGION"] != "us-east-1" {
		t.Errorf("%v", plain)
	}
	if secret["S3_SECRET_ACCESS_KEY"] != password || secret["AWS_SECRET_ACCESS_KEY"] != password || len(secret) != 2 {
		t.Errorf("%v", secret)
	}
	// With a name of its own, it leaves those names for another.
	named := manifest.Plan{Services: map[string]manifest.ServicePlan{"files": {Type: manifest.ServiceS3, Env: "UPLOADS"}}}
	secret, plain, _, err = f.m.Addresses(ctx, "shop", "production", named, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(plain["UPLOADS_ENDPOINT"], ":7070") || !strings.HasPrefix(plain["UPLOADS_ENDPOINT"], "http://moorage-svc-") || plain["AWS_REGION"] != "" || secret["UPLOADS_SECRET_ACCESS_KEY"] != password {
		t.Errorf("%v %v", plain, secret)
	}
	// A service cannot become another kind of thing.
	other := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{"files": {Type: manifest.ServicePostgres, Version: "18", Env: "DATABASE_URL"}}}
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", other, io.Discard); err == nil || !strings.Contains(err.Error(), "a service of s3") {
		t.Errorf("%v", err)
	}
	// Both of its volumes go with it.
	if err := f.m.Remove(ctx, "shop", "production", "files", "me"); err != nil {
		t.Fatal(err)
	}
	if asked := f.asked(); !strings.Contains(asked, "volume rm "+rec.Volume) || !strings.Contains(asked, "volume rm "+container.ServiceMetadataVolume(rec.ID)) {
		t.Errorf("%s", asked)
	}
}

func TestSmallThings(t *testing.T) {
	mounts := map[string]string{
		`{"Env":["PGDATA=/var/lib/postgresql/18/docker"],"Volumes":{"/var/lib/postgresql":{}}}`: "/var/lib/postgresql",
		`{"Env":["PGDATA=/var/lib/postgresql/data"],"Volumes":{"/var/lib/postgresql/data":{}}}`: "/var/lib/postgresql/data",
		`{"Env":["PGDATA=/srv/pg/data"]}`:                             "/srv/pg/data",
		`{"Env":["PGDATA=/srv/pg/data"],"Volumes":{"/elsewhere":{}}}`: "/srv/pg/data",
	}
	for config, want := range mounts {
		var info container.ImageInfo
		if err := json.Unmarshal([]byte(`{"Config":`+config+`}`), &info); err != nil {
			t.Fatal(err)
		}
		if got := dataMount(info, "18"); got != want {
			t.Errorf("%s: %s", config, got)
		}
	}
	if got := dataMount(container.ImageInfo{}, "14"); got != "/var/lib/postgresql/data" {
		t.Error(got)
	}
	if got := dataMount(container.ImageInfo{}, "18"); got != "/var/lib/postgresql" {
		t.Error(got)
	}
	// What a new service has already is left out of a dump, and rows that look like it are not.
	dump := "CREATE ROLE app;\nALTER ROLE app WITH SUPERUSER;\nCREATE ROLE other;\n--\n-- Database \"app\" dump\n--\nCREATE DATABASE app WITH TEMPLATE = template0;\n\\connect app\nCOPY public.t (a) FROM stdin;\nCREATE ROLE app;\nCREATE DATABASE app WITH x\n\\.\nSELECT 1;\n"
	want := "ALTER ROLE app WITH SUPERUSER;\nCREATE ROLE other;\n--\n-- Database \"app\" dump\n--\n\\connect app\nCOPY public.t (a) FROM stdin;\nCREATE ROLE app;\nCREATE DATABASE app WITH x\n\\.\nSELECT 1;\n"
	if got, err := io.ReadAll(withoutWhatIsThere(strings.NewReader(dump))); err != nil || string(got) != want {
		t.Errorf("%q %v", got, err)
	}
	for image, want := range map[string]string{
		"postgres:18":            "docker.io/library/postgres:18",
		"pgvector/pgvector:pg18": "docker.io/pgvector/pgvector:pg18",
		"ghcr.io/acme/db:1":      "ghcr.io/acme/db:1",
		"localhost:5000/db:1":    "localhost:5000/db:1",
		"localhost/moorage/db:1": "localhost/moorage/db:1",
	} {
		if got := qualified(image); got != want {
			t.Errorf("%s: %s", image, got)
		}
	}
	var info container.ImageInfo
	if err := json.Unmarshal([]byte(`{"Config":{"Env":["PG_VERSION=18.6","PG_MAJOR=18"]}}`), &info); err != nil {
		t.Fatal(err)
	}
	if got := postgresMajor(info); got != "18" {
		t.Error(got)
	}
	if got := container.ServiceName(ids.Service + "_01ABC"); got != "moorage-svc-01abc" {
		t.Error(got)
	}
	a, err := newPassword()
	if err != nil || len(a) < 32 || strings.ContainsAny(a, "@:/?#%") {
		t.Errorf("%q %v", a, err)
	}
}

// Data: a dump for a backup, what comes back on a fresh volume, what a preview may be seeded with, and moving between versions.
func TestDumpsAndWhatComesOfThem(t *testing.T) {
	const dumped = "a dump of it\n-- PostgreSQL database cluster dump complete\n"
	f := newFixture(t)
	ctx := context.Background()
	f.m.o.DataDir = f.dir
	f.m.o.GateFor = func(set, asked, kind string) (string, string) {
		if set != "" {
			return set, "setting"
		}
		return "public", "default"
	}
	plan := postgresPlan()
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", plan, io.Discard); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(f.dir, "data", "shop", ".services", "production", "db.sql")

	// A preview's is left out of a backup, as it is not meant to last.
	if _, err := f.db.CreateEnvironment(ctx, "shop", "topic", store.KindPreview, "topic"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Ensure(ctx, "shop", "topic", store.KindPreview, "me", "", plan, io.Discard); err != nil {
		t.Fatal(err)
	}
	if problems := f.m.DumpAll(ctx); len(problems) != 0 {
		t.Fatalf("%v", problems)
	}
	data, err := os.ReadFile(dump)
	if err != nil || string(data) != dumped {
		t.Errorf("%q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "data", "shop", ".services", "topic", "db.sql")); err == nil {
		t.Error("a preview's was dumped")
	}
	// One that fails says so, and leaves the last one as it was.
	f.flag("no-dump", true)
	problems := f.m.DumpAll(ctx)
	if len(problems) != 1 || !strings.Contains(problems[0], "was not dumped") || !strings.Contains(problems[0], "it went wrong") {
		t.Errorf("%v", problems)
	}
	if data, _ := os.ReadFile(dump); string(data) != dumped {
		t.Errorf("the last one: %q", data)
	}
	if entries, _ := os.ReadDir(filepath.Dir(dump)); len(entries) != 1 {
		t.Errorf("what it left: %v", entries)
	}
	f.flag("no-dump", false)

	// One that is asleep is dumped as well, where no app can reach it, as nothing says that its last one is current.
	rec, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dump); err != nil {
		t.Fatal(err)
	}
	if problems := f.m.DumpAll(ctx); len(problems) != 0 {
		t.Errorf("%v", problems)
	}
	if data, err := os.ReadFile(dump); err != nil || string(data) != dumped {
		t.Errorf("asleep: %q %v", data, err)
	}
	if f.running(rec) {
		t.Error("what dumped it left it running")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "running-"+container.ServiceName(rec.ID))); err == nil {
		t.Error("what dumped it left it running, where no app can reach it")
	}
	// It has one name, so that there cannot be two of it on one volume.
	if strings.Contains(f.asked(), "-private") {
		t.Error("it was given another name")
	}
	// With its volume gone, as on a new machine after a restore, a backup leaves its only dump as it is, and does not make a volume to dump.
	f.flag("volume-"+rec.Volume, false)
	if problems := f.m.DumpAll(ctx); len(problems) != 0 {
		t.Errorf("%v", problems)
	}
	if data, _ := os.ReadFile(dump); string(data) != dumped {
		t.Errorf("the only dump: %q", data)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "volume-"+rec.Volume)); err == nil {
		t.Error("it made a volume to dump")
	}
	f.flag("loaded", false)
	if err := f.m.Wake(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}

	if loaded, _ := os.ReadFile(filepath.Join(f.dir, "loaded")); !strings.Contains(string(loaded), "a dump of it") {
		t.Errorf("waking on no volume: %q", loaded)
	}

	// On a fresh volume, as after a restore to a new machine, what it held comes back from its dump, before anything can reach it.
	gone := func() {
		t.Helper()
		f.flag("running-"+container.ServiceName(rec.ID), false)
		f.flag("volume-"+rec.Volume, false)
		f.flag("loaded", false)
	}
	gone()
	f.flag("no-run", true)
	// With it failing to start, for a start: nothing is left to think that it holds anything.
	f.m.keepUp(ctx, rec)
	if f.running(rec) {
		t.Error("it is running")
	}
	f.flag("no-run", false)
	if got, _ := f.db.Service(ctx, "shop", "production", "db"); !got.PendingLoad {
		t.Errorf("what is pending: %+v", got)
	}
	f.flag("never-ready", true)
	f.m.keepUp(ctx, rec)
	f.flag("never-ready", false)
	if f.running(rec) {
		t.Error("it is running, with nothing in it")
	}
	if got := f.m.DumpAll(ctx); len(got) != 1 || !strings.Contains(got[0], "has not been put back") {
		t.Errorf("a dump of what is not back yet: %v", got)
	}
	if data, _ := os.ReadFile(dump); string(data) != dumped {
		t.Errorf("the only good dump: %q", data)
	}
	f.m.keepUp(ctx, rec)
	loaded, err := os.ReadFile(filepath.Join(f.dir, "loaded"))
	if err != nil || !strings.Contains(string(loaded), "a dump of it") {
		t.Errorf("what it loaded: %q %v", loaded, err)
	}
	// A dump holds the password of what it came from, and this may not be that.
	if !strings.Contains(string(loaded), "ALTER ROLE app WITH PASSWORD '"+strings.TrimPrefix(string(rec.Sealed), "sealed:")+"'") {
		t.Errorf("its own password: %q", loaded)
	}
	if !f.running(rec) {
		t.Error("it did not come up after that")
	}
	if got, _ := f.db.Service(ctx, "shop", "production", "db"); got.PendingLoad {
		t.Errorf("what is pending: %+v", got)
	}
	f.flag("loaded", false)
	// And on the volume that it had, nothing is loaded over it.
	f.flag("running-"+container.ServiceName(rec.ID), false)
	f.m.keepUp(ctx, rec)
	if _, err := os.Stat(filepath.Join(f.dir, "loaded")); err == nil {
		t.Error("it loaded a dump over what it held")
	}
	// Nor if the engine cannot say whether it has one: only saying that it has none means that.
	f.flag("running-"+container.ServiceName(rec.ID), false)
	f.flag("odd-volume", true)
	f.m.keepUp(ctx, rec)
	f.flag("odd-volume", false)
	if _, err := os.Stat(filepath.Join(f.dir, "loaded")); err == nil {
		t.Error("it loaded a dump, not knowing")
	}
	f.m.keepUp(ctx, rec)
	if !f.running(rec) {
		t.Error("it did not come up")
	}

	// A preview is seeded only if it says so, and only behind a gate.
	appDir := filepath.Join(f.dir, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "sanitize.sql"), []byte("update users set email = 'x';"), 0o644); err != nil {
		t.Fatal(err)
	}
	seeded := manifest.Plan{Kind: manifest.KindProcess, Services: map[string]manifest.ServicePlan{
		"db": {Type: manifest.ServicePostgres, Version: "18", Env: "DATABASE_URL", Previews: "own", Seed: "production", Sanitize: "sanitize.sql"},
	}}
	make := func(env string) string {
		t.Helper()
		var log strings.Builder
		if err := f.m.Ensure(ctx, "shop", env, store.KindPreview, "me", appDir, seeded, &log); err != nil {
			t.Fatal(err)
		}
		return log.String()
	}
	if _, err := f.db.CreateEnvironment(ctx, "shop", "open", store.KindPreview, "open"); err != nil {
		t.Fatal(err)
	}
	if log := make("open"); !strings.Contains(log, "anyone can reach this preview") {
		t.Errorf("one with no gate:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "loaded")); err == nil {
		t.Error("it copied production's data to where anyone can reach it")
	}
	// A pull request's branch with nothing to say that it is not from a fork, as on its first deployment.
	if _, err := f.db.CreateEnvironment(ctx, "shop", "pr-7", store.KindPreview, "pr-7"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetEnvironmentAccess(ctx, "shop", "pr-7", store.EnvAccess{Mode: "login"}); err != nil {
		t.Fatal(err)
	}
	if log := make("pr-7"); !strings.Contains(log, "may come from a fork") {
		t.Errorf("a pull request:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "loaded")); err == nil {
		t.Error("it copied production's data to a fork's preview")
	}
	if _, err := f.db.CreatePreviewAlone(ctx, "shop", "agent-work"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetEnvironmentAccess(ctx, "shop", "agent-work", store.EnvAccess{Mode: "login"}); err != nil {
		t.Fatal(err)
	}
	if log := make("agent-work"); !strings.Contains(log, "takes nothing of production") {
		t.Errorf("an agent's:\n%s", log)
	}
	if _, err := f.db.CreateEnvironment(ctx, "shop", "gated", store.KindPreview, "gated"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetEnvironmentAccess(ctx, "shop", "gated", store.EnvAccess{Mode: "login"}); err != nil {
		t.Fatal(err)
	}
	log := make("gated")
	if !strings.Contains(log, "seeding it") || !strings.Contains(log, "sanitizing it") {
		t.Errorf("one behind a gate:\n%s", log)
	}
	loaded, _ = os.ReadFile(filepath.Join(f.dir, "loaded"))
	if a, b := strings.Index(string(loaded), "a dump of it"), strings.Index(string(loaded), "update users"); a < 0 || b < a {
		t.Errorf("what it loaded, in order: %q", loaded)
	}
	// A build can leave a link there, which is not to be followed out of the app.
	if err := os.Remove(filepath.Join(appDir, "sanitize.sql")); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(f.dir, "master.key")
	if err := os.WriteFile(secret, []byte("the server's secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(appDir, "sanitize.sql")); err != nil {
		t.Fatal(err)
	}
	f.flag("loaded", false)
	if _, err := f.db.CreateEnvironment(ctx, "shop", "linked", store.KindPreview, "linked"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetEnvironmentAccess(ctx, "shop", "linked", store.EnvAccess{Mode: "login"}); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Ensure(ctx, "shop", "linked", store.KindPreview, "me", appDir, seeded, io.Discard); err == nil || !strings.Contains(err.Error(), "sanitize") {
		t.Errorf("a link out: %v", err)
	}
	if loaded, _ := os.ReadFile(filepath.Join(f.dir, "loaded")); strings.Contains(string(loaded), "the server's secret") {
		t.Error("it fed the server's own file to the service")
	}
	if list, _ := f.db.Services(ctx, "shop", "linked"); len(list) != 0 {
		t.Errorf("what it left: %+v", list)
	}
	if err := os.Remove(filepath.Join(appDir, "sanitize.sql")); err != nil {
		t.Fatal(err)
	}

	// If what removes what should not be copied goes wrong, nothing is left of the copy.
	if _, err := f.db.CreateEnvironment(ctx, "shop", "gated2", store.KindPreview, "gated2"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetEnvironmentAccess(ctx, "shop", "gated2", store.EnvAccess{Mode: "login"}); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Ensure(ctx, "shop", "gated2", store.KindPreview, "me", appDir, seeded, io.Discard); err == nil || !strings.Contains(err.Error(), "sanitize") {
		t.Errorf("no script: %v", err)
	}
	if list, _ := f.db.Services(ctx, "shop", "gated2"); len(list) != 0 {
		t.Errorf("%+v", list)
	}

	// Updating takes the newest image of its version, if there is one.
	if digest, err := f.m.Update(ctx, "shop", "production", "db", io.Discard); err != nil || digest != "" {
		t.Errorf("%q %v", digest, err)
	}

	// Upgrading goes only up, and refuses before anything has been stopped.
	prepare := func(version, image string) (*Upgrade, error) {
		return f.m.PrepareUpgrade(ctx, "shop", "production", "db", version, image, io.Discard)
	}
	if _, err := prepare("17", ""); err == nil || !strings.Contains(err.Error(), "not newer") {
		t.Errorf("going down: %v", err)
	}
	if _, err := prepare("18", ""); err == nil || !strings.Contains(err.Error(), "not newer") {
		t.Errorf("staying: %v", err)
	}
	f.image(17)
	if err := f.db.SetServiceImage(ctx, rec.ID, "17", "docker.io/library/postgres:17-trixie", "", "/var/lib/postgresql/data", rec.Volume, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare("18", ""); err == nil || !strings.Contains(err.Error(), "holds PostgreSQL 17") {
		t.Errorf("an image that holds something else: %v", err)
	}
	f.image(18)
	upgrade, err := prepare("18", "")
	if err != nil {
		t.Fatal(err)
	}
	before := f.asked()
	if err := f.m.Upgrade(ctx, upgrade, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "18" || got.OldVolume != rec.Volume || got.Volume == rec.Volume || got.Mount != "/var/lib/postgresql" {
		t.Errorf("%+v", got)
	}
	if _, err := prepare("19", ""); err == nil || !strings.Contains(err.Error(), "--finish") {
		t.Errorf("again before finishing: %v", err)
	}
	if strings.Contains(strings.TrimPrefix(f.asked(), before), "volume rm "+rec.Volume) {
		t.Error("it removed what it held")
	}
	if err := f.m.FinishUpgrade(ctx, "shop", "production", "db"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.TrimPrefix(f.asked(), before), "volume rm "+rec.Volume) {
		t.Error("it kept what it held")
	}
	if got, _ := f.db.Service(ctx, "shop", "production", "db"); got.OldVolume != "" {
		t.Errorf("%+v", got)
	}
	// Another image has to be named, as moving to the official one would lose what it was there for.
	if err := f.db.SetServiceImage(ctx, rec.ID, "18", "docker.io/pgvector/pgvector:pg18", "", "/var/lib/postgresql", got.Volume, ""); err != nil {
		t.Fatal(err)
	}
	f.image(19)
	if _, err := prepare("19", ""); err == nil || !strings.Contains(err.Error(), "with --image") {
		t.Errorf("a custom image: %v", err)
	}
	if u, err := prepare("19", "pgvector/pgvector:pg19"); err != nil || u.next.Image != "docker.io/pgvector/pgvector:pg19" {
		t.Errorf("%+v %v", u, err)
	}
	// Going back to a dump is a thing to ask for. It goes into a volume of its own, and what it held is kept.
	f.flag("loaded", false)
	f.image(18)
	if err := f.db.SetServiceImage(ctx, rec.ID, "18", "docker.io/library/postgres:18-trixie", "", "/var/lib/postgresql", got.Volume, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Restore(ctx, "shop", "production", "db"); err != nil {
		t.Fatal(err)
	}
	if loaded, _ := os.ReadFile(filepath.Join(f.dir, "loaded")); !strings.Contains(string(loaded), "a dump of it") {
		t.Errorf("%q", loaded)
	}
	restored, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	if restored.OldVolume != got.Volume || restored.Volume == got.Volume || !strings.Contains(restored.Volume, "-restored-") {
		t.Errorf("%+v", restored)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "volume-"+got.Volume)); err != nil {
		t.Errorf("what it held: %v", err)
	}
	if err := f.m.FinishUpgrade(ctx, "shop", "production", "db"); err != nil {
		t.Fatal(err)
	}
	// A dump that does not get to the end is not gone back to, or put back, and nothing is lost for it.
	if err := os.WriteFile(dump, []byte("a dump that stops here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Restore(ctx, "shop", "production", "db"); err == nil || !strings.Contains(err.Error(), "not whole") {
		t.Errorf("a dump cut short: %v", err)
	}
	if err := os.WriteFile(dump, []byte(dumped), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetServiceImage(ctx, rec.ID, "18", "docker.io/library/postgres:18-trixie", "", "/var/lib/postgresql", got.Volume, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	// Taking the newest image of something asleep does not wake it.
	f.image(18)
	f.flag("running-"+container.ServiceName(rec.ID), false)
	if _, err := f.m.Update(ctx, "shop", "production", "db", io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.running(rec) {
		t.Error("updating it started it")
	}
	if err := f.m.Wake(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	// A failed wake still means that it should be running, and what keeps it so picks it up.
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	f.flag("down", true)
	if err := f.m.Wake(ctx, "shop", "production"); err == nil {
		t.Error("it woke, with the engine down")
	}
	f.flag("down", false)
	if got, _ := f.db.Service(ctx, "shop", "production", "db"); got.State != store.ServiceRunning {
		t.Errorf("after a failed wake: %+v", got)
	}
	// A limit that changes reaches it, and what it holds stays.
	limited := postgresPlan()
	limited.Services["db"] = manifest.ServicePlan{Type: manifest.ServicePostgres, Version: "18", Env: "DATABASE_URL", Memory: 1 << 30}
	f.flag("running-"+container.ServiceName(rec.ID), false)
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", limited, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.asked(), "--memory 1073741824") {
		t.Errorf("%s", f.asked())
	}
}

func TestAPullRequestFromAFork(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.db.CreateEnvironment(ctx, "shop", "feature", store.KindPreview, "feature"); err != nil {
		t.Fatal(err)
	}
	if fork, err := f.db.EnvironmentFromFork(ctx, "shop", "feature"); err != nil || fork {
		t.Errorf("a branch: %v %v", fork, err)
	}
	if _, err := f.db.CreateEnvironment(ctx, "shop", "pr-9", store.KindPreview, "pr-9"); err != nil {
		t.Fatal(err)
	}
	if fork, err := f.db.EnvironmentFromFork(ctx, "shop", "pr-9"); err != nil || !fork {
		t.Errorf("nothing on record: %v %v", fork, err)
	}
	appID, _, err := f.db.Scope(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetForgePull(ctx, store.ForgePull{AppID: appID, Number: 9, HeadSHA: "abc", State: store.PullBuilding, FromFork: false, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if fork, err := f.db.EnvironmentFromFork(ctx, "shop", "pr-9"); err != nil || fork {
		t.Errorf("from this repository: %v %v", fork, err)
	}
	if err := f.db.SetForgePull(ctx, store.ForgePull{AppID: appID, Number: 9, HeadSHA: "abc", State: store.PullBuilding, FromFork: true, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if fork, err := f.db.EnvironmentFromFork(ctx, "shop", "pr-9"); err != nil || !fork {
		t.Errorf("from a fork: %v %v", fork, err)
	}
}

// TestAVolumeIsItsWholeName: Podman takes a name for any volume that it starts, so that asking for one that has gone can answer with another, and removing it would remove that.
func TestAVolumeIsItsWholeName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	dir := t.TempDir()
	removed := filepath.Join(dir, "removed")
	bin := filepath.Join(dir, "podman")
	script := `#!/bin/sh
case "$1 $2" in
  "volume inspect") for last; do :; done
    case "$last" in moorage-*) echo "moorage-svc-a-pg18" ;; esac ;;
  "volume rm") echo "$*" >> "` + removed + `" ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := container.New(&container.Engine{Bin: bin, Kind: "podman"}, "test", slog.New(slog.DiscardHandler))
	if there, err := d.VolumeThere(context.Background(), "moorage-svc-a-data"); err != nil || there {
		t.Errorf("one that has gone: %v %v", there, err)
	}
	if there, err := d.VolumeThere(context.Background(), "moorage-svc-a-pg18"); err != nil || !there {
		t.Errorf("one that is there: %v %v", there, err)
	}
	if err := d.RemoveVolume("moorage-svc-a-data"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(removed); err == nil {
		t.Errorf("it removed another: %s", data)
	}
	if err := d.RemoveVolume("moorage-svc-a-pg18"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(removed); !strings.Contains(string(data), "volume rm moorage-svc-a-pg18") {
		t.Errorf("it did not remove it: %s", data)
	}
	// And no volume's name starts with another's.
	id := "svc_01abc"
	names := []string{container.ServiceVolume(id), container.ServiceMetadataVolume(id), container.ServiceUpgradeVolume(id, "18"), container.ServiceUpgradeVolume(id, "19")}
	for _, a := range names {
		for _, b := range names {
			if a != b && strings.HasPrefix(b, a) {
				t.Errorf("%s starts with %s", b, a)
			}
		}
	}
}

// A copy of production's that has not been sanitized is for nobody, whatever else goes on around it.
func TestAnUnfinishedSeed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.o.GateFor = func(set, asked, kind string) (string, string) { return "login", "setting" }
	if _, err := f.db.CreateEnvironment(ctx, "shop", "topic", store.KindPreview, "topic"); err != nil {
		t.Fatal(err)
	}
	rec := store.Service{ID: "svc_seed", App: "shop", Env: "topic", Name: "db", Type: manifest.ServicePostgres, Version: "18", Image: "postgres", Engine: f.source.Status(ctx).Bin, Volume: "a-volume", Mount: "/x", Port: 45000,
		Sealed: []byte("sealed:pw"), EnvVar: "DATABASE_URL", State: store.ServiceSeeding, CreatedAt: time.Now()}
	if err := f.db.CreateService(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Sleep(ctx, "shop", "topic"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Wake(ctx, "shop", "topic"); err != nil {
		t.Fatal(err)
	}
	f.m.keepUp(ctx, rec)
	got, err := f.db.Service(ctx, "shop", "topic", "db")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.ServiceSeeding || f.running(got) {
		t.Errorf("it started, or is said to have finished: %+v", got)
	}
	plan := postgresPlan()
	if secret, _, _, err := f.m.Addresses(ctx, "shop", "topic", plan, false); err != nil || len(secret) != 0 {
		t.Errorf("an app is told of it: %v %v", secret, err)
	}
	// The next deployment starts it again from nothing.
	f.flag("volume-a-volume", true)
	if err := f.m.Ensure(ctx, "shop", "topic", store.KindPreview, "me", "", plan, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Service(ctx, "shop", "topic", "db"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "volume-a-volume")); err == nil {
		t.Errorf("what it held: %s", f.asked())
	}
	// With what kept a copy safe gone, a deployment says so.
	seeded, err := f.db.Service(ctx, "shop", "topic", "db")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetServiceSeeded(ctx, seeded.ID, true); err != nil {
		t.Fatal(err)
	}
	f.m.o.GateFor = func(set, asked, kind string) (string, string) { return "public", "manifest" }
	if err := f.m.Ensure(ctx, "shop", "topic", store.KindPreview, "me", "", plan, io.Discard); err == nil || !strings.Contains(err.Error(), "a copy of production's data") {
		t.Errorf("the gate gone: %v", err)
	}
}

// S3's two volumes: one gone is not something to clear up by removing the other.
func TestOneVolumeOfTwoGone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.o.DataDir = f.dir
	id := "svc_two"
	rec := store.Service{ID: id, App: "shop", Env: "production", Name: "files", Type: manifest.ServiceS3, Version: "1", Image: "x", Engine: f.source.Status(ctx).Bin, Volume: container.ServiceVolume(id), Port: 45001,
		Sealed: []byte("sealed:pw"), EnvVar: "S3_ENDPOINT", State: store.ServiceRunning, CreatedAt: time.Now()}
	if err := f.db.CreateService(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, err := f.db.Service(ctx, "shop", "production", "files")
	if err != nil {
		t.Fatal(err)
	}
	f.flag("volume-"+got.Volume, true)
	f.m.keepUp(ctx, got)
	if f.running(got) {
		t.Error("it came up with half of what it holds")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "volume-"+got.Volume)); err != nil {
		t.Errorf("what was left: %v", err)
	}
}

// Waiting for its turn does not mean going by what it read before that: the load that had the lock may have failed, and its volume then holds a part of what it should.
func TestWakeGoesByWhatIsSoOnceItIsItsTurn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.o.DataDir = f.dir
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", postgresPlan(), io.Discard); err != nil {
		t.Fatal(err)
	}
	rec, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Sleep(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	dump := f.m.dumpPath(rec)
	if err := os.MkdirAll(filepath.Dir(dump), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dump, []byte("the dump\n-- PostgreSQL database cluster dump complete\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock := f.m.lock("shop/production")
	done := make(chan error, 1)
	go func() { done <- f.m.Wake(ctx, "shop", "production") }()
	time.Sleep(300 * time.Millisecond)
	if err := f.db.SetServicePendingLoad(ctx, rec.ID, true); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	if got.PendingLoad {
		t.Error("it runs on a volume that is on record as a part of what it held")
	}
	if loaded, _ := os.ReadFile(filepath.Join(f.dir, "loaded")); !strings.Contains(string(loaded), "the dump") {
		t.Errorf("what it loaded: %q", loaded)
	}
}

// What a server left running, when it stopped during a dump, is stopped when the next one starts, and a backup does not take it for the service.
func TestWhatAServerLeft(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.o.DataDir = f.dir
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", "", postgresPlan(), io.Discard); err != nil {
		t.Fatal(err)
	}
	rec, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(f.dir, "running-"+container.ServiceName(rec.ID))
	dump := f.m.dumpPath(rec)

	// Being put back, it is not something to dump.
	if err := os.WriteFile(file, []byte("true true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if problems := f.m.DumpAll(ctx); len(problems) != 1 || !strings.Contains(problems[0], "not running just now") {
		t.Errorf("%v", problems)
	}
	if _, err := os.Stat(dump); err == nil {
		t.Error("it dumped what was being put back")
	}
	// What is on record as asleep does not stay up.
	if err := f.db.SetServiceState(ctx, rec.ID, store.ServiceStopped); err != nil {
		t.Fatal(err)
	}
	f.m.clearUp(ctx)
	if _, err := os.Stat(file); err == nil {
		t.Error("what it left is still running")
	}
	// A dump that is not whole does not replace one that is.
	if err := f.db.SetServiceState(ctx, rec.ID, store.ServiceRunning); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if problems := f.m.DumpAll(ctx); len(problems) != 0 {
		t.Fatalf("%v", problems)
	}
	good, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	f.flag("cut-short", true)
	if problems := f.m.DumpAll(ctx); len(problems) != 1 || !strings.Contains(problems[0], "not whole") {
		t.Errorf("%v", problems)
	}
	if now, _ := os.ReadFile(dump); string(now) != string(good) {
		t.Errorf("the dump: %q", now)
	}
}

func TestDumpWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	if err := w.WriteHeader(&tar.Header{Name: "s3/a", Mode: 0o600, Size: 2000}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, 2000)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		kind, body string
		whole      bool
	}{
		{manifest.ServicePostgres, "x\n--\n-- PostgreSQL database cluster dump complete\n--\n\n", true},
		{manifest.ServicePostgres, "x\nCOPY a FROM stdin;\n1\n", false},
		{manifest.ServicePostgres, "", false},
		{manifest.ServiceS3, archive.String(), true},
		{manifest.ServiceS3, archive.String()[:1024], false},
		{manifest.ServiceS3, "", false},
	} {
		if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := dumpWhole(path, c.kind); (err == nil) != c.whole {
			t.Errorf("%s of %d bytes: %v", c.kind, len(c.body), err)
		}
	}
}

// A load that fails does not leave what filters it waiting for ever.
func TestAFailedLoadLeavesNothingBehind(t *testing.T) {
	var body strings.Builder
	for i := range 50000 {
		fmt.Fprintf(&body, "select %d; -- a line long enough to fill what it goes through\n", i)
	}
	before := runtime.NumGoroutine()
	for range 5 {
		filtered := withoutWhatIsThere(strings.NewReader(body.String()))
		if _, err := io.ReadFull(filtered, make([]byte, 16)); err != nil {
			t.Fatal(err)
		}
		filtered.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("%d goroutines became %d", before, after)
	}
}
