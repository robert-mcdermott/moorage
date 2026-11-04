//go:build !unix

package platform

import "io/fs"

// OpenNonblock is nothing here, where nothing that opening could wait for is left in a directory.
const OpenNonblock = 0

// OwnedLike says that there is nothing to do here.
func OwnedLike(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }

// Links says how many names a file has, as far as anyone here is concerned.
func Links(fs.FileInfo) uint64 { return 1 }

// FileOwner says that it is not known whose a file is.
func FileOwner(fs.FileInfo) (uid int, ok bool) { return 0, false }
