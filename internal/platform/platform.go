// Package platform holds the operating-system-specific pieces Moorage needs:
// default locations, privilege checks and file locks.
package platform

import "errors"

// Dirs are the default locations of a server's configuration file and data
// directory.
type Dirs struct {
	ConfigFile string
	DataDir    string
}

// ErrLocked is returned by LockFile when another process holds the lock.
var ErrLocked = errors.New("locked by another process")
