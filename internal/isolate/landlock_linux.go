//go:build linux

package isolate

import (
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func sleepBriefly() { time.Sleep(20 * time.Millisecond) }

// landlockVersion is the version of Landlock this kernel has: 0 is none.
func landlockVersion() int {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(v)
}

const (
	// What may be done to a file, of what Landlock can restrict in its first version.
	fileRead  = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE
	fileWrite = unix.LANDLOCK_ACCESS_FS_WRITE_FILE
	// What may be done in a directory.
	dirRead  = unix.LANDLOCK_ACCESS_FS_READ_DIR
	dirWrite = unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM
)

// rulesetAttr is the part of the kernel's struct landlock_ruleset_attr that this asks of it. A kernel
// takes it at this size whichever version it has, with no fields it does not know.
type rulesetAttr struct{ fs, net uint64 }

// netPortAttr is the kernel's struct landlock_net_port_attr, and ruleNetPort its kind of rule.
type netPortAttr struct{ allowed, port uint64 }

const ruleNetPort = 2

// restrict applies a spec's rules to this thread. A kernel with an earlier version of Landlock
// restricts what it can, and the status says which version that is.
func restrict(s *Spec) error {
	version := landlockVersion()
	if version == 0 {
		return nil
	}
	everyFile := uint64(fileRead | fileWrite)
	everything := everyFile | dirRead | dirWrite
	if version >= 2 {
		everything |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if version >= 3 {
		everyFile |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
		everything |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	attr := rulesetAttr{fs: everything}
	listens := version >= 4 && s.Port != NoPortRule
	if listens {
		attr.net = unix.LANDLOCK_ACCESS_NET_BIND_TCP
	}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("make the rules: %w", errno)
	}
	defer unix.Close(int(fd))
	add := func(path string, access uint64) error {
		f, err := os.OpenFile(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if errors.Is(err, os.ErrNotExist) {
			return nil // not every system has all of them
		}
		if err != nil {
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			access &= everyFile
		}
		rule := unix.LandlockPathBeneathAttr{Allowed_access: access & everything, Parent_fd: int32(f.Fd())}
		if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
			return fmt.Errorf("%s: %w", path, errno)
		}
		return nil
	}
	for _, path := range s.ReadOnly {
		if err := add(path, fileRead|dirRead); err != nil {
			return err
		}
	}
	for _, path := range s.ReadWrite {
		if err := add(path, everything); err != nil {
			return err
		}
	}
	if listens && s.Port > 0 {
		rule := netPortAttr{allowed: unix.LANDLOCK_ACCESS_NET_BIND_TCP, port: uint64(s.Port)}
		if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, ruleNetPort, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
			return fmt.Errorf("port %d: %w", s.Port, errno)
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return fmt.Errorf("restrict: %w", errno)
	}
	return nil
}
