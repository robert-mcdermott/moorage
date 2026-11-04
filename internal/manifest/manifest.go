// Package manifest reads moorage.toml, the file in an app's repository that
// tells Moorage how to build and run it (spec/03-manifest.md).
//
// Every table of manifest version 1 is parsed and checked; Lint reports the
// ones whose features have not arrived yet. Unknown keys are errors, except
// in top-level tables named x-<something>, which are left to other tools.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

	Go        *Go        `toml:"go"`
	Python    *Python    `toml:"python"`
	Node      *JS        `toml:"node"`
	Bun       *JS        `toml:"bun"`
	Deno      *JS        `toml:"deno"`
	Container *Container `toml:"container"`
	Build     *Build     `toml:"build"`
	Release   *Release   `toml:"release"`

	Cron         []Cron                 `toml:"cron"`
	Routes       []Route                `toml:"routes"`
	Vars         map[string]Var         `toml:"vars"`
	Resources    *Resources             `toml:"resources"`
	Storage      *Storage               `toml:"storage"`
	Previews     *Previews              `toml:"previews"`
	Environments map[string]Environment `toml:"environments"`
	Services     map[string]Service     `toml:"services"`
}

// App is the [app] table.
type App struct {
	Name       string   `toml:"name"`
	Runtime    string   `toml:"runtime"`
	Toolchains []string `toml:"toolchains"`
	Root       string   `toml:"root"`
	Shell      string   `toml:"shell"`
	Watch      []string `toml:"watch"`
	// StripPrefix takes away the path it is served at from what it hears, with path routing.
	StripPrefix bool `toml:"strip_prefix"`
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
		if err = withoutToolTables(err); err != nil {
			return nil, decodeError(name, err)
		}
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
		if err := validateProcesses(procs, false); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		m.Processes = procs
	}
	if err := m.Deploy.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var problems []string
	checkEnv(&problems, "env", m.Env)
	problems = append(problems, m.validateTables()...)
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("%s:\n  - %s", name, strings.Join(problems, "\n  - "))
	}
	return &m, nil
}

// withoutToolTables drops the unknown keys that sit in x-* tables, which
// belong to other tools; it returns nil if no other unknown key remains.
func withoutToolTables(err error) error {
	var strict *toml.StrictMissingError
	if !errors.As(err, &strict) {
		return err
	}
	var kept []toml.DecodeError
	for _, e := range strict.Errors {
		if key := e.Key(); len(key) > 0 && strings.HasPrefix(key[0], "x-") {
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 {
		return nil
	}
	strict.Errors = kept
	return strict
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
