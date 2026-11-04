//go:build !unix

package isolate

import "io/fs"

// statOf has nothing to say here, where there is no isolation.
func statOf(fs.FileInfo) (uid, gid uint32, links uint64, ok bool) { return 0, 0, 0, false }
