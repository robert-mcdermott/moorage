//go:build !windows

package platform

import (
	"errors"
	"os"
	"path/filepath"
)

// homeDirs is the per-user home of Moorage: one folder, the same on macOS and Linux, short
// to type, and without a space in the path that builds run under.
func homeDirs() (Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	dir := filepath.Join(home, ".moorage")
	return Dirs{ConfigFile: filepath.Join(dir, "server.toml"), DataDir: filepath.Join(dir, "data")}, nil
}

// homeClientConfig is moor's own configuration, beside the server's, or where an earlier
// version put it if that is where it still is.
func homeClientConfig() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	current := filepath.Join(home, ".moorage", "config.toml")
	if exists(current) {
		return current, nil
	}
	if dir, err := os.UserConfigDir(); err == nil {
		if earlier := filepath.Join(dir, "moorage", "config.toml"); exists(earlier) {
			return earlier, nil
		}
	}
	return current, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}
