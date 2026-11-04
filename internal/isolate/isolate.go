// Package isolate keeps apps from one another (spec/05-runtime.md, "Isolation levels"): on Linux, an
// OS user of its own for each app, limits in a cgroup, and a Landlock sandbox. A tiny helper applies
// them in the child: the shim re-executes moor inside the instance's cgroup, and the helper switches
// user, restricts itself and then becomes the app, while the shim itself stays unrestricted.
package isolate

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/platform"
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

// TreeLimits says what a process and all it starts are held to, where the system does that itself.
func (s *Spec) TreeLimits() platform.TreeLimits {
	if s == nil {
		return platform.TreeLimits{}
	}
	return platform.TreeLimits{Memory: s.Limits.Memory, CPU: s.Limits.CPU, Processes: s.Limits.PIDs}
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
	// JobLimits says that limits are held to with Job Objects, as on Windows.
	JobLimits bool
	// Notes say what is missing, and why.
	Notes []string
}

// Enforced says whether limits on memory, CPU and processes are enforced by the kernel.
func (s Status) Enforced() bool { return s.CgroupRoot != "" || s.JobLimits }

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
	uidBase uint32
	// Identity says which user an app runs as.
	Identity func(ctx context.Context, app string) (uint32, error)
}

// uidOf says which user an app runs as, which has to be one of those kept for apps. This comes from the database,
// which a backup can bring from anywhere, and a server running as root should not take its word for root.
func (p *Policy) uidOf(ctx context.Context, app string) (uint32, error) {
	uid, err := p.Identity(ctx, app)
	if err != nil {
		return 0, err
	}
	if uid == 0 || uid < p.uidBase {
		return 0, fmt.Errorf("the database says it runs as user %d, and those kept for apps start at %d: see uid_base under [isolation]", uid, p.uidBase)
	}
	return uid, nil
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
	p := &Policy{Status: st, dataDir: cfg.DataDir, binary: cfg.Binary, uidBase: cfg.UIDBase, Identity: identity}
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
	// What each app has, from before apps were kept apart, or from what never runs as one: the group of its
	// app is given it the next time that app starts, and until then only the server goes in.
	for _, part := range []string{"builds", "data"} {
		if err := closeEach(filepath.Join(p.dataDir, part), 0o750); err != nil {
			return err
		}
	}
	return closeEach(filepath.Join(p.dataDir, "tmp"), 0o700)
}

// closeEach takes what is in a directory away from everyone else, leaving alone what is not a directory.
func closeEach(dir string, mode fs.FileMode) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() { // a link is not one, and is not followed
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&^mode != 0 {
			path := filepath.Join(dir, e.Name())
			if err := os.Chmod(path, info.Mode().Perm()&mode); err != nil {
				return fmt.Errorf("close %s: %w", path, err)
			}
		}
	}
	return nil
}

