// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/robert-mcdermott/moorage/internal/build"
	"github.com/robert-mcdermott/moorage/internal/manifest"
)

// maxChecked is how many files and directories are looked at for links that lead out of the root, before saying that a directory is too large to check.
const maxChecked = 200_000

// below resolves a directory an agent named, which has to be the root or below it, whatever links it goes through, and has to be one that an upload would
// not leave out. A tricked agent should not be able to read or upload anything else.
func (t *tools) below(dir string) (string, error) {
	if t.Root == "" {
		return "", errors.New("this server has no directory to read from")
	}
	root, err := filepath.EvalSymlinks(t.Root)
	if err != nil {
		return "", err
	}
	if err := broad(root); err != nil {
		return "", err
	}
	if dir == "" {
		dir = "."
	}
	// Before anything asks the system about it: an address on another machine would be a connection to it, and what exists out there is not for saying.
	asked := dir
	if !filepath.IsAbs(asked) {
		asked = filepath.Join(root, asked)
	}
	asked = filepath.Clean(asked)
	if !within(root, asked) && !within(filepath.Clean(t.Root), asked) {
		return "", refusedInput("%s is outside the directory this server reads from, %s, and it reads nothing else", dir, root)
	}
	resolved, err := filepath.EvalSymlinks(asked)
	if err != nil {
		return "", refusedInput("%s is not a directory here", dir)
	}
	if !within(root, resolved) {
		return "", refusedInput("%s is outside the directory this server reads from, %s, and it reads nothing else", dir, root)
	}
	fi, err := os.Stat(resolved)
	if err != nil || !fi.IsDir() {
		return "", refusedInput("%s is not a directory", dir)
	}
	if err := t.leftOut(root, resolved); err != nil {
		return "", err
	}
	// A link in the tree would have what reads it, such as moorage.toml, read something else.
	if err := t.linksStayIn(root, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// broad refuses a directory that holds more than any one app, which a client can start this in without meaning anything by it.
func broad(root string) error {
	const fix = ": start moor mcp in the app's directory, or give it --root"
	if filepath.Dir(root) == root {
		return refusedInput("this server was started in %s, and will not read from all of it%s", root, fix)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	if within(root, home) {
		return refusedInput("this server was started in %s, which holds your home directory, and will not read from all of it%s", root, fix)
	}
	return nil
}

// within says that path is root or below it.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// leftOut refuses a directory with a part that is hidden, such as .ssh, or that the root's own rules leave out of an upload.
func (t *tools) leftOut(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return err
	}
	var skip func(string, fs.DirEntry) bool
	if t.Skip != nil {
		if skip, err = t.Skip(root); err != nil {
			return err
		}
	}
	path := root
	sofar := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		path, sofar = filepath.Join(path, part), pathpkg.Join(sofar, part)
		if strings.HasPrefix(part, ".") {
			return refusedInput("%s is hidden, and this server does not read such directories", sofar)
		}
		if skip != nil {
			fi, err := os.Lstat(path)
			if err != nil {
				return refusedInput("%s is not a directory here", sofar)
			}
			if skip(sofar, fs.FileInfoToDirEntry(fi)) {
				return refusedInput("%s is left out of uploads by this directory's ignore rules", sofar)
			}
		}
	}
	return nil
}

// linksStayIn refuses a tree with a link that leads out of root. What an upload leaves out does not count: a virtual environment is full of them, and nothing reads it.
func (t *tools) linksStayIn(root, dir string) error {
	var skip func(string, fs.DirEntry) bool
	if t.Skip != nil {
		var err error
		if skip, err = t.Skip(dir); err != nil {
			return err
		}
	}
	seen := 0
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if seen++; seen > maxChecked {
			return refusedInput("%s holds too much to check: name a smaller directory", dir)
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if d.Name() == ".git" || skip != nil && skip(filepath.ToSlash(rel), d) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		// One that leads nowhere reads nothing.
		if target, err := filepath.EvalSymlinks(path); err == nil && !within(root, target) {
			return refusedInput("%s is a link that leads out of the directory this server reads from", filepath.ToSlash(rel))
		}
		return nil
	})
}

// skipFor is what to leave out of dir, by the rules there and by the root's, which name things by their place from there.
func (t *tools) skipFor(dir string) (func(string, fs.DirEntry) bool, error) {
	own, err := t.Skip(dir)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(t.Root)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return own, err
	}
	above, err := t.Skip(root)
	if err != nil {
		return nil, err
	}
	prefix := filepath.ToSlash(rel)
	return func(name string, d fs.DirEntry) bool {
		return own(name, d) || above(prefix+"/"+name, d)
	}, nil
}

type manifestInput struct {
	Dir string `json:"dir,omitempty" jsonschema:"the directory, below where this server reads from; that directory itself if left out"`
}

