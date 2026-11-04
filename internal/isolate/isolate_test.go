package isolate

import (
	"context"
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

// TestSealOnceWithALinkAtItsMark: a build step can leave anything at the name of the mark. With a link there,
// a server running as root would create the file it leads to.
func TestSealOnceWithALinkAtItsMark(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no such modes or owners")
	}
	sealOwner = os.Getuid()
	t.Cleanup(func() { sealOwner = 0 })
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "made-by-root")
	if err := os.Symlink(outside, filepath.Join(dir, sealedMark)); err != nil {
		t.Skip(err)
	}
	if err := sealOnce(dir, uint32(os.Getgid())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("it wrote through the link")
	}
	info, err := os.Lstat(filepath.Join(dir, sealedMark))
	if err != nil || !info.Mode().IsRegular() {
		t.Errorf("the mark: %v %v", info, err)
	}
}

// TestWalksStayInside: what a walk is given cannot take it elsewhere, by a link to a directory it meets, or one
// swapped in on the way.
func TestWalksStayInside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no such modes")
	}
	dir := t.TempDir()
	outside := t.TempDir()
	file := filepath.Join(outside, "theirs")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Skip(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mine"), []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := applyModes(dir); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("the walk reached outside: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "mine")); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("%v %v", info, err)
	}
}

// TestOwnTreeThroughAnAdministratorsLink: an environment's data can be a link to a bigger disk.
func TestOwnTreeThroughAnAdministratorsLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no such modes")
	}
	bigger := t.TempDir()
	link := filepath.Join(t.TempDir(), "production")
	if err := os.Symlink(bigger, link); err != nil {
		t.Skip(err)
	}
	if err := ownTree(link, uint32(os.Getuid()), uint32(os.Getgid()), 0o700); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(bigger); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("%v %v", info, err)
	}
}

// TestUserFromTheDatabase will not take the database's word for root.
func TestUserFromTheDatabase(t *testing.T) {
	for uid, ok := range map[uint32]bool{0: false, 1000: false, 299999: false, 300000: true, 300002: true} {
		p := &Policy{uidBase: DefaultUIDBase, Identity: func(context.Context, string) (uint32, error) { return uid, nil }}
		if _, err := p.uidOf(context.Background(), "shop"); (err == nil) != ok {
			t.Errorf("user %d: %v", uid, err)
		}
	}
}

// TestCloseEachLeavesLinksAlone: a link in there could lead anywhere.
func TestCloseEachLeavesLinksAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no such modes")
	}
	dir := t.TempDir()
	open := filepath.Join(dir, "shop")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Chmod(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skip(err)
	}
	if err := closeEach(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(open); err != nil || info.Mode().Perm() != 0o750 {
		t.Errorf("%v %v", info, err)
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("%v %v", info, err)
	}
}

// TestSealingLeavesOtherBuildsAlone: a package manager's cache links the same file into the builds of different
// apps, and sealing one for its app must not take it away from the others.
func TestSealingLeavesOtherBuildsAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no such modes or owners")
	}
	sealOwner = os.Getuid()
	t.Cleanup(func() { sealOwner = 0 })
	base := t.TempDir()
	mine, theirs := filepath.Join(base, "mine"), filepath.Join(base, "theirs")
	for _, d := range []string{mine, theirs} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(mine, "routing.py")
	if err := os.WriteFile(file, []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(theirs, "routing.py")
	if err := os.Link(file, other); err != nil {
		t.Skip(err)
	}
	if err := seal(mine, uint32(os.Getgid())); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(other)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("sealing one changed the other: %v %v", info, err)
	}
	sealed, err := os.Stat(file)
	if err != nil || sealed.Mode().Perm() != 0o640 {
		t.Errorf("%v %v", sealed, err)
	}
	if os.SameFile(info, sealed) {
		t.Error("they are still one file")
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "shared" {
		t.Errorf("%q %v", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(mine, "*.sealing")); len(left) != 0 {
		t.Errorf("%v", left)
	}
}
