package platform

import (
	"errors"
	"os"
	"path/filepath"
)

// DefaultDirs returns %ProgramData%\Moorage when system is true and
// %LocalAppData%\Moorage otherwise. The configuration file sits next to the
// data.
func DefaultDirs(system bool) (Dirs, error) {
	var base string
	if system {
		base = os.Getenv("ProgramData")
		if base == "" {
			return Dirs{}, errors.New("%ProgramData% is not set")
		}
	} else {
		local, err := os.UserCacheDir() // %LocalAppData%
		if err != nil {
			return Dirs{}, err
		}
		base = local
	}
	dir := filepath.Join(base, "Moorage")
	return Dirs{ConfigFile: filepath.Join(dir, "server.toml"), DataDir: dir}, nil
}

// legacyDirs is where a server of an earlier version may have its files: Windows's have
// not moved.
func legacyDirs() (Dirs, bool) { return Dirs{}, false }

// ClientConfigFile is where moor keeps its contexts.
func ClientConfigFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "moorage", "config.toml"), nil
}
