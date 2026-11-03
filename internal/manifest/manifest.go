// Package manifest reads moorage.toml, the file in an app's repository that
// tells Moorage how to build and run it (docs/03-manifest.md).
//
// This release reads [app], [static], [processes], [deploy] and [env]. The
// other tables are accepted but not checked until the features they
// configure arrive; unknown top-level keys are already errors.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// FileName is the manifest's name in a repository.
const FileName = "moorage.toml"

// Manifest is the content of moorage.toml.
type Manifest struct {
	Version int               `toml:"version"`
	App     App               `toml:"app"`
	Static  *Static           `toml:"static"`
	Deploy  Deploy            `toml:"deploy"`
	Env     map[string]string `toml:"env"`

	// Processes are the [processes] entries.
	Processes    map[string]Process `toml:"-"`
	RawProcesses map[string]any     `toml:"processes"`

	// Tables configuring features of later releases, accepted unchecked.
	Go           any `toml:"go"`
	Python       any `toml:"python"`
	Node         any `toml:"node"`
	Bun          any `toml:"bun"`
	Deno         any `toml:"deno"`
	Container    any `toml:"container"`
	Build        any `toml:"build"`
	Release      any `toml:"release"`
	Cron         any `toml:"cron"`
	Routes       any `toml:"routes"`
	Vars         any `toml:"vars"`
	Resources    any `toml:"resources"`
	Storage      any `toml:"storage"`
	Previews     any `toml:"previews"`
	Environments any `toml:"environments"`
	Services     any `toml:"services"`
}

// App is the [app] table.
type App struct {
	Name       string   `toml:"name"`
	Runtime    string   `toml:"runtime"`
	Toolchains []string `toml:"toolchains"`
	Root       string   `toml:"root"`
	Shell      string   `toml:"shell"`
	Watch      []string `toml:"watch"`
}

// Load reads dir/moorage.toml. It returns nil, nil when there is none.
func Load(dir string) (*Manifest, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(data, path)
}

// Parse decodes and validates a manifest; name is used in error messages.
func Parse(data []byte, name string) (*Manifest, error) {
	var m Manifest
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, decodeError(name, err)
	}
	if m.Version != 0 && m.Version != 1 {
		return nil, fmt.Errorf("%s: version %d is not supported (this moor reads version 1)", name, m.Version)
	}
	if m.Static != nil {
		if err := m.Static.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if m.RawProcesses != nil {
		procs, err := decodeProcesses(m.RawProcesses)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := ValidateProcesses(procs); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		m.Processes = procs
	}
	if err := m.Deploy.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	for k := range m.Env {
		if !envName.MatchString(k) {
			return nil, fmt.Errorf("%s: env: %q is not a valid variable name", name, k)
		}
	}
	return &m, nil
}

func decodeError(name string, err error) error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		var lines []string
		for _, e := range strict.Errors {
			row, col := e.Position()
			lines = append(lines, fmt.Sprintf("%s:%d:%d: unknown key %q", name, row, col, strings.Join(e.Key(), ".")))
		}
		return errors.New(strings.Join(lines, "\n"))
	}
	var decode *toml.DecodeError
	if errors.As(err, &decode) {
		row, col := decode.Position()
		return fmt.Errorf("%s:%d:%d: %s", name, row, col, decode.Error())
	}
	return fmt.Errorf("%s: %w", name, err)
}
