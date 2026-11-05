// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
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

// TestATokenLimitedToPreviews keeps an agent's token to previews that stand apart from production, in the API and in git.
func TestATokenLimitedToPreviews(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	admin := apiClient(t, cfg, addrs)
	api := caller{t, cfg, addrs}
	ctx := context.Background()
	deployedSource(t, admin, "shop", appManifest(""))
	const secretValue = "production-secret-value"
	if _, err := admin.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{Set: []apitypes.VarSet{
		{Name: "API_KEY", Value: secretValue, Secret: true},
		{Name: "PLAIN_SETTING", Value: "plain-value"},
	}}); err != nil {
		t.Fatal(err)
	}
	// A preview as a push makes it, which takes production's variables.
	if _, err := testAuth(t, cfg).DB.CreateEnvironment(ctx, "shop", "inherits-it", "preview", "inherits-it"); err != nil {
		t.Fatal(err)
	}
	pushed, _, err := admin.DeploySource(ctx, "shop", "inherits-it", writeSite(t, appManifest("")), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the preview to deploy", func() bool {
		d, err := admin.Deployment(ctx, pushed.ID)
		return err == nil && d.Finished()
	})

	// Only a token that could be limited can be.
	for _, scope := range []string{"admin", "gate"} {
		if _, err := admin.CreateToken(ctx, apitypes.NewToken{Name: "x" + scope, Scopes: []string{scope}, Envs: "previews"}); status(err) != http.StatusUnprocessableEntity {
			t.Errorf("a %s token limited to previews: %v", scope, err)
		}
	}
	if _, err := admin.CreateToken(ctx, apitypes.NewToken{Name: "odd", Scopes: []string{"deploy"}, Envs: "staging"}); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("a limit that does not exist: %v", err)
	}
	made, err := admin.CreateToken(ctx, apitypes.NewToken{Name: "agent", Scopes: []string{"deploy"}, App: "shop", Envs: "previews"})
	if err != nil {
		t.Fatal(err)
	}
	if made.Envs != "previews" {
		t.Errorf("%+v", made)
	}
	agent, err := client.New("http://"+addrs.HTTP.String(), made.Secret, "")
	if err != nil {
		t.Fatal(err)
	}
	if me, err := agent.Me(ctx); err != nil || me.Envs != "previews" {
		t.Errorf("who it is: %+v %v", me, err)
	}

	// Production is out of reach, for anything with an environment in it.
	envPath := "/api/v1/apps/shop/envs/production/"
	for _, try := range []struct{ method, path, body string }{
		{"GET", envPath + "logs", ""},
		{"GET", envPath + "vars", ""},
		{"GET", envPath + "deployments", ""},
		{"GET", envPath + "files", ""},
		{"POST", envPath + "restart", ""},
		{"POST", envPath + "rollback", `{"version":1}`},
		{"POST", envPath + "runs", `{"command":"true"}`},
		{"PATCH", envPath + "vars", `{"set":[{"name":"X","value":"y"}]}`},
		{"POST", envPath + "deployments", ""},
	} {
		if code, body := api.do(made.Secret, try.method, try.path, try.body); code != http.StatusForbidden {
			t.Errorf("%s %s: %d %s", try.method, try.path, code, body)
		}
	}
	// Nor can it read what happened there, or do anything with no environment to be limited by.
	if list, err := admin.Deployments(ctx, "shop", "production", 1); err != nil {
		t.Fatal(err)
	} else if code, body := api.do(made.Secret, "GET", "/api/v1/deployments/"+list[0].ID, ""); code != http.StatusNotFound {
		t.Errorf("a deployment to production: %d %s", code, body)
	}
	for _, try := range []struct{ method, path, body string }{
		{"PATCH", "/api/v1/apps/shop", `{"name":"other"}`},
		{"DELETE", "/api/v1/apps/shop", ""},
		{"POST", "/api/v1/apps/shop/envs", `{"name":"staging"}`},
	} {
		if code, body := api.do(made.Secret, try.method, try.path, try.body); code != http.StatusForbidden {
			t.Errorf("%s %s: %d %s", try.method, try.path, code, body)
		}
	}

	// A preview that takes production's secrets is the same to it.
	if code, body := api.do(made.Secret, "GET", "/api/v1/apps/shop/envs/inherits-it/deployments", ""); code != http.StatusForbidden {
		t.Errorf("reading one that takes production's secrets: %d %s", code, body)
	}
	if code, body := api.do(made.Secret, "POST", "/api/v1/apps/shop/envs/inherits-it/restart", ""); code != http.StatusForbidden {
		t.Errorf("changing one that takes production's secrets: %d %s", code, body)
	}

	// Deploying to a new name makes a preview of it, with nothing of production's.
	files := appManifest("")
	dep, _, err := agent.DeploySource(ctx, "shop", "agent-work", writeSite(t, files), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "that deployment", func() bool {
		d, err := agent.Deployment(ctx, dep.ID)
		return err == nil && d.Finished()
	})
	if d, err := agent.Deployment(ctx, dep.ID); err != nil || d.Status != apitypes.StatusLive {
		t.Fatalf("%+v %v", d, err)
	}
	env, err := agent.Environment(ctx, "shop", "agent-work")
	if err != nil || env.Kind != "preview" {
		t.Errorf("what it made: %+v %v", env, err)
	}
	host := "shop--agent-work." + cfg.Domain
	if _, got := get(t, addrs.HTTP, host, "/env?name=API_KEY"); got == secretValue || strings.Contains(got, secretValue) {
		t.Errorf("it can read production's secret: %q", got)
	}
	if _, got := get(t, addrs.HTTP, host, "/env?name=PLAIN_SETTING"); strings.Contains(got, "plain-value") {
		t.Errorf("it took a variable of production's: %q", got)
	}
	// And a preview that a push made still takes them, as it did.
	if _, got := get(t, addrs.HTTP, "shop--inherits-it."+cfg.Domain, "/env?name=PLAIN_SETTING"); !strings.Contains(got, "plain-value") {
		t.Errorf("a preview that a push made: %q", got)
	}
	// It can go on working there.
	if code, body := api.do(made.Secret, "POST", "/api/v1/apps/shop/envs/agent-work/restart", ""); code != http.StatusOK {
		t.Errorf("restarting its own: %d %s", code, body)
	}

	// It cannot push, which could go to any branch.
	req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/git/shop.git/info/refs?service=git-receive-pack", nil)
	req.Host = cfg.ConsoleHost
	req.SetBasicAuth("moor", made.Secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("pushing: %d", res.StatusCode)
	}
}
