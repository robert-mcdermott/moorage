//go:build unix

package isolate

import (
	"io/fs"
	"syscall"
)

// statOf says who a file belongs to, and how many names it has.
func statOf(info fs.FileInfo) (uid, gid uint32, links uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return st.Uid, st.Gid, uint64(st.Nlink), true
}
