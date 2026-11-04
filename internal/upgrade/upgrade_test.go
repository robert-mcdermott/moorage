package upgrade

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The test binary stands in for a release: copied and run with the version it is told to say.
func TestMain(m *testing.M) {
	if said := os.Getenv("MOOR_FAKE_VERSION"); said != "" {
		if said == "crash" {
			os.Exit(3)
		}
		platform := os.Getenv("MOOR_FAKE_PLATFORM")
		if platform == "" {
			platform = runtime.GOOS + "/" + runtime.GOARCH
		}
		fmt.Printf(`{"version":%q,"go":"x","platform":%q}`, said, platform)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// aRelease is a directory with a release in it.
func aRelease(t *testing.T, version string, key ed25519.PrivateKey) string {
	t.Helper()
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	asset := AssetName(runtime.GOOS, runtime.GOARCH)
	if err := os.WriteFile(filepath.Join(dir, asset), body, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n" + strings.Repeat("0", 64) + "  moor-plan9-mips\n")
	if err := os.WriteFile(filepath.Join(dir, SumsName), sums, 0o644); err != nil {
		t.Fatal(err)
	}
	if key != nil {
		if err := os.WriteFile(filepath.Join(dir, SignatureName), []byte(Sign(key, sums)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MOOR_FAKE_VERSION", version)
	return dir
}

type fakeService struct {
	up       bool
	restarts int
	// failing says that the new version does not come up.
	failing bool
	calls   []string
}

func (f *fakeService) Running() bool { return f.up }
func (f *fakeService) Start() error  { f.up, f.calls = true, append(f.calls, "start"); return nil }
func (f *fakeService) Stop() error   { f.up, f.calls = false, append(f.calls, "stop"); return nil }
func (f *fakeService) Restart() error {
	f.restarts++
	f.calls = append(f.calls, "restart")
	return nil
}

// aServer is a server's binary and data.
type aServer struct {
	exe, data, db string
}

func newServer(t *testing.T, schema int) aServer {
	t.Helper()
	dir := t.TempDir()
	s := aServer{exe: filepath.Join(dir, "moor"), data: filepath.Join(dir, "data"), db: filepath.Join(dir, "data", "moorage.db")}
	if err := os.WriteFile(s.exe, []byte("the old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.data, 0o700); err != nil {
		t.Fatal(err)
	}
	if schema > 0 {
		s.setDatabase(t, schema, "before")
	}
	return s
}

// setDatabase says what the database holds, and which schema it has.
func (s aServer) setDatabase(t *testing.T, schema int, holds string) {
	t.Helper()
	db, err := sql.Open("sqlite", s.db)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER)`,
		`CREATE TABLE IF NOT EXISTS holds (what TEXT)`,
		`DELETE FROM schema_migrations`,
		`DELETE FROM holds`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations VALUES (?)`, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO holds VALUES (?)`, holds); err != nil {
		t.Fatal(err)
	}
}

func (s aServer) databaseHolds(t *testing.T) string {
	t.Helper()
	db, err := sql.Open("sqlite", s.db)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var holds string
	if err := db.QueryRow(`SELECT what FROM holds`).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	return holds
}

func (s aServer) binary(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(s.exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == "the old binary" {
		return "old"
	}
	return "new"
}

func (s aServer) options(source Source) Options {
	return Options{Executable: s.exe, Current: "0.5.0", DataDir: s.data, Database: s.db, Source: source, Wait: time.Millisecond, Healthy: func(time.Duration) bool { return true }}
}

func wantRefused(t *testing.T, err error, saying string) {
	t.Helper()
	var refused *RefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), saying) {
		t.Errorf("wanted it refused for %q: %v", saying, err)
	}
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func TestUpgradeAndGoBack(t *testing.T) {
	ctx := context.Background()
	public, private := newKey(t)
	s := newServer(t, 21)
	service := &fakeService{up: true}
	o := s.options(Source{Base: aRelease(t, "0.6.0", private), Flat: true})
	o.Keys, o.Service = []ed25519.PublicKey{public}, service

	res, err := Upgrade(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Restarted || !res.Signed || res.To != "0.6.0" || res.From != "0.5.0" || service.restarts != 1 {
		t.Errorf("%+v %+v", res, service)
	}
	if s.binary(t) != "new" {
		t.Error("the binary is as it was")
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(s.exe); err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("%v %v", info, err)
		}
	}
	if body, err := os.ReadFile(previousPath(s.exe)); err != nil || string(body) != "the old binary" {
		t.Errorf("%q %v", body, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(s.exe), ".moor-upgrade-*"))
	if len(leftovers) != 0 {
		t.Errorf("%v", leftovers)
	}

	// Going back, when the database has not moved on, keeps what has happened since.
	s.setDatabase(t, 21, "since")
	service.calls = nil
	res, err = Rollback(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restored || !res.Restarted || res.To != "0.5.0" {
		t.Errorf("%+v", res)
	}
	if s.binary(t) != "old" || s.databaseHolds(t) != "since" {
		t.Errorf("%s %s", s.binary(t), s.databaseHolds(t))
	}
	if strings.Join(service.calls, " ") != "stop start" {
		t.Errorf("%v", service.calls)
	}
	// There is one way back, and it has been used.
	_, err = Rollback(ctx, o)
	wantRefused(t, err, "nothing to go back to")
}

func TestGoingBackOverAMigratedDatabase(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, 21)
	service := &fakeService{up: true}
	o := s.options(Source{Base: aRelease(t, "0.6.0", nil), Flat: true})
	o.Service = service
	then := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	o.Now = func() time.Time { return then }
	if _, err := Upgrade(ctx, o); err != nil {
		t.Fatal(err)
	}
	s.setDatabase(t, 22, "migrated and used")

	// Much later, that loses what has happened, so it has to be asked for by name.
	o.Now = func() time.Time { return then.Add(time.Hour) }
	_, err := Rollback(ctx, o)
	wantRefused(t, err, "--force")
	if s.binary(t) != "new" || s.databaseHolds(t) != "migrated and used" || len(service.calls) != 1 {
		t.Errorf("a refusal changed something: %s %v", s.binary(t), service.calls)
	}

	o.Force = true
	res, err := Rollback(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Restored || s.binary(t) != "old" || s.databaseHolds(t) != "before" {
		t.Errorf("%+v %s %s", res, s.binary(t), s.databaseHolds(t))
	}
	kept, err := sql.Open("sqlite", filepath.Join(res.Aside, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Close()
	var holds string
	if err := kept.QueryRow(`SELECT what FROM holds`).Scan(&holds); err != nil || holds != "migrated and used" {
		t.Errorf("what it had is not kept: %q %v", holds, err)
	}
}

// TestNewVersionThatDoesNotComeUp goes back by itself, and the database with it.
func TestNewVersionThatDoesNotComeUp(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, 21)
	service := &fakeService{up: true}
	o := s.options(Source{Base: aRelease(t, "0.6.0", nil), Flat: true})
	o.Service = service
	answers := 0
	o.Healthy = func(time.Duration) bool {
		answers++
		if answers == 1 {
			// It got as far as changing the database, and no further.
			s.setDatabase(t, 22, "half migrated")
			return false
		}
		return true
	}
	res, err := Upgrade(ctx, o)
	wantRefused(t, err, "back on 0.5.0")
	if !res.Restored || s.binary(t) != "old" || s.databaseHolds(t) != "before" {
		t.Errorf("%+v %s %s", res, s.binary(t), s.databaseHolds(t))
	}
	if strings.Join(service.calls, " ") != "restart stop start" {
		t.Errorf("%v", service.calls)
	}
	if !service.up {
		t.Error("the server was left stopped")
	}
}

func TestReleasesThatAreRefused(t *testing.T) {
	ctx := context.Background()
	public, private := newKey(t)
	_, stranger := newKey(t)
	keys := []ed25519.PublicKey{public}
	asset := AssetName(runtime.GOOS, runtime.GOARCH)

	try := func(t *testing.T, change func(dir string), want string) {
		t.Helper()
		s := newServer(t, 21)
		dir := aRelease(t, "0.6.0", private)
		if change != nil {
			change(dir)
		}
		o := s.options(Source{Base: dir, Flat: true})
		o.Keys = keys
		_, err := Upgrade(ctx, o)
		wantRefused(t, err, want)
		if s.binary(t) != "old" {
			t.Error("the binary changed")
		}
		if _, err := os.Stat(previousPath(s.exe)); err == nil {
			t.Error("it went as far as swapping")
		}
		if _, err := os.Stat(statePath(s.data)); err == nil {
			t.Error("it says that there was an upgrade")
		}
	}
	t.Run("a changed binary", func(t *testing.T) {
		try(t, func(dir string) {
			f, err := os.OpenFile(filepath.Join(dir, asset), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString("and something more"); err != nil {
				t.Fatal(err)
			}
		}, "does not match its checksum")
	})
	t.Run("no signature", func(t *testing.T) {
		try(t, func(dir string) { os.Remove(filepath.Join(dir, SignatureName)) }, "not signed")
	})
	t.Run("signed by someone else", func(t *testing.T) {
		try(t, func(dir string) {
			sums, _ := os.ReadFile(filepath.Join(dir, SumsName))
			os.WriteFile(filepath.Join(dir, SignatureName), []byte(Sign(stranger, sums)), 0o644)
		}, "not one of the project's")
	})
	t.Run("checksums changed after signing", func(t *testing.T) {
		try(t, func(dir string) {
			f, _ := os.OpenFile(filepath.Join(dir, SumsName), os.O_APPEND|os.O_WRONLY, 0)
			defer f.Close()
			f.WriteString("\n")
		}, "not one of the project's")
	})
	t.Run("nothing for this machine", func(t *testing.T) {
		try(t, func(dir string) {
			sums := []byte(strings.Repeat("0", 64) + "  moor-plan9-mips\n")
			os.WriteFile(filepath.Join(dir, SumsName), sums, 0o644)
			os.WriteFile(filepath.Join(dir, SignatureName), []byte(Sign(private, sums)), 0o644)
		}, "0 times")
	})
	t.Run("does not run", func(t *testing.T) {
		t.Setenv("MOOR_FAKE_VERSION", "crash")
		s := newServer(t, 21)
		dir := aRelease(t, "crash", nil)
		o := s.options(Source{Base: dir, Flat: true})
		_, err := Upgrade(ctx, o)
		wantRefused(t, err, "does not run")
		if s.binary(t) != "old" {
			t.Error("the binary changed")
		}
	})
	t.Run("for another machine", func(t *testing.T) {
		s := newServer(t, 21)
		dir := aRelease(t, "0.6.0", nil)
		t.Setenv("MOOR_FAKE_PLATFORM", "plan9/mips")
		_, err := Upgrade(ctx, s.options(Source{Base: dir, Flat: true}))
		wantRefused(t, err, "is for plan9/mips")
	})
	t.Run("not the version it should be", func(t *testing.T) {
		s := newServer(t, 21)
		o := s.options(Source{Base: aRelease(t, "0.7.0", nil), Flat: true})
		o.Version = "0.6.0"
		_, err := Upgrade(ctx, o)
		wantRefused(t, err, "says that it is 0.7.0")
	})
	t.Run("plain http from elsewhere", func(t *testing.T) {
		s := newServer(t, 21)
		_, err := Upgrade(ctx, s.options(Source{Base: "http://releases.example.com/", Flat: true}))
		wantRefused(t, err, "plain HTTP")
	})
}

// TestNoWhereToGo says so rather than doing anything.
func TestNowhereToGo(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, 21)
	res, err := Upgrade(ctx, s.options(Source{Base: aRelease(t, "0.5.0", nil), Flat: true}))
	if err != nil || !res.Current {
		t.Errorf("%+v %v", res, err)
	}
	_, err = Upgrade(ctx, s.options(Source{Base: aRelease(t, "0.4.0", nil), Flat: true}))
	wantRefused(t, err, "--rollback")
	if s.binary(t) != "old" {
		t.Error("the binary changed")
	}
}

func TestServerNotRunUnderAService(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, 21)
	up := true
	o := s.options(Source{Base: aRelease(t, "0.6.0", nil), Flat: true})
	o.ServerUp = func() bool { return up }
	res, err := Upgrade(ctx, o)
	if err != nil || res.Restarted || res.Stopped || s.binary(t) != "new" {
		t.Errorf("%+v %v", res, err)
	}
	// It has to be stopped to go back: it holds the database open.
	_, err = Rollback(ctx, o)
	wantRefused(t, err, "stop it")
	up = false
	res, err = Rollback(ctx, o)
	if err != nil || !res.Stopped || s.binary(t) != "old" {
		t.Errorf("%+v %v", res, err)
	}
}

func TestServiceThatIsStopped(t *testing.T) {
	s := newServer(t, 21)
	service := &fakeService{}
	o := s.options(Source{Base: aRelease(t, "0.6.0", nil), Flat: true})
	o.Service = service
	res, err := Upgrade(context.Background(), o)
	if err != nil || !res.Stopped || service.restarts != 0 {
		t.Errorf("%+v %v %d", res, err, service.restarts)
	}
}

// TestFromTheProject finds the newest release, and refuses to be told about nothing.
func TestFromTheProject(t *testing.T) {
	ctx := context.Background()
	dir := aRelease(t, "0.6.0", nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tag/v0.6.0", http.StatusFound)
	})
	mux.HandleFunc("GET /download/v0.6.0/{name}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(dir, r.PathValue("name")))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	s := newServer(t, 21)
	o := s.options(Source{Base: server.URL})
	o.Check = true
	res, err := Upgrade(ctx, o)
	if err != nil || !res.Available || res.To != "0.6.0" {
		t.Errorf("%+v %v", res, err)
	}
	if s.binary(t) != "old" {
		t.Error("--check changed the binary")
	}
	o.Check = false
	res, err = Upgrade(ctx, o)
	if err != nil || res.To != "0.6.0" || s.binary(t) != "new" {
		t.Errorf("%+v %v", res, err)
	}

	s = newServer(t, 21)
	o = s.options(Source{Base: server.URL})
	o.Version = "0.9.9"
	_, err = Upgrade(ctx, o)
	wantRefused(t, err, "no release 0.9.9")
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.6.0", "0.5.0", 1}, {"0.10.0", "0.9.0", 1}, {"1.0.0", "1.0.0", 0}, {"v1.0.0", "1.0.0", 0},
		{"1.0.0-rc1", "1.0.0", -1}, {"1.0.0", "1.0.0-rc1", 1}, {"1.0.0-rc1", "1.0.0-rc2", -1},
	} {
		if got, err := CompareVersions(c.a, c.b); err != nil || got != c.want {
			t.Errorf("%s %s: %d %v", c.a, c.b, got, err)
		}
	}
	if _, err := CompareVersions("latest", "1.0.0"); err == nil {
		t.Error("that is no version")
	}
}

func TestKeysInTheCode(t *testing.T) {
	keys, err := ReleaseKeys()
	if err != nil {
		t.Fatal(err)
	}
	public, private := newKey(t)
	signature := Sign(private, []byte("sums"))
	if !VerifySignature([]ed25519.PublicKey{public}, []byte("sums"), []byte(signature)) {
		t.Error("its own signature")
	}
	if VerifySignature(keys, []byte("sums"), []byte(signature)) {
		t.Error("a stranger's signature passed")
	}
	if _, err := parseKeys("not a key"); err == nil {
		t.Error("that is no key")
	}
}
