// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
)

// caller makes requests as a token, to say exactly what comes back.
type caller struct {
	t     *testing.T
	cfg   config.Config
	addrs Addrs
}

func (c caller) do(secret, method, path, body string) (int, string) {
	c.t.Helper()
	req, err := http.NewRequest(method, "http://"+c.addrs.HTTP.String()+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Host = c.cfg.ConsoleHost
	req.Header.Set("Authorization", "Bearer "+secret)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

func (c caller) read(secret, path string) string {
	c.t.Helper()
	code, body := c.do(secret, http.MethodGet, "/api/v1"+path, "")
	if code != http.StatusOK {
		c.t.Errorf("%s: %d %s", path, code, body)
	}
	return body
}

func scopedToken(t *testing.T, c *client.Client, scope string) string {
	t.Helper()
	made, err := c.CreateToken(context.Background(), apitypes.NewToken{Name: scope + " for this test", Scopes: []string{scope}})
	if err != nil {
		t.Fatal(err)
	}
	return made.Secret
}

var none = "none"

// aProject makes a project that no one has a role in without being given one.
func aProject(t *testing.T, c *client.Client, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.CreateProject(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ChangeProject(ctx, name, apitypes.ProjectChange{Everyone: &none}); err != nil {
		t.Fatal(err)
	}
}

func anAppIn(t *testing.T, c *client.Client, project, app string) {
	t.Helper()
	if _, err := c.CreateApp(context.Background(), app, project); err != nil {
		t.Fatal(err)
	}
}

// TestWhatIsNotThereAndWhatCannotBeSeen looks for any way of telling an app or a project that someone has no role in from one that does not
// exist, and for any name of one coming back.
func TestWhatIsNotThereAndWhatCannotBeSeen(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	owner := signIn(t, cfg, addrs)
	admin := apiClient(t, cfg, addrs)
	api := caller{t, cfg, addrs}
	aProject(t, admin, "closed")
	aProject(t, admin, "open")
	for _, app := range []string{"aahidden", "mmhidden", "zzhidden"} {
		anAppIn(t, admin, "closed", app)
	}
	anAppIn(t, admin, "open", "pubapp")
	anAppIn(t, admin, "open", "pubother")
	ctx := context.Background()

	// Everyone is a maintainer of the default project, which is where a new app would go: so that what is not there and what
	// is hidden start from different places.
	member := addMember(t, admin, addrs, "member@example.com")
	if _, err := admin.SetMember(ctx, "open", "member@example.com", "maintainer"); err != nil {
		t.Fatal(err)
	}
	read := scopedToken(t, member, "read")
	all := scopedToken(t, member, "admin")

	same := func(what, hidden, missing string, code1 int, body1 string, code2 int, body2 string) {
		t.Helper()
		one := strings.ReplaceAll(body1, hidden, "NAME")
		two := strings.ReplaceAll(body2, missing, "NAME")
		if code1 != code2 || one != two {
			t.Errorf("%s can be told apart: %d %q, and %d %q", what, code1, body1, code2, body2)
		}
	}

	// Every route that reads an app.
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`\{[a-z]+\}`)
	count := 0
	for path, methods := range doc.Paths {
		if _, ok := methods["get"]; !ok || !strings.Contains(path, "{app}") {
			continue
		}
		count++
		fill := func(app string) string {
			p := strings.ReplaceAll(path, "{app}", app)
			p = strings.ReplaceAll(p, "{env}", "production")
			return "/api/v1" + param.ReplaceAllString(p, "1")
		}
		code1, body1 := api.do(all, http.MethodGet, fill("aahidden"), "")
		code2, body2 := api.do(all, http.MethodGet, fill("nosuchapp"), "")
		if code1 != http.StatusNotFound {
			t.Errorf("%s of a hidden app: %d %s", path, code1, body1)
		}
		same(path, "aahidden", "nosuchapp", code1, body1, code2, body2)
	}
	if count < 20 {
		t.Errorf("only %d routes", count)
	}

	// A project: reading it, putting an app in it, moving one there.
	code1, body1 := api.do(read, http.MethodGet, "/api/v1/projects/closed", "")
	code2, body2 := api.do(read, http.MethodGet, "/api/v1/projects/nosuchproject", "")
	same("a project", "closed", "nosuchproject", code1, body1, code2, body2)
	code1, body1 = api.do(all, http.MethodPost, "/api/v1/apps", `{"name":"new-app","project":"closed"}`)
	code2, body2 = api.do(all, http.MethodPost, "/api/v1/apps", `{"name":"new-app","project":"nosuchproject"}`)
	same("making an app in a project", "closed", "nosuchproject", code1, body1, code2, body2)
	code1, body1 = api.do(all, http.MethodPatch, "/api/v1/apps/pubapp", `{"project":"closed"}`)
	code2, body2 = api.do(all, http.MethodPatch, "/api/v1/apps/pubapp", `{"project":"nosuchproject"}`)
	same("moving an app to a project", "closed", "nosuchproject", code1, body1, code2, body2)
	if code1 != http.StatusNotFound {
		t.Errorf("moving an app to a hidden project: %d %s", code1, body1)
	}

	// Git.
	gitRead := func(app string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/git/"+app+".git/info/refs?service=git-upload-pack", nil)
		req.Host = cfg.ConsoleHost
		req.SetBasicAuth("moor", read)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(out)
	}
	code1, body1 = gitRead("aahidden")
	code2, body2 = gitRead("nosuchapp")
	same("a repository", "aahidden", "nosuchapp", code1, body1, code2, body2)
	if code1 != http.StatusNotFound {
		t.Errorf("a hidden app's repository: %d %s", code1, body1)
	}

	// Going through the list, a cursor says nothing that the list does not, and no page comes up short.
	cursor, seen := "", []string{}
	for {
		var page apitypes.Page[apitypes.App]
		if err := json.Unmarshal([]byte(api.read(read, "/apps?limit=1&cursor="+url.QueryEscape(cursor))), &page); err != nil {
			t.Fatal(err)
		}
		for _, a := range page.Items {
			seen = append(seen, a.Name)
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
		if len(page.Items) != 1 {
			t.Errorf("a page before the last has %d", len(page.Items))
		}
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(decoded), "hidden") {
			t.Errorf("a cursor names %s", decoded)
		}
	}
	if strings.Join(seen, " ") != "pubapp pubother" {
		t.Errorf("the list: %v", seen)
	}

	// What the server itself does is for its administrators.
	admins := scopedToken(t, admin, "read")
	waitUntil(t, "an event about no app", func() bool { return strings.Contains(api.read(admins, "/events?limit=200"), "server.started") })
	if events := api.read(read, "/events?limit=200"); strings.Contains(events, "server.started") {
		t.Errorf("someone who is no administrator reads what the server does: %s", events)
	}

	// The console says the same of a project.
	ana := addTeammate(t, cfg, addrs, owner, "ana@example.com")
	res1, page1 := ana.post("/new", url.Values{"csrf": {ana.token()}, "name": {"new-app"}, "project": {"closed"}})
	res2, page2 := ana.post("/new", url.Values{"csrf": {ana.token()}, "name": {"new-app"}, "project": {"nosuchproject"}})
	if res1.StatusCode != res2.StatusCode || !strings.Contains(page1, "There is no such project") || !strings.Contains(page2, "There is no such project") {
		t.Errorf("making an app in the console: %d and %d", res1.StatusCode, res2.StatusCode)
	}
}

// TestWhatAViewerReads keeps to what the table of roles says: a viewer sees an app and how it stands. What it is
// configured with is for those who can deploy it, and logs may be kept from viewers, in whatever form they come.
func TestWhatAViewerReads(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	owner := signIn(t, cfg, addrs)
	admin := apiClient(t, cfg, addrs)
	api := caller{t, cfg, addrs}
	ctx := context.Background()
	aProject(t, admin, "open")
	aProject(t, admin, "closed")
	anAppIn(t, admin, "open", "pubapp")
	keepLogs := false
	if _, err := admin.ChangeProject(ctx, "open", apitypes.ProjectChange{ViewersSeeLogs: &keepLogs}); err != nil {
		t.Fatal(err)
	}
	viewer := addMember(t, admin, addrs, "viewer@example.com")
	deployer := addMember(t, admin, addrs, "deployer@example.com")
	for email, role := range map[string]string{"viewer@example.com": "viewer", "deployer@example.com": "deployer"} {
		if _, err := admin.SetMember(ctx, "open", email, role); err != nil {
			t.Fatal(err)
		}
	}
	reads, deploys := scopedToken(t, viewer, "read"), scopedToken(t, deployer, "deploy")

	// A release with a value in its manifest, then one with a variable.
	const inManifest, variable = "manifest-plain-value", "VISIBLE_TO_DEPLOYERS"
	const scheduled = "private-cron-words"
	files := appManifest("\n[[cron]]\nname = \"nightly\"\nschedule = \"0 3 * * *\"\ncommand = \"" + scheduled + "\"\n")
	files["moorage.toml"] = strings.Replace(files["moorage.toml"], `TESTAPP_BODY = "here"`, `TESTAPP_BODY = "`+inManifest+`"`, 1)
	first := deployedSource(t, admin, "pubapp", files)
	if _, err := admin.ChangeVars(ctx, "pubapp", "production", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: variable, Value: "its-value"}}}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the second release", func() bool {
		code, _ := api.do(deploys, http.MethodGet, "/api/v1/apps/pubapp/envs/production/releases/2", "")
		return code == http.StatusOK
	})

	for _, path := range []string{"/apps/pubapp/envs/production/releases/1", "/apps/pubapp/envs/production/releases/1/diff/2", "/events?app=pubapp&limit=200"} {
		if got := api.read(deploys, path); !strings.Contains(got, inManifest) && !strings.Contains(got, variable) {
			t.Errorf("%s says nothing to a deployer: %s", path, got)
		}
		if got := api.read(reads, path); strings.Contains(got, inManifest) || strings.Contains(got, variable) {
			t.Errorf("%s, to a viewer: %s", path, got)
		}
	}

	// What a failed deployment said is a log.
	failed, _ := deploySource(t, admin, "pubapp", map[string]string{"moorage.toml": sourceManifest("broken", "echo built", "echo PRIVATE-LINE && exit 4")})
	if failed.Status != apitypes.StatusFailed {
		t.Fatalf("that should have failed: %+v", failed)
	}
	for _, path := range []string{"/deployments/" + failed.ID, "/apps/pubapp/envs/production/deployments"} {
		if got := api.read(deploys, path); !strings.Contains(got, "PRIVATE-LINE") {
			t.Errorf("%s, to a deployer: %s", path, got)
		}
		if got := api.read(reads, path); strings.Contains(got, "PRIVATE-LINE") {
			t.Errorf("%s, to a viewer kept from logs: %s", path, got)
		}
	}

	// So is what someone typed.
	const typed = "private-command-words"
	run, err := admin.StartRun(ctx, "pubapp", "production", apitypes.NewRun{Command: typed})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/apps/pubapp/envs/production/runs", "/apps/pubapp/runs/" + run.ID} {
		if got := api.read(deploys, path); !strings.Contains(got, typed) {
			t.Errorf("%s, to a deployer: %s", path, got)
		}
		if got := api.read(reads, path); strings.Contains(got, typed) {
			t.Errorf("%s, to a viewer kept from logs: %s", path, got)
		}
	}
	if got := api.read(deploys, "/apps/pubapp/envs/production/cron"); !strings.Contains(got, scheduled) {
		t.Errorf("the cron jobs, to a deployer: %s", got)
	}
	if got := api.read(reads, "/apps/pubapp/envs/production/cron"); strings.Contains(got, scheduled) || !strings.Contains(got, "nightly") {
		t.Errorf("the cron jobs, to a viewer kept from logs: %s", got)
	}

	// The console keeps to the same.
	ana := addTeammate(t, cfg, addrs, owner, "ana@example.com")
	if _, err := admin.SetMember(ctx, "open", "ana@example.com", "viewer"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/deployments/" + first.ID, "/deployments/" + failed.ID, "/apps/pubapp", "/activity", "/apps/pubapp/cron", "/apps/pubapp/runs/" + run.ID} {
		res, page := ana.get(path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
		for _, private := range []string{inManifest, variable, "PRIVATE-LINE", typed, scheduled} {
			if strings.Contains(page, private) {
				t.Errorf("%s shows a viewer %s", path, private)
			}
		}
	}

	// An app's history does not go to whoever takes its name.
	anAppIn(t, admin, "closed", "ghost")
	deploySite(t, admin, "ghost")
	if _, err := admin.ChangeVars(ctx, "ghost", "production", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "GHOST_VARIABLE", Value: "boo"}}}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "that being said", func() bool {
		return strings.Contains(api.read(scopedToken(t, admin, "admin"), "/events?app=ghost&limit=200"), "GHOST_VARIABLE")
	})
	if err := admin.DeleteApp(ctx, "ghost", false); err != nil {
		t.Fatal(err)
	}
	anAppIn(t, admin, "open", "ghost")
	if got := api.read(deploys, "/events?app=ghost&limit=200"); strings.Contains(got, "GHOST_VARIABLE") {
		t.Errorf("the new app's events: %s", got)
	}
}

