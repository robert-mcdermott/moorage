package platform

import (
	"os"
	"path/filepath"
)

// DefaultDirs returns system-wide locations when system is true and the
// user's XDG locations otherwise.
func DefaultDirs(system bool) (Dirs, error) {
	if system {
		return Dirs{ConfigFile: "/etc/moorage/server.toml", DataDir: "/var/lib/moorage"}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		config = filepath.Join(home, ".config")
	}
	data := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(data) {
		data = filepath.Join(home, ".local", "share")
	}
	return Dirs{
		ConfigFile: filepath.Join(config, "moorage", "server.toml"),
		DataDir:    filepath.Join(data, "moorage"),
	}, nil
}
