// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/mcpserver"
)

// mcpSession is an agent's session with a real server, through a server of tools in this process.
func mcpSession(t *testing.T, c *client.Client, root string, allowDeploy bool) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	srv := mcpserver.New(mcpserver.Options{Client: c, AllowDeploy: allowDeploy, Root: root, Skip: func(string) (func(string, os.DirEntry) bool, error) {
		return func(string, os.DirEntry) bool { return false }, nil
	}, Version: "test"})
	if _, err := srv.Connect(ctx, serverSide, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func callTool[T any](t *testing.T, s *mcp.ClientSession, name string, args map[string]any) T {
	t.Helper()
	var out T
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s: %s", name, toolError(res))
	}
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func toolError(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func TestAnAgentDeploysAndLooks(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	admin := apiClient(t, cfg, addrs)
	ctx := context.Background()

	root := writeSite(t, appManifest(""))
	if err := os.WriteFile(filepath.Join(root, "moorage.toml"), []byte(appManifest("")["moorage.toml"]+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := mcpSession(t, admin, root, true)

	// It deploys, and is told at once how to find out how that went.
	type row struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Finished bool   `json:"finished"`
		URL      string `json:"url"`
		Error    string `json:"error"`
		Log      string `json:"log"`
		Next     string `json:"next"`
		Files    int    `json:"files"`
	}
	d := callTool[row](t, s, "deploy", map[string]any{"app": "shop", "env": "production"})
	if d.ID == "" || d.Finished || d.Next == "" || d.Files == 0 {
		t.Fatalf("deploy: %+v", d)
	}
	waitUntil(t, "the deployment to end", func() bool { return callTool[row](t, s, "get_deployment", map[string]any{"id": d.ID}).Finished })
	got := callTool[row](t, s, "get_deployment", map[string]any{"id": d.ID})
	if got.Status != apitypes.StatusLive || !strings.HasPrefix(got.Log, "<untrusted-") {
		t.Errorf("get_deployment: %+v", got)
	}

	// What exists, and how it stands.
	apps := callTool[struct {
		Apps []struct {
			Name       string `json:"name"`
			Production int    `json:"production_release"`
		} `json:"apps"`
	}](t, s, "list_apps", nil)
	if len(apps.Apps) != 1 || apps.Apps[0].Name != "shop" || apps.Apps[0].Production != 1 {
		t.Errorf("list_apps: %+v", apps)
	}
	status := callTool[struct {
		Environments []struct {
			Name      string `json:"name"`
			Release   int    `json:"release"`
			Instances []struct {
				Process string `json:"process"`
				Healthy bool   `json:"healthy"`
			} `json:"instances"`
		} `json:"environments"`
	}](t, s, "get_app", map[string]any{"app": "shop"})
	if len(status.Environments) != 1 || status.Environments[0].Name != "production" || status.Environments[0].Release != 1 || len(status.Environments[0].Instances) == 0 {
		t.Errorf("get_app: %+v", status)
	}
	if list := callTool[struct {
		Deployments []row `json:"deployments"`
	}](t, s, "list_deployments", map[string]any{"app": "shop"}); len(list.Deployments) != 1 || list.Deployments[0].ID != d.ID {
		t.Errorf("list_deployments: %+v", list)
	}
	if list := callTool[struct {
		Releases []struct {
			Version int  `json:"version"`
			Current bool `json:"current"`
		} `json:"releases"`
	}](t, s, "list_releases", map[string]any{"app": "shop"}); len(list.Releases) != 1 || !list.Releases[0].Current {
		t.Errorf("list_releases: %+v", list)
	}

	// Its logs come back as data. What an app says of a secret is as redacted here as anywhere.
	if _, err := admin.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "API_KEY", Value: "a-long-secret-value", Secret: true}}}); err != nil {
		t.Fatal(err)
	}
	logs := callTool[struct {
		Log   string `json:"log"`
		Note  string `json:"note"`
		Lines int    `json:"lines"`
	}](t, s, "read_logs", map[string]any{"app": "shop", "lines": 20})
	if logs.Lines > 0 && (!strings.HasPrefix(logs.Log, "<untrusted-") || logs.Note == "") {
		t.Errorf("read_logs: %+v", logs)
	}

	// It can go back, and start again.
	callTool[row](t, s, "restart", map[string]any{"app": "shop", "env": "production"})
	waitUntil(t, "a second release", func() bool {
		rels, err := admin.Releases(ctx, "shop", "production")
		return err == nil && len(rels) >= 2
	})
	rolled := callTool[row](t, s, "rollback", map[string]any{"app": "shop", "env": "production", "version": 1})
	if rolled.ID == "" {
		t.Errorf("rollback: %+v", rolled)
	}

	// Production is not what it does by accident, and only what is below where it started goes up.
	for name, args := range map[string]map[string]any{
		"no environment":   {"app": "shop"},
		"outside":          {"app": "shop", "env": "production", "dir": ".."},
		"an absolute path": {"app": "shop", "env": "production", "dir": os.TempDir()},
	} {
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "deploy", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Errorf("deploying with %s: %s", name, toolError(res))
		}
	}

	// An app it is told about that does not exist says so.
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "get_app", Arguments: map[string]any{"app": "nosuchapp"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(toolError(res), "nosuchapp") {
		t.Errorf("no such app: %s", toolError(res))
	}
}

// What an agent reads, a person of the same token could: a read token can read, and that is all.
func TestAnAgentsTokenIsWhatLimitsIt(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	admin := apiClient(t, cfg, addrs)
	root := writeSite(t, appManifest(""))
	deployedSource(t, admin, "shop", appManifest(""))
	reader := addMember(t, admin, addrs, "reader@example.com")
	secret := scopedToken(t, reader, "read")
	readOnly, err := client.New("http://"+addrs.HTTP.String(), secret, "")
	if err != nil {
		t.Fatal(err)
	}

	// Offered deploying by mistake, the API still says no.
	s := mcpSession(t, readOnly, root, true)
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "deploy", Arguments: map[string]any{"app": "shop", "env": "production"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("a read token deployed: %s", toolError(res))
	}
	if got := callTool[struct {
		Apps []any `json:"apps"`
	}](t, s, "list_apps", nil); len(got.Apps) != 1 {
		t.Errorf("list_apps: %+v", got)
	}
}
