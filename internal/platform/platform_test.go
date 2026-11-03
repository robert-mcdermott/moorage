package platform

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "moor.lock")
	first, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockFile(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second LockFile: err = %v, want ErrLocked", err)
	}
	if err := first.Unlock(); err != nil {
		t.Fatal(err)
	}
	again, err := LockFile(path)
	if err != nil {
		t.Fatalf("LockFile after Unlock: %v", err)
	}
	again.Unlock()
}

func TestDefaultDirs(t *testing.T) {
	for _, system := range []bool{true, false} {
		dirs, err := DefaultDirs(system)
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(dirs.ConfigFile) || !filepath.IsAbs(dirs.DataDir) {
			t.Errorf("DefaultDirs(%v) = %+v, want absolute paths", system, dirs)
		}
		if filepath.Base(dirs.ConfigFile) != "server.toml" {
			t.Errorf("DefaultDirs(%v).ConfigFile = %q", system, dirs.ConfigFile)
		}
	}
}

func TestProcessStartTime(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("not used on this operating system")
	}
	a, err := ProcessStartTime(os.Getpid())
	if err != nil || a == 0 {
		t.Fatalf("ProcessStartTime(self) = %d, %v", a, err)
	}
	if b, _ := ProcessStartTime(os.Getpid()); b != a {
		t.Errorf("start time changed: %d, %d", a, b)
	}
	if !Alive(os.Getpid()) {
		t.Error("this process is not alive")
	}
}
