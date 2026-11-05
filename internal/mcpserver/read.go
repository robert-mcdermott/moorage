// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
)

type none struct{}

// maxEnvironments is how many environments get_app looks at closely: an app can have a great many previews.
const maxEnvironments = 20

type appRow struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	Project    string `json:"project,omitempty"`
	Production int    `json:"production_release,omitempty"`
}

type appList struct {
	Apps []appRow `json:"apps"`
}

func (t *tools) listApps(ctx context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, appList, error) {
	apps, err := t.Client.Apps(ctx)
	if err != nil {
		return nil, appList{}, err
	}
	out := appList{Apps: []appRow{}}
	for _, a := range apps {
		row := appRow{Name: a.Name, URL: a.URL, Project: a.Project}
		if a.Production != nil {
			row.Production = a.Production.Version
		}
		out.Apps = append(out.Apps, row)
	}
	return nil, out, nil
}

type appInput struct {
	App string `json:"app,omitempty" jsonschema:"the app's name; list_apps says what there is"`
}

type instanceRow struct {
	Process  string `json:"process"`
	Index    int    `json:"index"`
	Release  int    `json:"release"`
	Status   string `json:"status"`
	Healthy  bool   `json:"healthy"`
	Restarts int    `json:"restarts"`
	LastExit *int   `json:"last_exit_code,omitempty"`
}

type envRow struct {
	Name      string        `json:"name"`
	Kind      string        `json:"kind,omitempty"`
	URL       string        `json:"url"`
	Release   int           `json:"release,omitempty"`
	Asleep    bool          `json:"asleep,omitempty"`
	LockedBy  string        `json:"locked_by,omitempty"`
	Instances []instanceRow `json:"instances"`
}

type appStatus struct {
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	Project      string   `json:"project,omitempty"`
	Environments []envRow `json:"environments"`
}

func (t *tools) getApp(ctx context.Context, _ *mcp.CallToolRequest, in appInput) (*mcp.CallToolResult, appStatus, error) {
	app, err := t.appOf(in.App)
	if err != nil {
		return nil, appStatus{}, err
	}
	info, err := t.Client.AppInfo(ctx, app)
	if err != nil {
		return nil, appStatus{}, err
	}
	envs, err := t.Client.Environments(ctx, app)
	if err != nil {
		return nil, appStatus{}, err
	}
	out := appStatus{Name: info.Name, URL: info.URL, Project: info.Project, Environments: []envRow{}}
	for i, e := range envs {
		// The list says little of each. A token may not reach them all, which is no reason to say nothing of the rest.
		if i < maxEnvironments {
			if full, err := t.Client.Environment(ctx, app, e.Name); err == nil {
				e = full
			}
		}
		row := envRow{Name: e.Name, Kind: e.Kind, URL: e.URL, Asleep: e.Asleep, LockedBy: e.LockedBy, Instances: []instanceRow{}}
		if e.Release != nil {
			row.Release = e.Release.Version
		}
		for _, in := range e.Instances {
			i := instanceRow{Process: in.Process, Index: in.Index, Release: in.Release, Status: in.Status, Healthy: in.Healthy, Restarts: in.Restarts}
			if in.LastExit != nil {
				code := in.LastExit.Code
				i.LastExit = &code
			}
			row.Instances = append(row.Instances, i)
		}
		out.Environments = append(out.Environments, row)
	}
	return nil, out, nil
}

type deploymentRow struct {
	ID       string    `json:"id"`
	App      string    `json:"app"`
	Env      string    `json:"env"`
	URL      string    `json:"url"`
	Status   string    `json:"status"`
	Finished bool      `json:"finished"`
	Release  int       `json:"release,omitempty"`
	Ref      string    `json:"ref,omitempty"`
	Commit   string    `json:"commit,omitempty"`
	Error    string    `json:"error,omitempty"`
	Created  time.Time `json:"created_at"`
}

func deploymentOf(d apitypes.Deployment) deploymentRow {
	row := deploymentRow{ID: d.ID, App: d.App, Env: d.Env, URL: d.URL, Status: d.Status, Finished: d.Finished(), Ref: d.Ref, Commit: d.Commit, Error: d.Error, Created: d.CreatedAt}
	if d.Release != nil {
		row.Release = d.Release.Version
	}
	return row
}

type deploymentsInput struct {
	App   string `json:"app,omitempty" jsonschema:"the app's name; list_apps says what there is"`
	Env   string `json:"env,omitempty" jsonschema:"the environment, or production if left out"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many, up to 50; 10 if left out"`
}

