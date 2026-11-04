// Package build turns an app's source into a build: it resolves how to
// build and run the app (its manifest, detection and the runtime presets),
// installs the toolchains it needs and runs the build in the build's final
// directory (spec/04-build-and-deploy.md).
package build

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/manifest"
)

// staticDirs are where a static site usually is, in order.
var staticDirs = []string{"dist", "build", "out", "public", "_site", "site", "."}

// UnresolvedError reports an app Moorage cannot work out how to build or
// run. Its message says what was inspected and what to add to the
// manifest.
type UnresolvedError struct{ Msg string }

func (e *UnresolvedError) Error() string { return e.Msg }

func unresolved(format string, args ...any) error {
	return &UnresolvedError{fmt.Sprintf(format, args...)}
}

// Resolve reads the app in dir — its moorage.toml, if any, and the files
// detection looks at — and returns the plan to build and run it on goos.
// Toolchain versions are as requested; the build resolves them. It also
// returns the manifest (nil without one).
func Resolve(dir, goos string) (manifest.Plan, *manifest.Manifest, error) {
	return ResolveImage(dir, goos, "")
}

// ResolveImage is Resolve for an image named when deploying, if image is not
// empty: it runs as the manifest says, whatever the manifest's runtime.
func ResolveImage(dir, goos, image string) (manifest.Plan, *manifest.Manifest, error) {
	m, err := manifest.Load(dir)
	if err != nil {
		return manifest.Plan{}, nil, err
	}
	if m == nil {
		m = &manifest.Manifest{}
	}
	if image != "" {
		c := manifest.Container{}
		if m.Container != nil {
			c = *m.Container
		}
		c.Image = image
		m.Container = &c
		m.App.Runtime = "image"
	}
	r := &resolver{dir: dir, goos: goos, m: m}
	plan, err := r.resolve()
	return plan, m, err
}

type resolver struct {
	dir  string
	goos string
	m    *manifest.Manifest
	plan manifest.Plan
	// web is the runtime's preset command for the web process, if any.
	web string
}

func (r *resolver) note(format string, args ...any) {
	r.plan.Notes = append(r.plan.Notes, fmt.Sprintf(format, args...))
}

func (r *resolver) resolve() (manifest.Plan, error) {
	m := r.m
	runtime := m.App.Runtime
	if runtime == "" || runtime == "auto" {
		var err error
		if runtime, err = r.detect(); err != nil {
			return manifest.Plan{}, err
		}
	}
	r.plan = manifest.Plan{Kind: manifest.KindProcess, Runtime: runtime, Notes: r.plan.Notes, Deploy: m.Deploy, Env: m.Env, Static: m.Static}
	r.plan.Path = []string{".moorage/bin"}
	if runtime == "dockerfile" || runtime == "image" {
		return r.containerRuntime(runtime)
	}

	var preset []manifest.Step
	var err error
	switch runtime {
	case "go":
		preset, err = r.goRuntime()
	case "python":
		preset, err = r.pythonRuntime()
	case "node":
		preset, err = r.nodeRuntime()
	case "static":
		r.plan.Kind = manifest.KindStatic
	case "command", "binary":
	case "bun", "deno":
		return manifest.Plan{}, unresolved("the %s runtime is not available yet: use runtime = \"command\" with toolchains = [\"%s@system\"], or a node app", runtime, runtime)
	default:
		return manifest.Plan{}, unresolved("unknown runtime %q", runtime)
	}
	if err != nil {
		return manifest.Plan{}, err
	}
	if err := r.extraToolchains(); err != nil {
		return manifest.Plan{}, err
	}

	if b := m.Build; b != nil {
		if b.Command != "" {
			preset = []manifest.Step{{Name: "build", Command: b.Command}}
		}
		if b.Pre != "" {
			preset = append([]manifest.Step{{Name: "pre", Command: b.Pre}}, preset...)
		}
		if b.Post != "" {
			preset = append(preset, manifest.Step{Name: "post", Command: b.Post})
		}
		r.plan.BuildEnv = merge(r.plan.BuildEnv, b.Env)
		r.plan.BuildTimeout = b.Timeout
		r.plan.Exclude = b.Exclude
	}
	r.plan.Steps = preset
	m.ApplyEnvironments(&r.plan)
	r.plan.StripPrefix = m.App.StripPrefix
	for _, c := range m.Cron {
		r.plan.Cron = append(r.plan.Cron, manifest.CronPlan{Name: c.Name, Schedule: c.Schedule, Command: c.Command, Timezone: c.Timezone, Timeout: c.Timeout, Overlap: c.Overlap, Environments: c.Environments})
	}
	for name, v := range m.Vars {
		if r.plan.Vars == nil {
			r.plan.Vars = map[string]manifest.VarPlan{}
		}
		r.plan.Vars[name] = manifest.VarPlan{Description: v.Description, Required: v.Required, Secret: v.Secret, Generate: v.Generate}
	}
	if m.Release != nil && (m.Release.Command != "" || m.Release.OnCreate != "") {
		rel := *m.Release
		r.plan.Release = &rel
	}

	if r.plan.Kind == manifest.KindStatic {
		if len(m.Processes) > 0 {
			r.note("[processes] is ignored: a static app has nothing to run")
		}
		r.plan.Processes = nil
		r.plan.Release = nil
	} else if err := r.processes(); err != nil {
		return manifest.Plan{}, err
	}
	r.plan.Defaults()
	return r.plan, nil
}

