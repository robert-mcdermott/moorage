package platform

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestHomeOfMoorage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps to its own folders")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	want := Dirs{ConfigFile: filepath.Join(home, ".moorage", "server.toml"), DataDir: filepath.Join(home, ".moorage", "data")}
	if got, err := FindDirs(false); err != nil || got != want {
		t.Errorf("%+v %v", got, err)
	}
	config, err := ClientConfigFile()
	if err != nil || config != filepath.Join(home, ".moorage", "config.toml") {
		t.Errorf("%q %v", config, err)
	}

	// A server from an earlier version stays where it is, and is found.
	earlier, ok := legacyDirs()
	if !ok {
		t.Fatal("no earlier location")
	}
	if !strings.HasPrefix(earlier.ConfigFile, home) {
		t.Fatalf("%+v", earlier)
	}
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(earlier.ConfigFile)
	if got, err := FindDirs(false); err != nil || got != earlier {
		t.Errorf("%+v %v", got, err)
	}
	// One in the new place wins.
	write(want.ConfigFile)
	if got, err := FindDirs(false); err != nil || got != want {
		t.Errorf("%+v %v", got, err)
	}

	// So with moor's own configuration.
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(configDir, "moorage", "config.toml")
	write(old)
	if got, err := ClientConfigFile(); err != nil || got != old {
		t.Errorf("%q %v", got, err)
	}
	write(filepath.Join(home, ".moorage", "config.toml"))
	if got, err := ClientConfigFile(); err != nil || got != filepath.Join(home, ".moorage", "config.toml") {
		t.Errorf("%q %v", got, err)
	}

	// System-wide locations have not moved.
	if got, err := FindDirs(true); err != nil || strings.Contains(got.ConfigFile, ".moorage") {
		t.Errorf("%+v %v", got, err)
	}
	if runtime.GOOS == "linux" {
		t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
		t.Setenv("XDG_DATA_HOME", "/xdg/data")
		got, err := DefaultDirs(false)
		if err != nil || got.ConfigFile != "/xdg/config/moorage/server.toml" || got.DataDir != "/xdg/data/moorage" {
			t.Errorf("%+v %v", got, err)
		}
		if got, err := ClientConfigFile(); err != nil || got != "/xdg/config/moorage/config.toml" {
			t.Errorf("%q %v", got, err)
		}
	}
}
