package isolate

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSettle(t *testing.T) {
	open := Config{DataDir: "/usr/moorage-data"}
	for _, tc := range []struct {
		level     string
		root      bool
		landlock  int
		supported bool
		want      string
		wrong     string
	}{
		{level: "", root: true, landlock: 5, supported: true, want: User},
		{level: Auto, root: true, supported: true, want: User},
		{level: Auto, root: false, supported: true, want: None},
		{level: Auto, root: true, supported: false, want: None},
		{level: None, root: true, supported: true, want: None},
		{level: User, root: true, supported: true, want: User},
		{level: Sandbox, root: true, landlock: 3, supported: true, want: Sandbox},
		{level: Sandbox, root: true, landlock: 0, supported: true, want: User},
		{level: User, root: false, supported: true, wrong: "root"},
		{level: Sandbox, root: false, supported: true, wrong: "root"},
		{level: User, root: true, supported: false, wrong: "Linux"},
	} {
		cfg := open
		cfg.Level = tc.level
		st, err := settle(cfg, tc.root, tc.landlock, tc.supported)
		if tc.wrong != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wrong) {
				t.Errorf("%+v: %v", tc, err)
			}
			continue
		}
		if err != nil || st.Level != tc.want {
			t.Errorf("%+v: %+v %v", tc, st, err)
		}
	}
}

// TestSettleWithADirectoryOthersCannotEnter: a server's data under a private directory would leave its
// apps unable to start.
func TestSettleWithADirectoryOthersCannotEnter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no such modes")
	}
	closed := filepath.Join(t.TempDir(), "closed")
	if err := os.MkdirAll(filepath.Join(closed, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Level: Auto, DataDir: filepath.Join(closed, "data")}
	st, err := settle(cfg, true, 5, true)
	if err != nil || st.Level != None || len(st.Notes) != 1 || !strings.Contains(st.Notes[0], "closed") {
		t.Errorf("auto: %+v %v", st, err)
	}
	cfg.Level = User
	if _, err := settle(cfg, true, 5, true); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("asked for: %v", err)
	}
}

func TestLimitsAny(t *testing.T) {
	if (Limits{}).Any() {
		t.Error("nothing limits something")
	}
	for _, l := range []Limits{{Memory: 1}, {CPU: 0.5}, {PIDs: 1}, {CPUWeight: 50}} {
		if !l.Any() {
			t.Errorf("%+v", l)
		}
	}
}

// A server that isolates nothing asks nothing of what it starts.
func TestNoPolicy(t *testing.T) {
	var none *Policy
	if none.Isolating() {
		t.Error("no policy isolates")
	}
	spec, err := none.Instance(t.Context(), "shop", "ins_1", Dirs{}, Limits{Memory: 1}, 80)
	if err != nil || spec != nil {
		t.Errorf("%+v %v", spec, err)
	}
	if spec, err := none.Build(t.Context(), "shop", "bld_1", "", ""); err != nil || spec != nil {
		t.Errorf("%+v %v", spec, err)
	}
	if cache := none.BuildCache("shop"); cache != "" {
		t.Error(cache)
	}
	if err := none.Seal(t.Context(), "shop", ""); err != nil {
		t.Error(err)
	}
}

func TestSealLetsTheGroupReadAndNobodyWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no such modes")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "secret")
	run := filepath.Join(dir, "run")
	os.WriteFile(file, []byte("x"), 0o600)
	os.WriteFile(run, []byte("x"), 0o755)
	os.Symlink("/nowhere", filepath.Join(dir, "link"))
	if err := applyModes(dir); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{file: 0o640, run: 0o750} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", path, fi.Mode().Perm(), err, want)
		}
	}
}

func TestDescribe(t *testing.T) {
	for _, level := range []string{None, User, Sandbox} {
		if (Status{Level: level}).Describe() == "" {
			t.Error(level)
		}
	}
}