// containerRuntime resolves an app that runs in a container: from an image of
// its own, or from one somebody else built. What only the image can say is left
// to the build.
func (r *resolver) containerRuntime(runtime string) (manifest.Plan, error) {
	m := r.m
	cfg := manifest.Container{}
	if m.Container != nil {
		cfg = *m.Container
	}
	c := &manifest.ContainerPlan{Port: cfg.Port, User: cfg.User, Mount: manifest.DefaultMount}
	if m.Storage != nil && m.Storage.Mount != "" {
		c.Mount = m.Storage.Mount
	}
	if res := m.Resources; res != nil {
		c.Memory, c.CPU, c.PIDs = int64(res.Memory), res.CPU, res.PIDs
	}
	if runtime == "image" {
		if cfg.Image == "" {
			return manifest.Plan{}, unresolved("the image runtime needs an image: set image under [container] in moorage.toml, or use moor deploy --image REF")
		}
		c.From = cfg.Image
	} else {
		c.File = cfg.File
		if c.File == "" {
			switch {
			case exists(r.dir, "Containerfile"):
				c.File = "Containerfile"
			case exists(r.dir, "Dockerfile"):
				c.File = "Dockerfile"
			default:
				return manifest.Plan{}, unresolved("the dockerfile runtime needs a Containerfile or a Dockerfile, and this app has neither; name another with file under [container]")
			}
		} else if !exists(r.dir, c.File) {
			return manifest.Plan{}, unresolved("container.file: this app has no %s", c.File)
		}
		c.Context = cfg.Context
		if c.Context == "" {
			c.Context = "."
		}
		c.Target = cfg.Target
		c.Args = cfg.Args
	}
	r.plan.Path = nil
	r.plan.Container = c
	if b := m.Build; b != nil {
		r.plan.BuildTimeout = b.Timeout
		if b.Command != "" || b.Pre != "" || b.Post != "" {
			r.note("[build] commands are ignored: the image is the build")
		}
		if len(b.Env) > 0 && runtime == "dockerfile" {
			args := merge(b.Env, c.Args)
			c.Args = args
		}
	}
	if len(m.App.Toolchains) > 0 {
		r.note("app.toolchains is ignored: an image brings its own")
	}
	if m.Release != nil && (m.Release.Command != "" || m.Release.OnCreate != "") {
		rel := *m.Release
		r.plan.Release = &rel
	}
	if len(m.Processes) > 0 {
		r.plan.Processes = clone(m.Processes)
	} else {
		// The web process runs what the image says to.
		r.plan.Processes = map[string]manifest.Process{manifest.Web: {}}
	}
	if err := manifest.ValidateContainerProcesses(r.plan.Processes); err != nil {
		return manifest.Plan{}, err
	}
	r.plan.Defaults()
	return r.plan, nil
}

