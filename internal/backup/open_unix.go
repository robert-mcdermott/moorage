//go:build unix

package backup

import "syscall"

// safeOpen are the flags for opening what an app may have changed: not through a link, and not waiting for
// whoever is meant to write to a pipe.
const safeOpen = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
