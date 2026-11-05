// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/robert-mcdermott/moorage/internal/build"
	"github.com/robert-mcdermott/moorage/internal/manifest"
)

// below resolves a directory an agent named, which has to be the root or below it, whatever links it goes through. A tricked agent
// should not be able to read or upload anything else.
func (t *tools) below(dir string) (string, error) {
	if t.Root == "" {
		return "", errors.New("this server has no directory to read from")
	}
	root, err := filepath.EvalSymlinks(t.Root)
	if err != nil {
		return "", err
	}
	if dir == "" {
		dir = "."
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", refusedInput("%s is not a directory here", dir)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", refusedInput("%s is outside the directory this server started in, %s, and it reads nothing else", dir, root)
	}
	fi, err := os.Stat(resolved)
	if err != nil || !fi.IsDir() {
		return "", refusedInput("%s is not a directory", dir)
	}
	return resolved, nil
}

type manifestInput struct {
	Dir string `json:"dir,omitempty" jsonschema:"the directory, below where this server started; that directory itself if left out"`
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
		return nil, manifestCheck{Exists: true, Problem: err.Error()}, nil
	}
	out := manifestCheck{Valid: true, Exists: m != nil}
	if m != nil {
		out.Warnings = manifest.Lint(m)
	}
	plan, _, err := build.Resolve(dir, runtime.GOOS)
	if err != nil {
		out.Valid, out.Problem = false, err.Error()
		return nil, out, nil
	}
	var buf bytes.Buffer
	manifest.Explain(&buf, plan)
	out.Plan = buf.String()
	return nil, out, nil
}

type deployInput struct {
	Dir string `json:"dir,omitempty" jsonschema:"the directory to upload, below where this server started; that directory itself if left out"`
	App string `json:"app,omitempty" jsonschema:"the app's name; moorage.toml's name if left out"`
	Env string `json:"env" jsonschema:"the environment to deploy to: production, or another's name"`
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
	skip, err := t.Skip(dir)
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