// detect picks the runtime from the files of the app.
func (r *resolver) detect() (string, error) {
	signals := []struct {
		files   []string
		runtime string
	}{
		{[]string{"go.mod"}, "go"},
		{[]string{"pyproject.toml", "uv.lock", "requirements.txt"}, "python"},
		{[]string{"deno.json", "deno.jsonc"}, "deno"},
		{[]string{"bun.lock", "bun.lockb"}, "bun"},
		{[]string{"package.json"}, "node"},
		{[]string{"Containerfile", "Dockerfile"}, "dockerfile"},
	}
	for _, s := range signals {
		for _, f := range s.files {
			if exists(r.dir, f) {
				r.note("detected the %s runtime (%s)", s.runtime, f)
				if s.runtime != "dockerfile" && (exists(r.dir, "Containerfile") || exists(r.dir, "Dockerfile")) {
					r.note("a Containerfile or Dockerfile is present; the %s runtime is preferred (set runtime = \"dockerfile\" under [app] to build the image instead)", s.runtime)
				}
				return s.runtime, nil
			}
		}
	}
	if d := staticOutput(r.dir, staticDirs); d != "" {
		r.note("detected a static site (%s/index.html)", d)
		return "static", nil
	}
	if r.m.Processes != nil || exists(r.dir, "Procfile") {
		r.note("no runtime detected; the processes run as they are")
		return "binary", nil
	}
	return "", unresolved("cannot tell how to build this app: there is no go.mod, pyproject.toml, requirements.txt, package.json or index.html, and no moorage.toml saying what to do. Add one, such as:\n\n  [app]\n  runtime = \"command\"\n\n  [build]\n  command = \"make\"\n\n  [processes.web]\n  command = \"./server --port $PORT\"")
}

func (r *resolver) toolchain(name, version, from string) {
	for i, t := range r.plan.Toolchains {
		if t.Name == name {
			r.plan.Toolchains[i] = manifest.Toolchain{Name: name, Version: version, From: from}
			return
		}
	}
	r.plan.Toolchains = append(r.plan.Toolchains, manifest.Toolchain{Name: name, Version: version, From: from})
}

// Toolchains the toolchain manager installs.
var knownToolchains = []string{"go", "node", "python", "uv"}

func (r *resolver) extraToolchains() error {
	for _, t := range r.m.App.Toolchains {
		name, version, _ := strings.Cut(t, "@")
		if version != "system" && !slices.Contains(knownToolchains, name) {
			return unresolved("app.toolchains: %s cannot be installed yet (available: %s); use %s@system for the one on the server's PATH", t, strings.Join(knownToolchains, ", "), name)
		}
		if name == "python" && version != "system" {
			r.toolchain("uv", "", "python needs uv")
		}
		r.toolchain(name, version, "app.toolchains")
	}
	return nil
}

func (r *resolver) exe(name string) string {
	if r.goos == "windows" {
		return name + ".exe"
	}
	return name
}

