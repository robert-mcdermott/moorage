package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ignore"
	"github.com/robert-mcdermott/moorage/internal/ingress/static"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/oneoff"
	"github.com/robert-mcdermott/moorage/internal/toolchain"
)

// Builder builds apps in place.
type Builder struct {
	DataDir    string
	Toolchains *toolchain.Manager
	// Shell runs `moor sh -c` for commands that are not simple; ShellEnv is
	// added to its environment.
	Shell    []string
	ShellEnv []string
}

// Job is one build.
type Job struct {
	App, Env string
	BuildID  string
	// Dir is the build's directory, builds/<app>/<id>: the source is
	// already in Dir/app, where it is built and later run.
	Dir string
	// Log receives the build's output as log lines.
	Log io.Writer
}

// Result is a finished build.
type Result struct {
	Plan  manifest.Plan
	Files int
	Size  int64
}

// Error reports a build that failed, with a message for the deployment.
type Error struct {
	Msg string
	Err error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// Marker writes a line of Moorage's own (stream M) to a log.
func Marker(w io.Writer, format string, args ...any) {
	io.WriteString(w, logs.Format(time.Now(), logs.Moorage, fmt.Sprintf(format, args...)))
}

// Build resolves, prepares and builds the app in job.Dir/app, and leaves it
// ready to run: tidied, measured and read-only.
func (b *Builder) Build(ctx context.Context, job Job) (Result, error) {
	appDir := filepath.Join(job.Dir, "app")
	plan, _, err := Resolve(appDir, runtime.GOOS)
	if err != nil {
		var u *UnresolvedError
		if errors.As(err, &u) {
			return Result{}, &Error{Msg: u.Msg, Err: err}
		}
		return Result{}, &Error{Msg: err.Error(), Err: err}
	}
	Marker(job.Log, "▸ detected   %s", describe(plan))
	for _, n := range plan.Notes {
		Marker(job.Log, "             %s", n)
	}

	vars, err := b.environment(job, appDir)
	if err != nil {
		return Result{}, err
	}
	bins, err := b.toolchains(ctx, job, &plan, vars)
	if err != nil {
		return Result{}, err
	}
	var path []string
	path = append(path, bins...)
	for _, p := range plan.Path {
		path = append(path, filepath.Join(appDir, filepath.FromSlash(p)))
	}
	for k, v := range plan.Env {
		vars[k] = v
	}
	for k, v := range plan.BuildEnv {
		vars[k] = v
	}
	vars["PATH"] = oneoff.PrependPath(path, vars["PATH"])

	if len(plan.Steps) > 0 {
		timeout := plan.BuildTimeout.Std()
		buildCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		out, errw := logs.NewWriter(job.Log, logs.Stdout), logs.NewWriter(job.Log, logs.Stderr)
		for _, step := range plan.Steps {
			Marker(job.Log, "▸ %-10s %s", step.Name, step.Command)
			start := time.Now()
			err := oneoff.Run(buildCtx, oneoff.Command{
				Command: step.Command, Env: oneoff.List(vars), Dir: appDir,
				Shell: b.Shell, ShellEnv: b.ShellEnv, Stdout: out, Stderr: errw,
			})
			out.Flush()
			errw.Flush()
			switch {
			case err == nil:
				Marker(job.Log, "             done in %s", time.Since(start).Round(10*time.Millisecond))
			case errors.Is(buildCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
				return Result{}, &Error{Msg: fmt.Sprintf("the build did not finish within %s ([build].timeout)", timeout), Err: err}
			case ctx.Err() != nil:
				return Result{}, ctx.Err()
			default:
				var exit *oneoff.ExitError
				if errors.As(err, &exit) {
					return Result{}, &Error{Msg: fmt.Sprintf("the %s step failed: %s exited with status %d", step.Name, step.Command, exit.Code), Err: err}
				}
				return Result{}, &Error{Msg: fmt.Sprintf("the %s step failed: %v", step.Name, err), Err: err}
			}
		}
	}

	if err := Finish(&plan, appDir); err != nil {
		var u *UnresolvedError
		if errors.As(err, &u) {
			return Result{}, &Error{Msg: u.Msg, Err: err}
		}
		return Result{}, err
	}
	if err := exclude(appDir, plan.Exclude); err != nil {
		return Result{}, err
	}
	if plan.Kind == manifest.KindStatic {
		idx, err := static.BuildIndex(filepath.Join(job.Dir, filepath.FromSlash(plan.Root)))
		if err != nil {
			return Result{}, err
		}
		if err := idx.WriteIndex(filepath.Join(job.Dir, "index.json")); err != nil {
			return Result{}, err
		}
	}
	files, size, err := measure(appDir)
	if err != nil {
		return Result{}, err
	}
	if err := MakeReadOnly(appDir); err != nil {
		return Result{}, err
	}
	return Result{Plan: plan, Files: files, Size: size}, nil
}

func describe(plan manifest.Plan) string {
	s := plan.Runtime
	for _, t := range plan.Toolchains {
		v := t.Version
		if v == "" {
			v = "latest"
		}
		s += fmt.Sprintf(" · %s %s", t.Name, v)
		if t.From != "" {
			s += " (" + t.From + ")"
		}
	}
	return s
}

// environment is what build commands run with, before the toolchains,
// [env] and [build.env] are added.
func (b *Builder) environment(job Job, appDir string) (map[string]string, error) {
	cache := filepath.Join(b.DataDir, "cache")
	tmp := filepath.Join(b.DataDir, "tmp", "build-"+job.BuildID)
	home := filepath.Join(tmp, "home")
	bin := filepath.Join(appDir, ".moorage", "bin")
	for _, d := range []string{home, bin, cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	vars := oneoff.BaseEnv()
	for k, v := range map[string]string{
		"HOME": home, "TMPDIR": tmp, "TEMP": tmp, "TMP": tmp,
		"CI":                "true",
		"MOORAGE_APP":       job.App,
		"MOORAGE_ENV":       job.Env,
		"MOORAGE_BUILD_ID":  job.BuildID,
		"MOORAGE_BUILD_DIR": appDir,
		"MOORAGE_BIN":       bin,
		// Shared caches, on the same filesystem as builds so that package
		// managers can link files instead of copying them.
		"GOMODCACHE":       filepath.Join(cache, "go", "mod"),
		"GOCACHE":          filepath.Join(cache, "go", "build"),
		"GOPATH":           filepath.Join(cache, "go", "path"),
		"UV_CACHE_DIR":     filepath.Join(cache, "uv"),
		"npm_config_cache": filepath.Join(cache, "npm"),
	} {
		vars[k] = v
	}
	if runtime.GOOS == "windows" {
		// Builds are made read-only, which on Windows would also lock the
		// cache's files if they were hard links.
		vars["UV_LINK_MODE"] = "copy"
	}
	return vars, nil
}

// toolchains installs the plan's toolchains, records their exact versions
// in the plan, and returns their PATH directories.
func (b *Builder) toolchains(ctx context.Context, job Job, plan *manifest.Plan, vars map[string]string) ([]string, error) {
	if len(plan.Toolchains) == 0 {
		return nil, nil
	}
	out := logs.NewWriter(job.Log, logs.Stdout)
	defer out.Flush()
	var bins, summary []string
	var uv *toolchain.Installed
	// uv first: it installs Python.
	order := append([]manifest.Toolchain{}, plan.Toolchains...)
	for i, t := range order {
		if t.Name == "uv" && i > 0 {
			order = append(append([]manifest.Toolchain{t}, order[:i]...), order[i+1:]...)
			break
		}
	}
	resolved := map[string]string{}
	for _, t := range order {
		if t.Version == "system" {
			summary = append(summary, t.Name+" (the server's)")
			continue
		}
		if b.Toolchains == nil {
			return nil, &Error{Msg: "this server cannot install toolchains"}
		}
		in, err := b.Toolchains.Ensure(ctx, t.Name, t.Version, uv, out)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &Error{Msg: fmt.Sprintf("toolchain %s %s: %v", t.Name, orLatest(t.Version), err), Err: err}
		}
		if t.Name == "uv" {
			uv = &in
		}
		if t.Name == "python" {
			vars["UV_PYTHON"] = in.Exe
		}
		resolved[t.Name] = in.Version
		bins = append(bins, in.Bin...)
		s := t.Name + " " + in.Version
		if in.Cached {
			s += " (cached)"
		}
		summary = append(summary, s)
	}
	for i, t := range plan.Toolchains {
		if v, ok := resolved[t.Name]; ok {
			plan.Toolchains[i].Version = v
		}
	}
	Marker(job.Log, "▸ toolchains %s", strings.Join(summary, ", "))
	return bins, nil
}

func orLatest(v string) string {
	if v == "" {
		return "latest"
	}
	return v
}

// exclude removes the paths matching globs ([build].exclude, with
// .gitignore syntax) and the files of .moorignore from a built app.
func exclude(dir string, globs []string) error {
	m := ignore.New(globs...)
	if err := m.AddFile("", filepath.Join(dir, ".moorignore")); err != nil {
		return err
	}
	if m.Empty() {
		return nil
	}
	var remove []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, dir+string(filepath.Separator)))
		if path.Dir(rel) == ".moorage" || rel == ".moorage" {
			return nil
		}
		if m.Match(rel, d.IsDir()) {
			remove = append(remove, p)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range remove {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

func measure(dir string) (files int, size int64, err error) {
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files++
			size += info.Size()
		}
		return nil
	})
	return files, size, err
}

// MakeReadOnly removes write permission from the files under dir, so a
// published build cannot be changed by accident. Executables stay
// executable, and directories stay writable (Python writes its bytecode
// caches next to the sources).
func MakeReadOnly(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := os.Chmod(p, info.Mode().Perm()&^0o222); err != nil {
			return fmt.Errorf("make %s read-only: %w", strings.TrimPrefix(p, dir), err)
		}
		return nil
	})
}
