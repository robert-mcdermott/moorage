package platform

import (
	"os"
	"path/filepath"
)

// DefaultDirs returns /etc and /var/lib locations when system is true, and ~/.moorage
// otherwise, unless XDG_CONFIG_HOME or XDG_DATA_HOME says where those belong.
func DefaultDirs(system bool) (Dirs, error) {
	if system {
		return Dirs{ConfigFile: "/etc/moorage/server.toml", DataDir: "/var/lib/moorage"}, nil
	}
	dirs, err := homeDirs()
	if err != nil {
		return Dirs{}, err
	}
	if config := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(config) {
		dirs.ConfigFile = filepath.Join(config, "moorage", "server.toml")
	}
	if data := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(data) {
		dirs.DataDir = filepath.Join(data, "moorage")
	}
	return dirs, nil
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
func ClientConfigFile() (string, error) {
	if config := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(config) {
		return filepath.Join(config, "moorage", "config.toml"), nil
	}
	return homeClientConfig()
}
