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
