// Package platform holds the operating-system-specific pieces Moorage needs:
// default locations, privilege checks and file locks.
package platform

import (
	"errors"
	"os"
	"runtime"
	"time"
)

// Dirs are the default locations of a server's configuration file and data
// directory.
type Dirs struct {
	ConfigFile string
	DataDir    string
}

// FindDirs is where a server's files are: the default locations, unless this user has a
// server from an earlier version, whose configuration is where it was. Its data is
// wherever that configuration says.
func FindDirs(system bool) (Dirs, error) {
	dirs, err := DefaultDirs(system)
	if err != nil || system {
		return dirs, err
	}
	if _, err := os.Stat(dirs.ConfigFile); err == nil {
		return dirs, nil
	}
	if earlier, ok := legacyDirs(); ok {
		if _, err := os.Stat(earlier.ConfigFile); err == nil {
			return earlier, nil
		}
	}
	return dirs, nil
}

// ErrLocked is returned by LockFile when another process holds the lock.
var ErrLocked = errors.New("locked by another process")

// RenameDir moves a directory. On Windows, a directory is refused for a moment after a program in it has stopped, or
// while something such as a virus scanner looks at what was just written there, and that passes.
func RenameDir(from, to string) error {
	err := os.Rename(from, to)
	if runtime.GOOS != "windows" {
		return err
	}
	for wait := 50 * time.Millisecond; err != nil && !errors.Is(err, os.ErrNotExist) && wait < 5*time.Second; wait *= 2 {
		time.Sleep(wait)
		err = os.Rename(from, to)
	}
	return err
}

// TreeLimits say what a command and all it starts are held to, where the system does that for a tree of processes, as
// Windows does with a Job Object. Elsewhere they are held to limits another way, or not.
type TreeLimits struct {
	// Memory is in bytes: what it asks for beyond this is refused.
	Memory int64
	// CPU is in whole CPUs.
	CPU float64
	// Processes is how many may run at once.
	Processes int
}