func (r *resolver) goRuntime() ([]manifest.Step, error) {
	opts := manifest.Go{}
	if r.m.Go != nil {
		opts = *r.m.Go
	}
	mod, err := readGoMod(r.dir)
	if err != nil {
		return nil, unresolved("the go runtime needs a go.mod: %v", err)
	}
	switch {
	case opts.Version != "":
		r.toolchain("go", opts.Version, "go.version")
	case mod.Toolchain != "":
		r.toolchain("go", mod.Toolchain, "go.mod toolchain line")
	case mod.Go != "":
		// The go line is a minimum; the newest patch release of that
		// minor version is used.
		r.toolchain("go", minorOf(mod.Go), "go.mod go line")
	default:
		r.toolchain("go", "", "latest")
	}
	main := opts.Main
	if main == "" {
		if hasMainPackage(r.dir) {
			main = "."
		} else {
			cmds, _ := filepath.Glob(filepath.Join(r.dir, "cmd", "*"))
			var mains []string
			for _, c := range cmds {
				if hasMainPackage(c) {
					mains = append(mains, "./cmd/"+filepath.Base(c))
				}
			}
			if len(mains) != 1 {
				return nil, unresolved("which Go package is the app? There is no main package at the top and %d under cmd/. Set it in moorage.toml:\n\n  [go]\n  main = \"./cmd/server\"", len(mains))
			}
			main = mains[0]
		}
	}
	binary := opts.Binary
	if binary == "" {
		if main == "." {
			binary = lastElement(mod.Module)
		} else {
			binary = lastElement(main)
		}
		if binary == "" || binary == "." {
			binary = "app"
		}
	}
	ldflags := "-s -w"
	if opts.LDFlags != nil {
		ldflags = *opts.LDFlags
	}
	cmd := "go build -trimpath"
	if ldflags != "" {
		cmd += " -ldflags=" + shellQuote(ldflags)
	}
	if len(opts.Tags) > 0 {
		cmd += " -tags=" + strings.Join(opts.Tags, ",")
	}
	cmd += ` -o "$MOORAGE_BIN/` + r.exe(binary) + `" ` + main
	cgo := "0"
	if opts.CGO {
		cgo = "1"
	}
	r.plan.BuildEnv = merge(r.plan.BuildEnv, map[string]string{"CGO_ENABLED": cgo, "GOTOOLCHAIN": "local", "GOFLAGS": "-buildvcs=false"})
	r.web = `"$MOORAGE_BIN/` + r.exe(binary) + `"`
	return []manifest.Step{{Name: "build", Command: cmd}}, nil
}

func (r *resolver) pythonRuntime() ([]manifest.Step, error) {
	opts := manifest.Python{}
	if r.m.Python != nil {
		opts = *r.m.Python
	}
	py, err := readPyProject(r.dir)
	if err != nil {
		return nil, err
	}
	r.toolchain("uv", "", "the python runtime")
	switch v := firstLine(r.dir, ".python-version"); {
	case opts.Version != "":
		r.toolchain("python", opts.Version, "python.version")
	case v != "":
		r.toolchain("python", v, ".python-version")
	case py != nil && py.Project.RequiresPython != "":
		r.toolchain("python", py.Project.RequiresPython, "requires-python")
	default:
		r.toolchain("python", "", "latest")
	}
	manager := opts.Manager
	if manager == "" {
		switch {
		case py != nil:
			manager = "uv"
		case exists(r.dir, "requirements.txt"):
			manager = "uv-pip"
		default:
			return nil, unresolved("the python runtime needs a pyproject.toml or a requirements.txt")
		}
	}
	venvBin := ".venv/bin"
	if r.goos == "windows" {
		venvBin = ".venv/Scripts"
	}
	r.plan.Path = append([]string{venvBin}, r.plan.Path...)
	r.plan.DirEnv = map[string]string{"VIRTUAL_ENV": ".venv"}
	r.plan.RunEnv = map[string]string{"PYTHONUNBUFFERED": "1"}
	r.plan.BuildEnv = merge(r.plan.BuildEnv, map[string]string{"UV_PYTHON_DOWNLOADS": "never", "UV_COMPILE_BYTECODE": "1", "UV_NO_PROGRESS": "1"})

	var steps []manifest.Step
	if manager == "uv" {
		cmd := "uv sync --no-dev"
		if exists(r.dir, "uv.lock") {
			cmd += " --locked"
		} else {
			r.note("there is no uv.lock, so the dependencies are not locked: run `uv lock` and commit uv.lock for reproducible builds")
		}
		for _, g := range opts.Groups {
			cmd += " --group " + shellQuote(g)
		}
		for _, e := range opts.Extras {
			cmd += " --extra " + shellQuote(e)
		}
		steps = []manifest.Step{{Name: "install", Command: cmd}}
	} else {
		steps = []manifest.Step{
			{Name: "venv", Command: "uv venv .venv"},
			{Name: "install", Command: "uv pip install -r requirements.txt"},
		}
	}
	r.web = r.pythonWeb(py)
	return steps, nil
}

