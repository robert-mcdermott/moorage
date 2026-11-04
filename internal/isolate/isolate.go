// Package isolate keeps apps from one another (spec/05-runtime.md, "Isolation levels"): on Linux, an
// OS user of its own for each app, limits in a cgroup, and a Landlock sandbox. A tiny helper applies
// them in the child: the shim re-executes moor inside the instance's cgroup, and the helper switches
// user, restricts itself and then becomes the app, while the shim itself stays unrestricted.
package isolate

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The levels of isolation. Auto is User where the server runs as root on Linux, and None otherwise.
const (
	Auto    = "auto"
	None    = "none"
	User    = "user"
	Sandbox = "sandbox"
)

// DefaultUIDBase is where the users of apps start, clear of what systems and containers use.
const DefaultUIDBase = 300000

// HelperArg is the mode of moor that is the helper.
const HelperArg = "isolate-exec"

// NoPortRule says not to restrict what a process may listen on.
const NoPortRule = -1

// Limits say how much an instance may use.
type Limits struct {
	Memory int64   `json:"memory,omitempty"`
	CPU    float64 `json:"cpu,omitempty"`
	PIDs   int     `json:"pids,omitempty"`
	// CPUWeight says how much it counts beside others, where 100 is the usual. Builds count for less.
	CPUWeight int `json:"cpu_weight,omitempty"`
}

// Any says whether it limits anything.
func (l Limits) Any() bool { return l.Memory > 0 || l.CPU > 0 || l.PIDs > 0 || l.CPUWeight > 0 }

// Spec says how a process runs.
type Spec struct {
	// Drop says to run as UID and GID.
	Drop     bool   `json:"drop,omitempty"`
	UID, GID uint32 `json:",omitempty"`
	// Sandbox says to restrict it with no_new_privs and Landlock, to what may be read and what may be
	// written, and to listening on Port alone.
	Sandbox   bool     `json:"sandbox,omitempty"`
	ReadOnly  []string `json:"read_only,omitempty"`
	ReadWrite []string `json:"read_write,omitempty"`
	Port      int      `json:"port,omitempty"`
	// Cgroup is the cgroup it runs in, and Limits what that allows.
	Cgroup string `json:"cgroup,omitempty"`
	Limits Limits `json:"limits,omitempty"`
}

// Status says what a server asked for, and what it has.
type Status struct {
	// Asked is what its configuration asks for, and Level what is in effect.
	Asked, Level string
	// CgroupRoot is where limits go, if they can be enforced.
	CgroupRoot string
	// Landlock is the version of Landlock this kernel has: 0 is none.
	Landlock int
	// Notes say what is missing, and why.
	Notes []string
}

// Enforced says whether limits on memory, CPU and processes are enforced by the kernel.
func (s Status) Enforced() bool { return s.CgroupRoot != "" }

// Describe says the level in words.
func (s Status) Describe() string {
	switch s.Level {
	case Sandbox:
		return "each app as its own user, in a sandbox"
	case User:
		return "each app as its own user"
	}
	return "all apps as the same user, who can read one another's data"
}

// Config is what the server's configuration says.
type Config struct {
	Level   string
	UIDBase uint32
	DataDir string
	// Binary is moor itself, which runs commands that are not simple.
	Binary string
}

// Policy is how a server isolates its apps.
type Policy struct {
	Status
	dataDir string
	binary  string
	// Identity says which user an app runs as.
	Identity func(ctx context.Context, app string) (uint32, error)
}

// Isolating says whether apps run as users of their own.
func (p *Policy) Isolating() bool {
	return p != nil && (p.Level == User || p.Level == Sandbox)
}

