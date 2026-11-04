//go:build unix

package platform

import (
	"io/fs"
	"os"
	"syscall"
)

// OpenNonblock has opening something that is not a file, which someone has left to be found, not wait for ever.
const OpenNonblock = syscall.O_NONBLOCK

// OwnedLike says whose a new file should be for it to belong with info, if that is for this process to say: only the
// superuser can give things away.
func OwnedLike(info fs.FileInfo) (uid, gid int, ok bool) {
	if os.Geteuid() != 0 {
		return 0, 0, false
	}
	st, is := info.Sys().(*syscall.Stat_t)
	if !is {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// Links says how many names a file has.
func Links(info fs.FileInfo) uint64 {
	if st, is := info.Sys().(*syscall.Stat_t); is {
		return uint64(st.Nlink)
	}
	return 1
}

// FileOwner says whose a file is.
func FileOwner(info fs.FileInfo) (uid int, ok bool) {
	st, is := info.Sys().(*syscall.Stat_t)
	if !is {
		return 0, false
	}
	return int(st.Uid), true
}