// pythonWeb guesses the command of a Python web app.
func (r *resolver) pythonWeb(py *pyProject) string {
	if py != nil && len(py.Project.Scripts) == 1 && py.packaged() {
		for name := range py.Project.Scripts {
			r.note("web runs the project script %s", name)
			return name
		}
	}
	deps := pythonDependencies(r.dir, py)
	files := pythonCandidates(r.dir)
	if deps["uvicorn"] {
		if mod, v := findAppObject(r.dir, files, "FastAPI", "Starlette"); mod != "" {
			r.note("web runs uvicorn on %s:%s", mod, v)
			return "uvicorn " + mod + ":" + v + " --host 127.0.0.1 --port $PORT"
		}
	}
	if deps["gunicorn"] {
		if deps["django"] {
			if pkg := djangoProject(r.dir); pkg != "" {
				r.note("web runs gunicorn on the Django project %s", pkg)
				return "gunicorn " + pkg + ".wsgi --bind 127.0.0.1:$PORT"
			}
		}
		if mod, v := findAppObject(r.dir, files, "Flask"); mod != "" {
			r.note("web runs gunicorn on %s:%s", mod, v)
			return "gunicorn " + mod + ":" + v + " --bind 127.0.0.1:$PORT"
		}
	}
	return ""
}

func (r *resolver) nodeRuntime() ([]manifest.Step, error) {
	opts := manifest.JS{}
	if r.m.Node != nil {
		opts = *r.m.Node
	}
	pkg, err := readPackageJSON(r.dir)
	if err != nil {
		return nil, err
	}
	if pkg == nil {
		return nil, unresolved("the node runtime needs a package.json")
	}
	switch {
	case opts.Version != "":
		r.toolchain("node", opts.Version, "node.version")
	case firstLine(r.dir, ".nvmrc") != "":
		r.toolchain("node", strings.TrimPrefix(firstLine(r.dir, ".nvmrc"), "v"), ".nvmrc")
	case firstLine(r.dir, ".node-version") != "":
		r.toolchain("node", strings.TrimPrefix(firstLine(r.dir, ".node-version"), "v"), ".node-version")
	case pkg.Engines["node"] != "":
		r.toolchain("node", pkg.Engines["node"], "engines.node")
	default:
		r.toolchain("node", "lts", "the current LTS")
	}
	manager := opts.PackageManager
	if manager == "" {
		switch {
		case exists(r.dir, "pnpm-lock.yaml"):
			manager = "pnpm"
		case exists(r.dir, "yarn.lock"):
			manager = "yarn"
		case exists(r.dir, "bun.lock"), exists(r.dir, "bun.lockb"):
			manager = "bun"
		default:
			manager = "npm"
		}
	}
	if manager != "npm" {
		return nil, unresolved("%s is not available yet; this release builds Node apps with npm (a package-lock.json, or none). Set the build yourself under [build] with toolchains = [\"%s@system\"] to use the one on the server", manager, manager)
	}
	r.plan.Path = append([]string{"node_modules/.bin"}, r.plan.Path...)
	r.plan.RunEnv = map[string]string{"NODE_ENV": "production"}
	r.plan.BuildEnv = merge(r.plan.BuildEnv, map[string]string{"npm_config_update_notifier": "false", "npm_config_fund": "false", "npm_config_audit": "false"})

	install := "npm ci"
	if !exists(r.dir, "package-lock.json") {
		install = "npm install"
		r.note("there is no package-lock.json, so the dependencies are not locked: run `npm install` and commit package-lock.json for reproducible builds")
	}
	steps := []manifest.Step{{Name: "install", Command: install}}
	script := opts.BuildScript
	if script == "" && pkg.Scripts["build"] != "" {
		script = "build"
	}
	if script != "" {
		if pkg.Scripts[script] == "" {
			return nil, unresolved("node.build_script %q is not a script of package.json", script)
		}
		steps = append(steps, manifest.Step{Name: "build", Command: "npm run " + shellQuote(script)})
	}
	if opts.Prune == nil || *opts.Prune {
		steps = append(steps, manifest.Step{Name: "prune", Command: "npm prune --omit=dev"})
	}
	if start := pkg.Scripts["start"]; start != "" {
		r.web = start
		r.note("web runs the start script directly: %s", start)
	} else {
		r.plan.StaticFallback = true
	}
	return steps, nil
}

