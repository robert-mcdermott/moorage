package platform

import (
	"os"
	"path/filepath"
)

// DefaultDirs returns /Library/Application Support/Moorage when system is
// true and the user's ~/Library/Application Support/Moorage otherwise. The
// configuration file sits next to the data.
func DefaultDirs(system bool) (Dirs, error) {
	dir := "/Library/Application Support/Moorage"
	if !system {
		home, err := os.UserHomeDir()
		if err != nil {
			return Dirs{}, err
		}
		dir = filepath.Join(home, "Library", "Application Support", "Moorage")
	}
	return Dirs{ConfigFile: filepath.Join(dir, "server.toml"), DataDir: dir}, nil
}
