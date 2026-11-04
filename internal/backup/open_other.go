//go:build !unix

package backup

// safeOpen has no such flags here, where apps do not run as anyone else.
const safeOpen = 0
