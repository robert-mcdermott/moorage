package build

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Files of an app that detection reads, in one place so that a source tree
// is inspected the same way by `moor manifest explain` and by the server.

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
	return err == nil
}

func isDir(dir, name string) bool {
	fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
	return err == nil && fi.IsDir()
}

// firstLine returns the first line of a file that is neither empty nor a
// comment.
func firstLine(dir, name string) string {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

// goMod is what detection needs from go.mod.
type goMod struct {
	Module    string
	Go        string // the go line
	Toolchain string // the toolchain line, without "go"
}

func readGoMod(dir string) (goMod, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return goMod{}, err
	}
	var m goMod
	for _, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "//")
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "module":
			m.Module = strings.Trim(fields[1], `"`)
		case "go":
			m.Go = fields[1]
		case "toolchain":
			m.Toolchain = strings.TrimPrefix(fields[1], "go")
		}
	}
	return m, nil
}

var packageMain = regexp.MustCompile(`(?m)^package\s+main\s*$`)

// hasMainPackage reports whether a directory holds a Go main package.
func hasMainPackage(dir string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			continue
		}
		data, err := os.ReadFile(m)
		if err == nil && packageMain.Match(data) {
			return true
		}
	}
	return false
}

// pyProject is what detection needs from pyproject.toml.
type pyProject struct {
	Project struct {
		Name           string            `toml:"name"`
		RequiresPython string            `toml:"requires-python"`
		Dependencies   []string          `toml:"dependencies"`
		Scripts        map[string]string `toml:"scripts"`
	} `toml:"project"`
	BuildSystem *struct {
		Backend string `toml:"build-backend"`
	} `toml:"build-system"`
	Tool struct {
		UV struct {
			Package *bool `toml:"package"`
		} `toml:"uv"`
	} `toml:"tool"`
}

func readPyProject(dir string) (*pyProject, error) {
	data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p pyProject
	if err := toml.Unmarshal(data, &p); err != nil {
		return nil, errors.New("pyproject.toml: " + err.Error())
	}
	return &p, nil
}

// packaged reports whether uv installs the project itself (and so its
// scripts) into the virtual environment.
func (p *pyProject) packaged() bool {
	if p.Tool.UV.Package != nil {
		return *p.Tool.UV.Package
	}
	return p.BuildSystem != nil
}

var requirementName = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)`)

// pythonDependencies returns the normalised names of the direct
// dependencies in pyproject.toml or requirements.txt.
func pythonDependencies(dir string, p *pyProject) map[string]bool {
	var reqs []string
	if p != nil {
		reqs = p.Project.Dependencies
	}
	if data, err := os.ReadFile(filepath.Join(dir, "requirements.txt")); err == nil {
		reqs = append(reqs, strings.Split(string(data), "\n")...)
	}
	deps := map[string]bool{}
	for _, r := range reqs {
		if strings.HasPrefix(strings.TrimSpace(r), "#") || strings.HasPrefix(strings.TrimSpace(r), "-") {
			continue
		}
		if m := requirementName.FindStringSubmatch(r); m != nil {
			deps[strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(m[1]))] = true
		}
	}
	return deps
}

// packageJSON is what detection needs from package.json.
type packageJSON struct {
	Scripts        map[string]string `json:"scripts"`
	Engines        map[string]string `json:"engines"`
	PackageManager string            `json:"packageManager"`
}

func readPackageJSON(dir string) (*packageJSON, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p packageJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, errors.New("package.json: " + err.Error())
	}
	return &p, nil
}

// readProcfile reads the "name: command" lines of a Procfile.
func readProcfile(dir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "Procfile"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	procs := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, command, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(command) == "" {
			return nil, errors.New("Procfile line " + strconv.Itoa(i+1) + " is not \"name: command\"")
		}
		procs[strings.TrimSpace(name)] = strings.TrimSpace(command)
	}
	return procs, nil
}

// findAppObject looks in the given Python files for a module-level
// assignment of one of the constructors (`app = FastAPI(`) and returns the
// module (a.b) and the variable.
func findAppObject(dir string, files []string, constructors ...string) (module, variable string) {
	re := regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)\s*(?::[^=]+)?=\s*(?:[A-Za-z_][A-Za-z0-9_]*\.)?(` + strings.Join(constructors, "|") + `)\(`)
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		if m := re.FindSubmatch(data); m != nil {
			mod := strings.TrimSuffix(f, ".py")
			mod = strings.TrimSuffix(mod, "/__init__")
			return strings.ReplaceAll(mod, "/", "."), string(m[1])
		}
	}
	return "", ""
}

// pythonCandidates lists the files where a web app object usually lives.
func pythonCandidates(dir string) []string {
	files := []string{"main.py", "app.py", "server.py", "asgi.py", "wsgi.py", "api.py"}
	entries, _ := os.ReadDir(dir)
	var pkgs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != "tests" && exists(dir, e.Name()+"/__init__.py") {
			pkgs = append(pkgs, e.Name())
		}
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		for _, f := range []string{"main.py", "app.py", "server.py", "asgi.py", "__init__.py"} {
			files = append(files, p+"/"+f)
		}
	}
	return files
}

// djangoProject returns the package holding wsgi.py next to a settings
// module, if any.
func djangoProject(dir string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "wsgi.py"))
	sort.Strings(matches)
	for _, m := range matches {
		pkg := filepath.Base(filepath.Dir(m))
		if exists(dir, pkg+"/settings.py") || isDir(dir, pkg+"/settings") {
			return pkg
		}
	}
	return ""
}

// staticOutput returns the first of the usual output directories that holds
// an index.html, or "".
func staticOutput(dir string, candidates []string) string {
	for _, c := range candidates {
		if exists(dir, c+"/index.html") {
			return c
		}
	}
	return ""
}
