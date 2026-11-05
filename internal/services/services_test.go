// SPDX-License-Identifier: Apache-2.0

package services

import (
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
case "$1" in
  --version) echo "podman version 5" ;;
  image) echo '[{"Id":"abc","RepoDigests":["docker.io/library/postgres@sha256:abc"],"Size":1,"Config":{"Env":["PATH=/x","PG_MAJOR=18"]}}]' ;;
  inspect) if [ -e "` + dir + `/running" ]; then echo '{"Running":true,"Status":"running"}'; else echo "Error: no such object" >&2; exit 1; fi ;;
  run) touch "` + dir + `/running" ;;
  rm|stop) rm -f "` + dir + `/running" ;;
  exec) if [ -e "` + dir + `/never-ready" ]; then exit 2; fi ;;
  logs) echo "what it said" ;;
  volume|network)
    if [ "$2" = inspect ] && [ ! -e "` + dir + `/$1" ]; then echo "no such $1" >&2; exit 1; fi
    if [ "$2" = create ]; then touch "` + dir + `/$1"; fi
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.source = container.NewSource(container.SourceOptions{Setting: bin, Kept: time.Nanosecond, NewDriver: func(e *container.Engine) *container.Driver {
		return container.New(e, "test", slog.New(slog.DiscardHandler))
	}})
	port := 40000
	f.m = New(Options{
		DB: db, Keyring: plainKeyring{}, Engines: f.source, Log: slog.New(slog.DiscardHandler), ReadyLimit: 500 * time.Millisecond,
		AllocPort:   func(context.Context) (int, error) { port++; return port, nil },
		ReleasePort: func(int) {},
	})
	return f
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
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", plain, io.Discard); err != nil {
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
	if err := none.Ensure(ctx, "shop", "production", store.KindProduction, "me", postgresPlan(), io.Discard); err != nil || none.Any(ctx) {
		t.Errorf("no manager: %v", err)
	}
}

func TestMakingAService(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plan := postgresPlan()
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", plan, io.Discard); err != nil {
		t.Fatal(err)
	}
	rec, err := f.db.Service(ctx, "shop", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Version != "18" || rec.Digest != "docker.io/library/postgres@sha256:abc" || rec.State != store.ServiceRunning || rec.EnvVar != "DATABASE_URL" || rec.Port != 40001 {
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
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", plan, io.Discard); err != nil {
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
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", newer, io.Discard); err == nil || !strings.Contains(err.Error(), "holds data of PostgreSQL 18") {
		t.Errorf("a newer major: %v", err)
	}

	// With what it needs gone, it says so, and leaves things as they are.
	if err := os.WriteFile(filepath.Join(f.dir, "down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
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
	os.Remove(filepath.Join(f.dir, "down"))

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
	before = f.asked()
	f.m.look(ctx)
	if again := strings.TrimPrefix(f.asked(), before); strings.Contains(again, " run ") || strings.HasPrefix(again, "run ") {
		t.Errorf("it started what should be asleep:\n%s", again)
	}
	if err := f.m.Wake(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.db.Service(ctx, "shop", "production", "db"); got.State != store.ServiceRunning {
		t.Errorf("%+v", got)
	}

	// One that has stopped by itself is started again.
	os.Remove(filepath.Join(f.dir, "running"))
	f.m.look(ctx)
	if _, err := os.Stat(filepath.Join(f.dir, "running")); err != nil {
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
	if _, err := os.Stat(filepath.Join(f.dir, "running")); err == nil {
		t.Error("it started what had just gone to sleep")
	}
	if err := f.m.Wake(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}

	// Removing it goes with what it holds.
	if err := f.m.Remove(ctx, "shop", "production", "db", "me"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.asked(), "volume rm -f "+rec.Volume) {
		t.Errorf("%s", f.asked())
	}
	if list, _ := f.db.Services(ctx, "shop", "production"); len(list) != 0 {
		t.Errorf("%+v", list)
	}
	f.m.keepUp(ctx, stale)
	if _, err := os.Stat(filepath.Join(f.dir, "running")); err == nil {
		t.Error("it started what had just been removed")
	}
}

func TestAServiceThatDoesNotComeUpLeavesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(f.dir, "never-ready"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", postgresPlan(), io.Discard)
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
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", plan, io.Discard); err != nil {
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
	if err := f.m.Ensure(ctx, "shop", "production", store.KindProduction, "me", other, io.Discard); err == nil || !strings.Contains(err.Error(), "a service of s3") {
		t.Errorf("%v", err)
	}
	// Both of its volumes go with it.
	if err := f.m.Remove(ctx, "shop", "production", "files", "me"); err != nil {
		t.Fatal(err)
	}
	if asked := f.asked(); !strings.Contains(asked, "volume rm -f "+rec.Volume) || !strings.Contains(asked, "volume rm -f "+container.ServiceMetadataVolume(rec.ID)) {
		t.Errorf("%s", asked)
	}
}

func TestSmallThings(t *testing.T) {
	for version, want := range map[string]string{"15": "/var/lib/postgresql/data", "17": "/var/lib/postgresql/data", "18": "/var/lib/postgresql", "19": "/var/lib/postgresql"} {
		if got := postgresMount(version); got != want {
			t.Errorf("%s: %s", version, got)
		}
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