// TestAFollowedLogEndsWithTheRole: a deployment's log has no app in its address, and it still ends when someone may no longer read it.
func TestAFollowedLogEndsWithTheRole(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	admin := apiClient(t, cfg, addrs)
	ctx := context.Background()
	aProject(t, admin, "open")
	anAppIn(t, admin, "open", "slowapp")
	viewer := addMember(t, admin, addrs, "viewer@example.com")
	if _, err := admin.SetMember(ctx, "open", "viewer@example.com", "viewer"); err != nil {
		t.Fatal(err)
	}
	reads := scopedToken(t, viewer, "read")

	// It waits for a process that takes longer to start than this test does.
	files := appManifest("")
	files["moorage.toml"] = strings.Replace(files["moorage.toml"], `TESTAPP_BODY = "here"`, "TESTAPP_BODY = \"here\"\nTESTAPP_START_DELAY = \"120s\"", 1)
	queued, _, err := admin.DeploySource(ctx, "slowapp", "production", writeSite(t, files), nil)
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/api/v1/deployments/"+queued.ID+"/log?follow=true", nil)
	req.Host = cfg.ConsoleHost
	req.Header.Set("Authorization", "Bearer "+reads)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("following it: %d", res.StatusCode)
	}
	lines := bufio.NewReader(res.Body)
	if _, err := lines.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for {
			if _, err := lines.ReadString('\n'); err != nil {
				return
			}
		}
	}()
	if _, err := admin.RemoveMember(ctx, "open", "viewer@example.com"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
		if d, err := admin.Deployment(ctx, queued.ID); err != nil || d.Status == apitypes.StatusLive || d.Status == apitypes.StatusFailed {
			t.Errorf("the deployment had ended, so that this says nothing: %+v %v", d, err)
		}
	case <-time.After(20 * time.Second):
		t.Error("it is still open, for someone with no role")
	}
}