// New works out what can be done here, and gets the data directory and cgroups ready for it. An
// explicit level that cannot be had is an error, and "auto" settles for what it can get.
func New(cfg Config, identity func(ctx context.Context, app string) (uint32, error)) (*Policy, error) {
	st, err := resolve(cfg)
	if err != nil {
		return nil, err
	}
	p := &Policy{Status: st, dataDir: cfg.DataDir, binary: cfg.Binary, Identity: identity}
	if p.Isolating() {
		if err := p.tighten(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Probe says what New would find, without changing anything.
func Probe(cfg Config) (Status, error) { return probe(cfg) }

// settle says which level is in effect, from what this machine has.
func settle(cfg Config, root bool, landlock int, supported bool) (Status, error) {
	asked := cfg.Level
	if asked == "" {
		asked = Auto
	}
	st := Status{Asked: asked, Level: None, Landlock: landlock}
	want := asked
	if want == Auto {
		if !supported || !root {
			return st, nil
		}
		want = User
	}
	if want == None {
		return st, nil
	}
	if !supported {
		return st, fmt.Errorf("isolation level %q needs Linux", want)
	}
	if !root {
		return st, fmt.Errorf("isolation level %q needs the server to run as root, to switch to each app's user", want)
	}
	if problem := unreachable(cfg); problem != "" {
		if asked == Auto {
			st.Notes = append(st.Notes, problem+": apps run as the same user until it is")
			return st, nil
		}
		return st, fmt.Errorf("isolation level %q: %s", want, problem)
	}
	st.Level = want
	if want == Sandbox && landlock == 0 {
		st.Level = User
		st.Notes = append(st.Notes, "this kernel has no Landlock, so that apps run as their own users, without a sandbox")
	}
	return st, nil
}

// unreachable says what would keep an app's user from what it needs: the data directory, and moor
// itself, which runs commands that are not simple.
func unreachable(cfg Config) string {
	if dir := closedParent(cfg.DataDir); dir != "" {
		return fmt.Sprintf("the data directory is inside %s, which other users cannot enter", dir)
	}
	if cfg.Binary != "" {
		if dir := closedParent(filepath.Dir(cfg.Binary)); dir != "" {
			return fmt.Sprintf("moor is inside %s, which other users cannot enter", dir)
		}
		if fi, err := os.Stat(cfg.Binary); err == nil && fi.Mode().Perm()&0o001 == 0 {
			return fmt.Sprintf("other users cannot run %s", cfg.Binary)
		}
	}
	return ""
}

// closedParent finds a directory above path that others cannot pass through.
func closedParent(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		if fi, err := os.Stat(dir); err == nil && fi.Mode().Perm()&0o001 == 0 {
			return dir
		}
		if dir == filepath.Dir(dir) {
			return ""
		}
	}
}

// tighten closes what is only the server's. A data directory is open to all by default, since on a
// computer of one's own there is nobody to keep out.
func (p *Policy) tighten() error {
	for name, mode := range map[string]fs.FileMode{
		".": 0o711, "builds": 0o711, "data": 0o711, "tmp": 0o711, "cache": 0o711,
		"logs": 0o700, "repos": 0o700, "toolchains": 0o755,
	} {
		path := filepath.Join(p.dataDir, name)
		if err := os.Chmod(path, mode); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("close %s: %w", path, err)
		}
	}
	matches, err := filepath.Glob(filepath.Join(p.dataDir, "moorage.db*"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if err := os.Chmod(m, 0o600); err != nil {
			return fmt.Errorf("close %s: %w", m, err)
		}
	}
	return nil
}

// Dirs are what an instance or a command uses.
type Dirs struct {
	// Build is builds/<app>/<id>, and App the directory in it that it runs in.
	Build string
	App   string
	Data  string
	Tmp   string
}

// systemRead is what a sandboxed process may read beyond its own: what programs need to run at all.
var systemRead = []string{"/usr", "/lib", "/lib32", "/lib64", "/bin", "/sbin", "/etc", "/opt", "/proc", "/sys", "/run/systemd/resolve"}

// systemWrite is the devices that programs expect to be able to write to.
var systemWrite = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty", "/dev/shm", "/dev/pts"}

// Instance says how an instance or a command of an app runs, and gives it what it needs: id names its
// cgroup. It is nil if there is nothing to apply.
func (p *Policy) Instance(ctx context.Context, app, id string, dirs Dirs, limits Limits, port int) (*Spec, error) {
	if p == nil {
		return nil, nil
	}
	s := &Spec{Port: port}
	if p.Isolating() {
		uid, err := p.Identity(ctx, app)
		if err != nil {
			return nil, fmt.Errorf("the user of %s: %w", app, err)
		}
		gid := uid
		s.Drop, s.UID, s.GID = true, uid, gid
		if err := ownTree(dirs.Data, uid, gid, 0o700); err != nil {
			return nil, err
		}
		if err := ownParent(dirs.Data, gid); err != nil {
			return nil, err
		}
		if err := ownTree(dirs.Tmp, uid, gid, 0o700); err != nil {
			return nil, err
		}
		if dirs.Build != "" {
			if err := ownParent(dirs.Build, gid); err != nil {
				return nil, err
			}
			if err := sealOnce(dirs.Build, gid); err != nil {
				return nil, err
			}
		}
	}
	if p.Level == Sandbox {
		s.Sandbox = true
		s.ReadOnly = append(p.commonRead(), dirs.App)
		s.ReadWrite = append(append([]string{}, systemWrite...), dirs.Data, dirs.Tmp)
	}
	if p.Enforced() && (limits.Any() || p.Isolating()) {
		s.Cgroup, s.Limits = filepath.Join(p.CgroupRoot, "apps", id), limits
	}
	if !s.Drop && s.Cgroup == "" {
		return nil, nil
	}
	return s, nil
}

func (p *Policy) commonRead() []string {
	read := append([]string{}, systemRead...)
	read = append(read, filepath.Join(p.dataDir, "toolchains"))
	if p.binary != "" {
		read = append(read, p.binary)
	}
	return read
}

// BuildCache is the cache a build of an app uses: its own while isolating, so that a build cannot
// poison another's, and "" otherwise.
func (p *Policy) BuildCache(app string) string {
	if !p.Isolating() {
		return ""
	}
	return filepath.Join(p.dataDir, "cache", "apps", app)
}

// Build says how a build of an app runs: as a user of its own, beside the app's user and in its
// group, so that what it runs from a package cannot reach the app's data. Everything the build needs
// has to exist by now, to become that user's.
func (p *Policy) Build(ctx context.Context, app, id, dir, tmp string) (*Spec, error) {
	if !p.Isolating() {
		return nil, nil
	}
	uid, err := p.Identity(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("the user of %s: %w", app, err)
	}
	build, gid := uid+1, uid
	cache := p.BuildCache(app)
	for _, d := range []string{dir, tmp, cache} {
		if err := ownTree(d, build, gid, 0o750); err != nil {
			return nil, err
		}
	}
	if err := ownParent(dir, gid); err != nil {
		return nil, err
	}
	s := &Spec{Drop: true, UID: build, GID: gid, Port: NoPortRule}
	if p.Level == Sandbox {
		s.Sandbox = true
		s.ReadOnly = p.commonRead()
		s.ReadWrite = append(append([]string{}, systemWrite...), dir, tmp, cache)
	}
	if p.Enforced() {
		s.Cgroup, s.Limits = filepath.Join(p.CgroupRoot, "builds", id), Limits{CPUWeight: 50}
	}
	return s, nil
}

// Seal gives a finished build to the server, for the app to read and nobody to change.
func (p *Policy) Seal(ctx context.Context, app, dir string) error {
	if p == nil || !p.Isolating() {
		return nil
	}
	uid, err := p.Identity(ctx, app)
	if err != nil {
		return err
	}
	return seal(dir, uid)
}

// ownTree makes a directory someone's, and all that is in it if it was someone else's.
func ownTree(dir string, uid, gid uint32, mode fs.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	if err := os.Chmod(dir, mode); err != nil {
		return err
	}
	if owner, group, ok := ownerOf(dir); ok && owner == uid && group == gid {
		return nil
	}
	return filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, int(uid), int(gid))
	})
}

