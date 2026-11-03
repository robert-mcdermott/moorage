package toolchain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Python is installed by uv into the toolchain directory, from builds whose
// checksums uv carries. uv is told not to link executables into the
// user's bin directory and not to register them with Windows, so nothing
// leaks outside the data directory.

func (m *Manager) pythonDir() string { return filepath.Join(m.dir, "python") }

// uvEnv is the environment uv runs with to manage Pythons.
func (m *Manager) uvEnv() []string {
	env := append(os.Environ(),
		"UV_PYTHON_INSTALL_DIR="+m.pythonDir(),
		"UV_CACHE_DIR="+filepath.Join(m.cacheDir, "uv"),
		"UV_NO_PROGRESS=1",
		"UV_PYTHON_INSTALL_BIN=0",
		"UV_PYTHON_INSTALL_REGISTRY=0",
		"UV_NO_CONFIG=1",
	)
	if m.src.Python != "" {
		env = append(env, "UV_PYTHON_INSTALL_MIRROR="+m.src.Python)
	}
	return env
}

type uvPython struct {
	Key            string  `json:"key"`
	Version        string  `json:"version"`
	Path           *string `json:"path"`
	Implementation string  `json:"implementation"`
	Variant        string  `json:"variant"`
}

// listPythons asks uv for the CPython builds matching request: the
// installed ones, or those it can download.
func (m *Manager) listPythons(ctx context.Context, uv Installed, request string, installedOnly bool) ([]uvPython, error) {
	args := []string{"python", "list"}
	if request != "" {
		args = append(args, request)
	}
	args = append(args, "--managed-python", "--output-format", "json")
	if installedOnly {
		args = append(args, "--only-installed")
	}
	cmd := exec.CommandContext(ctx, uv.Exe, args...)
	cmd.Env = m.uvEnv()
	cmd.Dir = m.dir // not a project: no .python-version is read
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("uv python list: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var all []uvPython
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("uv python list: %w", err)
	}
	var cpython []uvPython
	for _, p := range all {
		if _, ok := parseVersion(p.Version); ok && p.Implementation == "cpython" && p.Variant == "default" {
			cpython = append(cpython, p)
		}
	}
	sort.SliceStable(cpython, func(i, j int) bool {
		a, _ := parseVersion(cpython[i].Version)
		b, _ := parseVersion(cpython[j].Version)
		return b.less(a)
	})
	return cpython, nil
}

func (m *Manager) ensurePython(ctx context.Context, uv Installed, request string, log io.Writer) (Installed, error) {
	if err := os.MkdirAll(m.pythonDir(), 0o755); err != nil {
		return Installed{}, err
	}
	available, err := m.listPythons(ctx, uv, request, false)
	if err != nil {
		return Installed{}, err
	}
	if len(available) == 0 {
		return Installed{}, fmt.Errorf("python: no release matches %q", request)
	}
	ver := available[0].Version
	unlock := m.lock("python")
	defer unlock()
	if in, ok := m.findPython(ver); ok {
		in.Cached = true
		return in, nil
	}
	fmt.Fprintf(log, "installing Python %s with uv %s\n", ver, uv.Version)
	cmd := exec.CommandContext(ctx, uv.Exe, "python", "install", ver, "--no-bin", "--no-registry")
	cmd.Env = m.uvEnv()
	cmd.Dir = m.dir
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return Installed{}, fmt.Errorf("uv python install %s: %w", ver, err)
	}
	in, ok := m.findPython(ver)
	if !ok {
		return Installed{}, fmt.Errorf("python %s was installed but cannot be found", ver)
	}
	return in, nil
}

// findPython returns an installed CPython of exactly this version.
func (m *Manager) findPython(ver string) (Installed, bool) {
	matches, _ := filepath.Glob(filepath.Join(m.pythonDir(), "cpython-"+ver+"-*"))
	for _, dir := range matches {
		if strings.Contains(filepath.Base(dir), "freethreaded") {
			continue
		}
		short := ver
		if v, ok := parseVersion(ver); ok && len(v) >= 2 {
			short = fmt.Sprintf("%d.%d", v[0], v[1])
		}
		candidates := []string{filepath.Join(dir, "bin", "python"+short), filepath.Join(dir, "bin", "python3")}
		bin := filepath.Join(dir, "bin")
		if m.goos == "windows" {
			candidates = []string{filepath.Join(dir, "python.exe")}
			bin = dir
		}
		for _, exe := range candidates {
			if _, err := os.Stat(exe); err == nil {
				return Installed{Name: "python", Version: ver, Dir: dir, Bin: []string{bin}, Exe: exe}, true
			}
		}
	}
	return Installed{}, false
}

func (m *Manager) pythonVersions() []string {
	matches, _ := filepath.Glob(filepath.Join(m.pythonDir(), "cpython-*"))
	seen := map[string]bool{}
	var vs []version
	for _, dir := range matches {
		rest := strings.TrimPrefix(filepath.Base(dir), "cpython-")
		s, _, _ := strings.Cut(rest, "-")
		// uv also links cpython-3.13-… to the newest 3.13 it installed.
		if v, ok := parseVersion(s); ok && len(v) == 3 && !seen[s] {
			if _, found := m.findPython(s); found {
				seen[s] = true
				vs = append(vs, v)
			}
		}
	}
	sort.Slice(vs, func(i, j int) bool { return vs[j].less(vs[i]) })
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.String()
	}
	return out
}