type manifestCheck struct {
	Valid    bool     `json:"valid"`
	Exists   bool     `json:"exists"`
	Problem  string   `json:"problem,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Plan     string   `json:"plan,omitempty"`
}

func (t *tools) checkManifest(_ context.Context, _ *mcp.CallToolRequest, in manifestInput) (*mcp.CallToolResult, manifestCheck, error) {
	dir, err := t.below(in.Dir)
	if err != nil {
		return nil, manifestCheck{}, err
	}
	m, err := manifest.Load(dir)
	if err != nil {
		return nil, manifestCheck{Exists: true, Problem: cleanText(err.Error())}, nil
	}
	out := manifestCheck{Valid: true, Exists: m != nil}
	if m != nil {
		for _, w := range manifest.Lint(m) {
			out.Warnings = append(out.Warnings, cleanText(w))
		}
	}
	plan, _, err := build.Resolve(dir, runtime.GOOS)
	if err != nil {
		out.Valid, out.Problem = false, cleanText(err.Error())
		return nil, out, nil
	}
	var buf bytes.Buffer
	manifest.Explain(&buf, plan)
	out.Plan = cleanText(buf.String())
	return nil, out, nil
}

type deployInput struct {
	Dir string `json:"dir,omitempty" jsonschema:"the directory to upload, below where this server reads from; that directory itself if left out"`
	App string `json:"app,omitempty" jsonschema:"the app's name; moorage.toml's name if left out"`
	Env string `json:"env" jsonschema:"the environment to deploy to: production, or another's name. A token limited to previews deploys only to a name starting with agent-, which makes that preview"`
}

type deployAnswer struct {
	deploymentRow
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	Next  string `json:"next"`
}

func (t *tools) deploy(ctx context.Context, _ *mcp.CallToolRequest, in deployInput) (*mcp.CallToolResult, deployAnswer, error) {
	dir, err := t.below(in.Dir)
	if err != nil {
		return nil, deployAnswer{}, err
	}
	if in.Env == "" {
		return nil, deployAnswer{}, errors.New("which environment? name one, so that this is not production by accident")
	}
	app := in.App
	if app == "" {
		m, err := manifest.Load(dir)
		if err != nil {
			return nil, deployAnswer{}, err
		}
		if m != nil {
			app = m.App.Name
		}
	}
	if app, err = t.appOf(app); err != nil {
		return nil, deployAnswer{}, fmt.Errorf("%w, or set name under [app] in moorage.toml", err)
	}
	skip, err := t.skipFor(dir)
	if err != nil {
		return nil, deployAnswer{}, err
	}
	d, stats, err := t.Client.DeploySource(ctx, app, in.Env, dir, skip)
	if err != nil {
		return nil, deployAnswer{}, err
	}
	return nil, deployAnswer{deploymentRow: deploymentOf(d), Files: stats.Files, Bytes: stats.Bytes, Next: "Call get_deployment with this id until finished is true."}, nil
}

type rollbackInput struct {
	App     string `json:"app,omitempty" jsonschema:"the app's name; list_apps says what there is"`
	Env     string `json:"env" jsonschema:"the environment"`
	Version int    `json:"version" jsonschema:"the release to serve again; list_releases says what there is"`
}

func (t *tools) rollback(ctx context.Context, _ *mcp.CallToolRequest, in rollbackInput) (*mcp.CallToolResult, deploymentRow, error) {
	app, err := t.appOf(in.App)
	if err != nil {
		return nil, deploymentRow{}, err
	}
	if in.Env == "" || in.Version <= 0 {
		return nil, deploymentRow{}, errors.New("which environment and release? list_releases says what there is")
	}
	d, err := t.Client.Rollback(ctx, app, in.Env, in.Version)
	if err != nil {
		return nil, deploymentRow{}, err
	}
	return nil, deploymentOf(d), nil
}

type restartInput struct {
	App string `json:"app,omitempty" jsonschema:"the app's name; list_apps says what there is"`
	Env string `json:"env" jsonschema:"the environment"`
}

type restartAnswer struct {
	Instances []instanceRow `json:"instances"`
}

func (t *tools) restart(ctx context.Context, _ *mcp.CallToolRequest, in restartInput) (*mcp.CallToolResult, restartAnswer, error) {
	app, err := t.appOf(in.App)
	if err != nil {
		return nil, restartAnswer{}, err
	}
	if in.Env == "" {
		return nil, restartAnswer{}, errors.New("which environment? name one, so that this is not production by accident")
	}
	list, err := t.Client.Restart(ctx, app, in.Env)
	if err != nil {
		return nil, restartAnswer{}, err
	}
	out := restartAnswer{Instances: []instanceRow{}}
	for _, in := range list {
		out.Instances = append(out.Instances, instanceRow{Process: in.Process, Index: in.Index, Release: in.Release, Status: in.Status, Healthy: in.Healthy, Restarts: in.Restarts})
	}
	return nil, out, nil
}
