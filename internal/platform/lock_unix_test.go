//go:build !windows

package platform

import (
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestUnlockReleasesWhateverHoldsACopy: a child process caught between fork and exec holds a copy of
// the lock's descriptor. Unlocking must let go of the lock all the same, or a server started right
// after another stops is told that the other is still there.
func TestUnlockReleasesWhateverHoldsACopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := unix.Dup(int(first.f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(copied)
	if err := first.Unlock(); err != nil {
		t.Fatal(err)
	}
	again, err := LockFile(path)
	if errors.Is(err, ErrLocked) {
		t.Fatal("the lock is still held after Unlock, by a copy of its descriptor")
	}
	if err != nil {
		t.Fatal(err)
	}
	again.Unlock()
}
