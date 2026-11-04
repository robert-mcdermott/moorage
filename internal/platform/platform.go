// Package platform holds the operating-system-specific pieces Moorage needs:
// default locations, privilege checks and file locks.
package platform

import (
	"errors"
	"os"
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
