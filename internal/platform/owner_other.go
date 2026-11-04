//go:build !unix

package platform

import "io/fs"

// OpenNonblock is nothing here, where nothing that opening could wait for is left in a directory.
const OpenNonblock = 0

// OwnedLike says that there is nothing to do here.
func OwnedLike(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