// ownParent closes the directory above a thing to all but the group of the app it is for.
func ownParent(path string, gid uint32) error {
	parent := filepath.Dir(path)
	if err := os.Lchown(parent, 0, int(gid)); err != nil {
		return err
	}
	return os.Chmod(parent, 0o750)
}

const sealedMark = ".sealed"

// sealOnce seals a build the first time an isolated app runs from it: one from before isolation may
// hold what only its owner can read.
func sealOnce(dir string, gid uint32) error {
	mark := filepath.Join(dir, sealedMark)
	if _, err := os.Stat(mark); err == nil {
		return nil
	}
	if err := seal(dir, gid); err != nil {
		return err
	}
	return os.WriteFile(mark, nil, 0o640)
}

func seal(dir string, gid uint32) error {
	if err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, 0, int(gid))
	}); err != nil {
		return err
	}
	return applyModes(dir)
}

// applyModes lets the group read and run what the owner may, and nobody else do anything.
func applyModes(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		return os.Chmod(path, mode&0o700|(mode&0o500)>>3)
	})
}

// fieldOf is what follows a name in a list of lines such as "oom_kill 3".
func fieldOf(text, name string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		if key, value, ok := strings.Cut(line, " "); ok && key == name {
			return value, true
		}
	}
	return "", false
}
