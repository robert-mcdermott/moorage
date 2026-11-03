package oneoff

import (
	"os"
	"runtime"
	"sort"
	"strings"
)

// BaseEnv returns the variables of the server's own environment that apps
// and builds inherit: PATH and the few others programs expect (on Windows,
// the system directories).
func BaseEnv() map[string]string {
	names := []string{"PATH", "LANG", "TZ", "USER", "LOGNAME"}
	if runtime.GOOS == "windows" {
		names = []string{"PATH", "PATHEXT", "SystemRoot", "SystemDrive", "WINDIR", "ComSpec", "ProgramData", "ProgramFiles", "USERPROFILE", "APPDATA", "LOCALAPPDATA"}
	}
	vars := map[string]string{}
	for _, k := range names {
		if v, ok := os.LookupEnv(k); ok {
			vars[k] = v
		}
	}
	return vars
}

// PrependPath puts dirs, in order, in front of the list path.
func PrependPath(dirs []string, path string) string {
	parts := append([]string{}, dirs...)
	if path != "" {
		parts = append(parts, path)
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// List turns variables into a sorted KEY=value list.
func List(vars map[string]string) []string {
	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env
}
