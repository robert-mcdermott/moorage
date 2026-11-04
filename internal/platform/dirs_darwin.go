package platform

import (
	"os"
	"path/filepath"
)

// DefaultDirs returns /Library/Application Support/Moorage when system is true, with the
// configuration file next to the data, and ~/.moorage otherwise.
func DefaultDirs(system bool) (Dirs, error) {
	if system {
		dir := "/Library/Application Support/Moorage"
		return Dirs{ConfigFile: filepath.Join(dir, "server.toml"), DataDir: dir}, nil
	}
	return homeDirs()
}

// legacyDirs is where a server of an earlier version may have its files.
func legacyDirs() (Dirs, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, false
	}
	dir := filepath.Join(home, "Library", "Application Support", "Moorage")
	return Dirs{ConfigFile: filepath.Join(dir, "server.toml"), DataDir: dir}, true
}

// ClientConfigFile is where moor keeps its contexts.
func ClientConfigFile() (string, error) { return homeClientConfig() }