// processes settles what runs: [processes], else a Procfile, else the
// runtime's web preset.
func (r *resolver) processes() error {
	switch {
	case len(r.m.Processes) > 0:
		r.plan.Processes = clone(r.m.Processes)
		if web, ok := r.plan.Processes[manifest.Web]; ok && web.Command == "" && r.web != "" {
			web.Command = r.web
			r.plan.Processes[manifest.Web] = web
		}
		r.plan.StaticFallback = false
	case exists(r.dir, "Procfile"):
		procs, err := readProcfile(r.dir)
		if err != nil {
			return err
		}
		r.plan.Processes = map[string]manifest.Process{}
		for name, cmd := range procs {
			r.plan.Processes[name] = manifest.Process{Command: cmd}
		}
		r.plan.StaticFallback = false
		r.note("the processes come from the Procfile")
	case r.web != "":
		r.plan.Processes = map[string]manifest.Process{manifest.Web: {Command: r.web}}
	case r.plan.StaticFallback:
		r.note("there is no start script: the build's output (%s) is served as a static site", outputList())
		return nil
	default:
		return unresolved("what should run? Moorage found no web command for this %s app. Declare it in moorage.toml, such as:\n\n  [processes.web]\n  command = \"./server --port $PORT\"", r.plan.Runtime)
	}
	return manifest.ValidateProcesses(r.plan.Processes)
}

// Finish settles what only the built app can tell: the directory a static
// app serves. dir is the app's directory after the build.
func Finish(plan *manifest.Plan, dir string) error {
	if plan.StaticFallback && len(plan.Processes) == 0 {
		plan.Kind = manifest.KindStatic
	}
	if plan.Kind != manifest.KindStatic {
		return nil
	}
	if plan.Static == nil {
		plan.Static = &manifest.Static{}
		plan.Static.Defaults()
	}
	site := plan.Static.Dir
	if site == "" {
		candidates := staticDirs
		if plan.StaticFallback {
			candidates = []string{"dist", "build", "out"}
		}
		if site = staticOutput(dir, candidates); site == "" {
			if plan.StaticFallback {
				return unresolved("the app has no start script and its build made no %s with an index.html: add a start script to package.json, or declare [processes.web]", outputList())
			}
			return unresolved("there is no index.html in any of %s; set [static].dir", strings.Join(staticDirs, ", "))
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(site))); err != nil || !fi.IsDir() {
		return unresolved("static.dir %q is not a directory of the built app", site)
	}
	plan.Root = filepath.ToSlash(filepath.Join("app", filepath.FromSlash(site)))
	return nil
}

func outputList() string { return "dist/, build/ or out/" }

// minorOf turns "1.26.3" into "1.26"; "1.26" and "1.26rc1" stay as they are.
func minorOf(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) >= 3 {
		return parts[0] + "." + parts[1]
	}
	return v
}

// lastElement returns the last element of a module or package path,
// skipping a major-version suffix (example.com/tool/v2 gives tool).
func lastElement(p string) string {
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	last := parts[len(parts)-1]
	if len(parts) > 1 && majorSuffix.MatchString(last) {
		last = parts[len(parts)-2]
	}
	return last
}

var majorSuffix = regexp.MustCompile(`^v[0-9]+$`)

// shellQuote quotes s for the embedded shell when it needs quoting.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`;&|<>()*?[]#~=%") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func merge(dst, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func clone(procs map[string]manifest.Process) map[string]manifest.Process {
	out := make(map[string]manifest.Process, len(procs))
	for k, v := range procs {
		out[k] = v
	}
	return out
}