type deploymentList struct {
	Deployments []deploymentRow `json:"deployments"`
}

func (t *tools) listDeployments(ctx context.Context, _ *mcp.CallToolRequest, in deploymentsInput) (*mcp.CallToolResult, deploymentList, error) {
	app, err := t.appOf(in.App)
	if err != nil {
		return nil, deploymentList{}, err
	}
	list, err := t.Client.Deployments(ctx, app, envOf(in.Env), bounded(in.Limit, 10, 50))
	if err != nil {
		return nil, deploymentList{}, err
	}
	out := deploymentList{Deployments: []deploymentRow{}}
	for _, d := range list {
		out.Deployments = append(out.Deployments, deploymentOf(d))
	}
	return nil, out, nil
}

type deploymentInput struct {
	ID       string `json:"id" jsonschema:"the deployment's id, from deploy or list_deployments"`
	LogLines int    `json:"log_lines,omitempty" jsonschema:"how many of its log's last lines, up to 500; 50 if left out"`
}

type deploymentDetail struct {
	deploymentRow
	untrustedLog
}

func (t *tools) getDeployment(ctx context.Context, _ *mcp.CallToolRequest, in deploymentInput) (*mcp.CallToolResult, deploymentDetail, error) {
	d, err := t.Client.Deployment(ctx, in.ID)
	if err != nil {
		return nil, deploymentDetail{}, err
	}
	out := deploymentDetail{deploymentRow: deploymentOf(d)}
	var lines []string
	// Someone kept from logs still learns how the deployment stands.
	err = t.Client.DeploymentLog(ctx, in.ID, func(l apitypes.LogLine) error {
		lines = append(lines, l.Text)
		if most := bounded(in.LogLines, 50, 500); len(lines) > most {
			lines = lines[1:]
		}
		return nil
	})
	if err == nil {
		out.untrustedLog = fence(lines)
	}
	return nil, out, nil
}

type logsInput struct {
	App      string `json:"app,omitempty" jsonschema:"the app's name; list_apps says what there is"`
	Env      string `json:"env,omitempty" jsonschema:"the environment, or production if left out"`
	Lines    int    `json:"lines,omitempty" jsonschema:"how many of the newest lines, up to 1000; 100 if left out"`
	Process  string `json:"process,omitempty" jsonschema:"only this process, such as web"`
	Grep     string `json:"grep,omitempty" jsonschema:"only lines with this text"`
	Since    string `json:"since,omitempty" jsonschema:"only since this long ago, such as 10m, or this time"`
	Problems bool   `json:"problems,omitempty" jsonschema:"only errors and warnings"`
}

func (t *tools) readLogs(ctx context.Context, _ *mcp.CallToolRequest, in logsInput) (*mcp.CallToolResult, untrustedLog, error) {
	app, err := t.appOf(in.App)
	if err != nil {
		return nil, untrustedLog{}, err
	}
	var lines []string
	err = t.Client.Logs(ctx, app, envOf(in.Env), client.LogQuery{Process: in.Process, Grep: in.Grep, Since: in.Since, Problems: in.Problems, Tail: bounded(in.Lines, 100, 1000)}, func(l apitypes.LogLine) error {
		lines = append(lines, logLine(l))
		return nil
	})
	if err != nil {
		return nil, untrustedLog{}, err
	}
	return nil, fence(lines), nil
}

type releasesInput struct {
	App string `json:"app,omitempty" jsonschema:"the app's name; list_apps says what there is"`
	Env string `json:"env,omitempty" jsonschema:"the environment, or production if left out"`
}

type releaseRow struct {
	Version int       `json:"version"`
	Reason  string    `json:"reason"`
	Current bool      `json:"current,omitempty"`
	By      string    `json:"created_by,omitempty"`
	Created time.Time `json:"created_at"`
}

type releaseList struct {
	Releases []releaseRow `json:"releases"`
}

func (t *tools) listReleases(ctx context.Context, _ *mcp.CallToolRequest, in releasesInput) (*mcp.CallToolResult, releaseList, error) {
	app, err := t.appOf(in.App)
	if err != nil {
		return nil, releaseList{}, err
	}
	list, err := t.Client.Releases(ctx, app, envOf(in.Env))
	if err != nil {
		return nil, releaseList{}, err
	}
	out := releaseList{Releases: []releaseRow{}}
	for _, r := range list {
		out.Releases = append(out.Releases, releaseRow{Version: r.Version, Reason: r.Reason, Current: r.Current, By: r.CreatedBy, Created: r.CreatedAt})
	}
	return nil, out, nil
}
