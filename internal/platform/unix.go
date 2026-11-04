//go:build !windows

package platform

import (
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// IsPrivileged reports whether the process runs as root.
func IsPrivileged() bool {
	return os.Geteuid() == 0
}

// Lock is an exclusive lock on a file, held until Unlock or process exit.
type Lock struct {
	f *os.File
}

// LockFile takes an exclusive lock on path, creating the file if needed, and
// writes the process ID into it. It fails at once with ErrLocked if another
// process holds the lock.
func LockFile(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{f: f}, nil
}

// Unlock releases the lock.
func (l *Lock) Unlock() error {
	return l.f.Close()
}

// AddressInUse reports whether an error says something else listens on an address.
func AddressInUse(err error) bool { return errors.Is(err, unix.EADDRINUSE) }

// AccessDenied reports whether an error says this user may not listen on an address.
func AccessDenied(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, os.ErrPermission)
}

// DiskSpace returns how much of the filesystem holding path is free for this
// user, and how big it is, in bytes.
func DiskSpace(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	size := uint64(st.Bsize)
	return uint64(st.Bavail) * size, uint64(st.Blocks) * size, nil
}

// KillGroup sends SIGKILL to the process group pgid.
func KillGroup(pgid int) error {
	if pgid <= 1 {
		return ErrUnsupported
	}
	return unix.Kill(-pgid, unix.SIGKILL)
}

// Alive reports whether a process with this PID exists.
func Alive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || err == unix.EPERM
}
