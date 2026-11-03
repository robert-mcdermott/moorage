//go:build !linux && !darwin && !windows

package platform

import (
	"os"
	"path/filepath"
)

// DefaultDirs returns /etc and /var/lib locations when system is true and
// dot-directories in the user's home otherwise.
func DefaultDirs(system bool) (Dirs, error) {
	if system {
		return Dirs{ConfigFile: "/etc/moorage/server.toml", DataDir: "/var/lib/moorage"}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	return Dirs{
		ConfigFile: filepath.Join(home, ".config", "moorage", "server.toml"),
		DataDir:    filepath.Join(home, ".local", "share", "moorage"),
	}, nil
}
