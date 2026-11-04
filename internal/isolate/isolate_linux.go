//go:build linux

package isolate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const cgroupFS = "/sys/fs/cgroup"

func resolve(cfg Config) (Status, error) {
	st, err := probe(cfg)
	if err != nil {
		return st, err
	}
	if root, err := prepareCgroups(); err != nil {
		st.Notes = append(st.Notes, "limits are not enforced by the kernel: "+err.Error())
	} else {
		st.CgroupRoot = root
	}
	return st, nil
}

func probe(cfg Config) (Status, error) {
	return settle(cfg, os.Geteuid() == 0, landlockVersion(), true)
}

func ownerOf(path string) (uint32, uint32, bool) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return 0, 0, false
	}
	return st.Uid, st.Gid, true
}

// prepareCgroups readies the group that apps run in. It is a top-level group of its own, and not a
// part of the service's: a service that hands out controllers from its own group cannot be restarted
// by a systemd older than 254, since that puts the new process in a group that may no longer hold one.
func prepareCgroups() (string, error) {
	if os.Geteuid() != 0 {
		return "", errors.New("that takes the server running as root")
	}
	if _, err := os.Stat(filepath.Join(cgroupFS, "cgroup.controllers")); err != nil {
		return "", errors.New("this system does not use cgroup v2")
	}
	var release unix.Utsname
	if err := unix.Uname(&release); err != nil {
		return "", err
	}
	if !kernelAtLeast(unix.ByteSliceToString(release.Release[:]), 5, 7) {
		return "", errors.New("this kernel cannot start a process in a cgroup: it takes Linux 5.7")
	}
	root := filepath.Join(cgroupFS, "moorage")
	if err := enableControllers(cgroupFS); err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	if err := enableControllers(root); err != nil {
		return "", err
	}
	for _, sub := range []string{"apps", "builds"} {
		dir := filepath.Join(root, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if err := enableControllers(dir); err != nil {
			return "", err
		}
	}
	return root, nil
}

func kernelAtLeast(release string, major, minor int) bool {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return false
	}
	gotMajor, err1 := strconv.Atoi(parts[0])
	gotMinor, err2 := strconv.Atoi(strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	if err1 != nil || err2 != nil {
		return false
	}
	return gotMajor > major || gotMajor == major && gotMinor >= minor
}

// enableControllers hands the controllers on to a group's children. One that this system does not
// have here is left out: what cannot be limited is said when it is asked for.
func enableControllers(dir string) error {
	control := filepath.Join(dir, "cgroup.subtree_control")
	var failed error
	for _, c := range []string{"memory", "pids", "cpu"} {
		if err := os.WriteFile(control, []byte("+"+c), 0o644); err != nil {
			failed = errors.Join(failed, fmt.Errorf("%s: %w", c, err))
		}
	}
	data, err := os.ReadFile(control)
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), "memory") {
		return fmt.Errorf("cannot hand out the memory controller in %s: %w", dir, failed)
	}
	return nil
}

// makeCgroup makes the group a spec names, with its limits.
func makeCgroup(s *Spec) error {
	if err := os.MkdirAll(s.Cgroup, 0o755); err != nil {
		return err
	}
	write := func(name, value string) error {
		return os.WriteFile(filepath.Join(s.Cgroup, name), []byte(value), 0o644)
	}
	if s.Limits.Memory > 0 {
		if err := write("memory.max", strconv.FormatInt(s.Limits.Memory, 10)); err != nil {
			return fmt.Errorf("limit memory: %w", err)
		}
		// All of it goes together: half an app is worse than none.
		if err := write("memory.oom.group", "1"); err != nil {
			return fmt.Errorf("limit memory: %w", err)
		}
		// Without this, an instance over its limit slows the whole machine by swapping, and is never
		// stopped. A kernel that does not account for swap has no such file.
		if err := write("memory.swap.max", "0"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("limit memory: %w", err)
		}
	}
	if s.Limits.CPU > 0 {
		const period = 100000
		if err := write("cpu.max", fmt.Sprintf("%d %d", int64(s.Limits.CPU*period), period)); err != nil {
			return fmt.Errorf("limit CPU: %w", err)
		}
	}
	if s.Limits.PIDs > 0 {
		if err := write("pids.max", strconv.Itoa(s.Limits.PIDs)); err != nil {
			return fmt.Errorf("limit processes: %w", err)
		}
	}
	if s.Limits.CPUWeight > 0 {
		if err := write("cpu.weight", strconv.Itoa(s.Limits.CPUWeight)); err != nil {
			return fmt.Errorf("weigh CPU: %w", err)
		}
	}
	return nil
}

