//go:build !linux && !darwin && !windows

package platform

import (
	"os"
	"path/filepath"
)

// DefaultDirs returns /etc and /var/lib locations when system is true and ~/.moorage
// otherwise.
func DefaultDirs(system bool) (Dirs, error) {
	if system {
		return Dirs{ConfigFile: "/etc/moorage/server.toml", DataDir: "/var/lib/moorage"}, nil
	}
	return homeDirs()
}

// legacyDirs is where a server of an earlier version may have its files.
func legacyDirs() (Dirs, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, false
	}
	return Dirs{
		ConfigFile: filepath.Join(home, ".config", "moorage", "server.toml"),
		DataDir:    filepath.Join(home, ".local", "share", "moorage"),
	}, true
}

// ClientConfigFile is where moor keeps its contexts.
func ClientConfigFile() (string, error) { return homeClientConfig() }
