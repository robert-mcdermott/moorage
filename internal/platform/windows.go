//go:build windows

package platform

import (
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/windows"
)

// IsPrivileged reports whether the process runs elevated (as an
// Administrator).
func IsPrivileged() bool {
	return windows.GetCurrentProcessToken().IsElevated()
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
	var ol windows.Overlapped
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &ol); err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrLocked
		}
		return nil, err
	}
	// The lock covers the first byte, which Windows then refuses to write
	// through another handle; write the process ID through this one.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\r\n"), 0)
	}
	return &Lock{f: f}, nil
}

// Unlock releases the lock.
func (l *Lock) Unlock() error {
	return l.f.Close()
}

// AddressInUse reports whether an error says something else listens on an address.
func AddressInUse(err error) bool { return errors.Is(err, windows.WSAEADDRINUSE) }

// AccessDenied reports whether an error says this user may not listen on an address.
func AccessDenied(err error) bool {
	return errors.Is(err, windows.WSAEACCES) || errors.Is(err, os.ErrPermission)
}

// DiskSpace returns how much of the volume holding path is free for this user,
// and how big it is, in bytes.
func DiskSpace(path string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	err = windows.GetDiskFreeSpaceEx(p, &free, &total, nil)
	return free, total, err
}

// KillGroup is not used on Windows, where Job Objects end process trees.
func KillGroup(pgid int) error { return ErrUnsupported }

// Alive reports whether a process with this PID exists.
func Alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}