// CloseAbove closes the directory holding a thing of an app's, data/<app> or builds/<app>, where the server runs
// as root and could otherwise leave it open to every app there. What never runs as an app, such as a site or a
// container, needs it no more open than that: containers get what they mount from a root engine.
func CloseAbove(path string) error {
	if !platform.IsPrivileged() {
		return nil
	}
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o007 == 0 {
		return nil
	}
	return os.Chmod(parent, info.Mode().Perm()&0o770)
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
		uid, err := p.uidOf(ctx, app)
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
	if p.CgroupRoot != "" && (limits.Any() || p.Isolating()) {
		s.Cgroup, s.Limits = filepath.Join(p.CgroupRoot, "apps", id), limits
	}
	if p.JobLimits && limits.Any() {
		s.Limits = limits
		return s, nil
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
	uid, err := p.uidOf(ctx, app)
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
	if p.CgroupRoot != "" {
		s.Cgroup, s.Limits = filepath.Join(p.CgroupRoot, "builds", id), Limits{CPUWeight: 50}
	}
	return s, nil
}

// Seal gives a finished build to the server, for the app to read and nobody to change.
func (p *Policy) Seal(ctx context.Context, app, dir string) error {
	if p == nil || !p.Isolating() {
		return nil
	}
	uid, err := p.uidOf(ctx, app)
	if err != nil {
		return err
	}
	return seal(dir, uid)
}

// ownTree makes a directory someone's, and all that is in it if it was someone else's. A link here is one that an
// administrator made, to keep an app's data on a bigger disk: an app cannot, as the directory above is not its own,
// and a restore refuses a backup that holds one.
func ownTree(dir string, uid, gid uint32, mode fs.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	if err := os.Chmod(dir, mode); err != nil {
		return err
	}
	// All of it each time, and not only when the top is someone else's: what ran as root before now, such as an
	// app that has not restarted since an upgrade, leaves its files among those that are already theirs. Only the
	// top has to work: some of what is inside may not be up to anyone, on a read-only mount or one that does not
	// let root change owners, and an app has run from those before now.
	return walkInside(dir, func(at *os.Root, name string) error {
		info, err := at.Lstat(name)
		if err != nil {
			return err
		}
		if owner, group, _, ok := statOf(info); ok && owner == uid && group == gid {
			return nil
		}
		if err := at.Lchown(name, int(uid), int(gid)); err != nil && name == "." {
			return err
		}
		return nil
	})
}

// walkInside goes through all that is in a directory. What this walks may belong to someone who is still running, who
// could swap a directory for a link on the way: going by paths, that would take the walk, as root, to wherever the
// link leads.
func walkInside(dir string, act func(at *os.Root, name string) error) error {
	return platform.WalkRoot(dir, func(at *os.Root, name, _ string, _ bool) error { return act(at, name) })
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

// sealOnce seals a build the first time an isolated app runs from it: one from before isolation may hold what
// only its owner can read. A build step can leave anything at the name of the mark, and only a plain file counts.
func sealOnce(dir string, gid uint32) error {
	mark := filepath.Join(dir, sealedMark)
	if info, err := os.Lstat(mark); err == nil && info.Mode().IsRegular() {
		return nil
	}
	if err := seal(dir, gid); err != nil {
		return err
	}
	if err := os.Remove(mark); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(mark, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	return f.Close()
}

// sealOwner is who a sealed build belongs to: root, except in a test that is not.
var sealOwner = 0

func seal(dir string, gid uint32) error {
	if err := walkInside(dir, func(at *os.Root, name string) error {
		info, err := at.Lstat(name)
		if err != nil {
			return err
		}
		// A package manager may have linked this from its cache, and then from there to other builds. Changing
		// who it belongs to would change it for all of them, so that this one gets a copy of its own.
		if _, _, links, ok := statOf(info); ok && links > 1 && info.Mode().IsRegular() {
			if err := ownCopy(at, name, info); err != nil {
				return err
			}
		}
		return at.Lchown(name, sealOwner, int(gid))
	}); err != nil {
		return err
	}
	return applyModes(dir)
}

// ownCopy replaces a file with a copy that shares nothing with any other name.
func ownCopy(root *os.Root, name string, info fs.FileInfo) error {
	in, err := root.Open(name)
	if err != nil {
		return err
	}
	defer in.Close()
	copied := name + ".sealing"
	_ = root.Remove(copied)
	out, err := root.OpenFile(copied, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if closed := out.Close(); err == nil {
		err = closed
	}
	if err != nil {
		_ = root.Remove(copied)
		return err
	}
	_ = root.Chtimes(copied, info.ModTime(), info.ModTime())
	return root.Rename(copied, name)
}

// applyModes lets the group read and run what the owner may, and nobody else do anything.
func applyModes(dir string) error {
	return walkInside(dir, func(at *os.Root, name string) error {
		info, err := at.Lstat(name)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		mode := info.Mode().Perm()
		return at.Chmod(name, mode&0o700|(mode&0o500)>>3)
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