// Wrap makes cmd run as s says. What it returns lets go of what the start needed, once it has started.
func Wrap(cmd *exec.Cmd, s *Spec) (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	release := func() {}
	if s.Cgroup != "" {
		if err := makeCgroup(s); err != nil {
			return nil, err
		}
		fd, err := unix.Open(s.Cgroup, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = true, fd
		release = func() { unix.Close(fd) }
	}
	if !s.Drop && !s.Sandbox {
		return release, nil
	}
	self, err := os.Executable()
	if err != nil {
		release()
		return nil, err
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		release()
		return nil, err
	}
	path := cmd.Path
	cmd.Args = append([]string{self, HelperArg, string(encoded), "--", path}, cmd.Args...)
	cmd.Path = self
	return release, nil
}

// OOMKills says how many times the kernel has killed what is in a spec's cgroup for using too much memory.
func OOMKills(s *Spec) int {
	if s == nil || s.Cgroup == "" {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(s.Cgroup, "memory.events"))
	if err != nil {
		return 0
	}
	value, ok := fieldOf(string(data), "oom_group_kill")
	if n, err := strconv.Atoi(value); ok && err == nil && n > 0 {
		return n
	}
	value, _ = fieldOf(string(data), "oom_kill")
	n, _ := strconv.Atoi(value)
	return n
}

// Remove ends what is left in a spec's cgroup, and lets go of it.
func Remove(s *Spec) {
	if s == nil || s.Cgroup == "" {
		return
	}
	os.WriteFile(filepath.Join(s.Cgroup, "cgroup.kill"), []byte("1"), 0o644)
	// It takes the kernel a moment to see that the group is empty.
	for range 50 {
		if err := os.Remove(s.Cgroup); err == nil || os.IsNotExist(err) {
			return
		}
		sleepBriefly()
	}
}

// Main is the helper: moor isolate-exec SPEC -- PATH ARGV... It becomes the program it is given. It
// stays on one thread throughout, since a sandbox is per thread until exec makes it the process's.
func Main(args []string) int {
	runtime.LockOSThread()
	if len(args) < 4 || args[1] != "--" {
		fmt.Fprintln(os.Stderr, "moor: isolate-exec SPEC -- PATH ARGV...")
		return 127
	}
	var s Spec
	if err := json.Unmarshal([]byte(args[0]), &s); err != nil {
		fmt.Fprintln(os.Stderr, "moor: the isolation spec:", err)
		return 127
	}
	path, argv := args[2], args[3:]
	if err := s.apply(); err != nil {
		fmt.Fprintln(os.Stderr, "moor: cannot isolate this app:", err)
		return 127
	}
	err := syscall.Exec(path, argv, os.Environ())
	fmt.Fprintf(os.Stderr, "moor: cannot run %s: %v\n", path, err)
	return 127
}

// apply restricts this thread, in the order that works: the sandbox while it may still read what its
// rules name, and then the change of user.
func (s *Spec) apply() error {
	parent := unix.Getppid()
	if s.Sandbox {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("no_new_privs: %w", err)
		}
		if err := restrict(s); err != nil {
			return fmt.Errorf("Landlock: %w", err)
		}
	}
	if !s.Drop {
		return nil
	}
	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("groups: %w", err)
	}
	if err := syscall.Setgid(int(s.GID)); err != nil {
		return fmt.Errorf("group %d: %w", s.GID, err)
	}
	if err := syscall.Setuid(int(s.UID)); err != nil {
		return fmt.Errorf("user %d: %w", s.UID, err)
	}
	// Changing user cancels what is to happen if the shim dies, so it is said again, and if the
	// shim has died already, this is the moment to go too.
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0); err != nil {
		return fmt.Errorf("the parent-death signal: %w", err)
	}
	if unix.Getppid() != parent {
		return errors.New("the shim has gone")
	}
	return nil
}
