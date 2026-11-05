// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/archive"
	"github.com/robert-mcdermott/moorage/internal/client"
)

// limitedToken makes a deploy token limited to previews, for one app.
func limitedToken(t *testing.T, admin *client.Client, addrs Addrs, app string) (string, *client.Client) {
	t.Helper()
	made, err := admin.CreateToken(context.Background(), apitypes.NewToken{Name: "agent", Scopes: []string{"deploy"}, App: app, Envs: "previews"})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := client.New("http://"+addrs.HTTP.String(), made.Secret, "")
	if err != nil {
		t.Fatal(err)
	}
	return made.Secret, agent
}

// deployFrom uploads a directory as someone, and waits for it to end.
func deployFrom(t *testing.T, c *client.Client, app, env string, files map[string]string) apitypes.Deployment {
	t.Helper()
	ctx := context.Background()
	dep, _, err := c.DeploySource(ctx, app, env, writeSite(t, files), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got apitypes.Deployment
	waitUntil(t, "the deployment to "+env, func() bool {
		got, err = c.Deployment(ctx, dep.ID)
		return err == nil && got.Finished()
	})
	return got
}

// TestALimitedTokenSeesOnlyWhatItMade goes through what a token limited to previews should not reach of production and
// of the app's other environments, by every way that has reached them.
func TestALimitedTokenSeesOnlyWhatItMade(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	admin := apiClient(t, cfg, addrs)
	api := caller{t, cfg, addrs}
	ctx := context.Background()
	deployedSource(t, admin, "shop", appManifest(""))
	if _, err := admin.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{AllEnvs: true, Set: []apitypes.VarSet{
		{Name: "SHARED_SECRET", Value: "app-wide-secret-value", Secret: true},
		{Name: "SHARED_PLAIN", Value: "app-wide-plain-value"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.CreateEnvironment(ctx, "shop", apitypes.NewEnvironment{Name: "named"}); err != nil {
		t.Fatal(err)
	}
	run, err := admin.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "echo production-run-output"})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the run", func() bool {
		r, err := admin.Run(ctx, "shop", run.ID)
		return err == nil && r.Status != "running" && r.Status != "queued" && r.Status != "starting"
	})
	secret, agent := limitedToken(t, admin, addrs, "shop")

	// Its own, on a name that nothing will want.
	if d := deployFrom(t, agent, "shop", "agent-work", appManifest("")); d.Status != apitypes.StatusLive {
		t.Fatalf("its own preview: %+v", d)
	}
	host := "shop--agent-work." + cfg.Domain

	for _, try := range []struct{ method, path, body string }{
		// Names that are free, on which what it could do once made environments and promoted things.
		{"POST", "/api/v1/apps/shop/envs/fresh/promote", `{"from":"production"}`},
		{"POST", "/api/v1/apps/shop/envs/fresh/lock", ""},
		{"POST", "/api/v1/apps/shop/envs/fresh/rollback", `{"version":1}`},
		{"POST", "/api/v1/apps/shop/envs/fresh/redeploy", ""},
		{"POST", "/api/v1/apps/shop/envs/fresh/sleep", ""},
		{"POST", "/api/v1/apps/shop/envs/fresh/restart", ""},
		{"POST", "/api/v1/apps/shop/envs/fresh/runs", `{"command":"true"}`},
		{"POST", "/api/v1/apps/shop/envs/fresh/share-links", `{"name":"x"}`},
		{"PUT", "/api/v1/apps/shop/envs/fresh/files/content?path=planted.txt", "hello"},
		{"POST", "/api/v1/apps/shop/envs/fresh/files/directory?path=planted", ""},
		{"DELETE", "/api/v1/apps/shop/envs/fresh", ""},
		// An agent- name is only for deploying to.
		{"POST", "/api/v1/apps/shop/envs/agent-fresh/runs", `{"command":"true"}`},
		{"GET", "/api/v1/apps/shop/envs/agent-fresh", ""},
		// Deploying to a name that a manifest may come to want.
		{"POST", "/api/v1/apps/shop/envs/staging/deployments", ""},
		// Another environment, for reading as well.
		{"GET", "/api/v1/apps/shop/envs/named", ""},
		{"GET", "/api/v1/apps/shop/envs/named/logs", ""},
		{"GET", "/api/v1/apps/shop/envs/named/vars", ""},
		{"POST", "/api/v1/apps/shop/envs/named/restart", ""},
		{"POST", "/api/v1/apps/shop/envs/named/promote", `{"from":"production"}`},
		// Its own, for what could reach beyond it.
		{"POST", "/api/v1/apps/shop/envs/agent-work/promote", `{"from":"production"}`},
		{"PATCH", "/api/v1/apps/shop/envs/agent-work/vars", `{"all_envs":true,"set":[{"name":"X","value":"y"}]}`},
		{"PATCH", "/api/v1/apps/shop/envs/agent-work", `{"pinned":true}`},
		{"POST", "/api/v1/apps/shop/envs/agent-work/lock", ""},
		{"POST", "/api/v1/apps/shop/envs/agent-work/share-links", `{"name":"x"}`},
		// What has no environment to be limited by.
		{"GET", "/api/v1/events?app=shop&env=production", ""},
		{"GET", "/api/v1/notifications", ""},
		{"GET", "/api/v1/previews", ""},
		{"POST", "/api/v1/apps/shop/previews", `{"branch":"main"}`},
		{"GET", "/api/v1/apps/shop/forge", ""},
		{"GET", "/api/v1/me/tokens", ""},
		{"POST", "/api/v1/me/tokens", `{"name":"more","scopes":["deploy"]}`},
		{"GET", "/api/v1/projects", ""},
	} {
		if code, body := api.do(secret, try.method, try.path, try.body); code != http.StatusForbidden {
			t.Errorf("%s %s: %d %.200s", try.method, try.path, code, body)
		}
	}
	// None of that made anything.
	if list, err := admin.Environments(ctx, "shop"); err != nil || len(list) != 3 {
		t.Errorf("after all that: %+v %v", list, err)
	}

	// A run of production's, by its id.
	for _, try := range []struct{ method, path string }{
		{"GET", "/api/v1/apps/shop/runs/" + run.ID},
		{"GET", "/api/v1/apps/shop/runs/" + run.ID + "/log"},
		{"POST", "/api/v1/apps/shop/runs/" + run.ID + "/stop"},
	} {
		if code, body := api.do(secret, try.method, try.path, ""); code != http.StatusNotFound {
			t.Errorf("%s %s: %d %.200s", try.method, try.path, code, body)
		}
	}
	// What is listed keeps to its own.
	code, body := api.do(secret, "GET", "/api/v1/apps", "")
	if code != http.StatusOK || !strings.Contains(body, `"shop"`) || strings.Contains(body, `"production"`) {
		t.Errorf("the apps: %d %s", code, body)
	}
	if code, body := api.do(secret, "GET", "/api/v1/apps/shop", ""); code != http.StatusOK || !strings.Contains(body, `"builds":0`) {
		t.Errorf("the app: %d %s", code, body)
	}
	list, err := agent.Environments(ctx, "shop")
	if err != nil || len(list) != 1 || list[0].Name != "agent-work" || !list[0].StandsAlone {
		t.Errorf("its environments: %+v %v", list, err)
	}

	// The variables that are for all environments are not for this one, to see or to run with.
	vars, err := agent.Vars(ctx, "shop", "agent-work")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vars {
		if strings.HasPrefix(v.Name, "SHARED_") {
			t.Errorf("its variables: %+v", vars)
		}
	}
	for _, name := range []string{"SHARED_SECRET", "SHARED_PLAIN"} {
		if _, got := get(t, addrs.HTTP, host, "/env?name="+name); strings.Contains(got, "app-wide") {
			t.Errorf("it runs with %s: %q", name, got)
		}
	}

	// Its logs of builds are of its own, and those of what ran in production are kept beside them.
	code, body = api.do(secret, "GET", "/api/v1/apps/shop/envs/agent-work/logs?source=build", "")
	if code != http.StatusOK || strings.Contains(body, "production-run-output") {
		t.Errorf("its build logs: %d %.300s", code, body)
	}
	adminSecret, err := admin.CreateToken(ctx, apitypes.NewToken{Name: "reader", Scopes: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := api.do(adminSecret.Secret, "GET", "/api/v1/apps/shop/envs/production/logs?source=build", ""); code != http.StatusOK || !strings.Contains(body, "production-run-output") {
		t.Errorf("production's build logs: %d %.300s", code, body)
	}

	// Neither what a person would clone, nor what a scraper reads.
	req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/git/shop.git/info/refs?service=git-upload-pack", nil)
	req.Host = cfg.ConsoleHost
	req.SetBasicAuth("moor", secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("reading the repository: %d", res.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/metrics", nil)
	req.Host = cfg.ConsoleHost
	req.Header.Set("Authorization", "Bearer "+secret)
	if res, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(metrics), `env="production"`) || strings.Contains(string(metrics), `env="named"`) {
		t.Errorf("metrics: %.400s", metrics)
	}

	// It can go on working in its own, reading and acting. Reading its own deployment is covered above.
	for _, try := range []struct{ method, path, body string }{
		{"GET", "/api/v1/me", ""},
		{"GET", "/api/v1/server", ""},
		{"GET", "/api/v1/apps/shop/envs/agent-work", ""},
		{"GET", "/api/v1/apps/shop/envs/agent-work/deployments", ""},
		{"GET", "/api/v1/apps/shop/envs/agent-work/releases", ""},
		{"GET", "/api/v1/apps/shop/envs/agent-work/instances", ""},
		{"GET", "/api/v1/apps/shop/envs/agent-work/logs", ""},
		{"POST", "/api/v1/apps/shop/envs/agent-work/restart", ""},
	} {
		if code, body := api.do(secret, try.method, try.path, try.body); code != http.StatusOK {
			t.Errorf("%s %s: %d %.200s", try.method, try.path, code, body)
		}
	}
	// And the environment says what it is, for a person looking at it.
	if doc, err := admin.Environment(ctx, "shop", "agent-work"); err != nil || !doc.StandsAlone {
		t.Errorf("%+v %v", doc, err)
	}
	if doc, err := admin.Environment(ctx, "shop", "production"); err != nil || doc.StandsAlone {
		t.Errorf("%+v %v", doc, err)
	}
	b := signIn(t, cfg, addrs)
	_, page := b.get("/apps/shop?env=agent-work")
	contains(t, "the page of a preview that stands alone", page, "nothing of production")
}

// TestALimitedTokenDoesNotSayWhatIsThere keeps to not found for what its owner cannot see, as a token with no limit does.
func TestALimitedTokenDoesNotSayWhatIsThere(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	admin := apiClient(t, cfg, addrs)
	api := caller{t, cfg, addrs}
	aProject(t, admin, "private")
	anAppIn(t, admin, "private", "hidden")
	c := addMember(t, admin, addrs, "pat@example.com")
	made, err := c.CreateToken(context.Background(), apitypes.NewToken{Name: "agent", Scopes: []string{"deploy"}, Envs: "previews"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/apps/hidden/envs/production", "/api/v1/apps/hidden/envs/agent-work", "/api/v1/apps/none/envs/production", "/api/v1/apps/hidden/forge"} {
		if code, body := api.do(made.Secret, "GET", path, ""); code != http.StatusNotFound {
			t.Errorf("GET %s: %d %.200s", path, code, body)
		}
	}
}

// TestWhatAPreviewOfALimitedTokenCannotChange: what such a preview's own manifest says, or its name, should not reach
// anything else of the app.
func TestWhatAPreviewOfALimitedTokenCannotChange(t *testing.T) {
	cfg := testConfig(t)
	addrs := startWith(t, cfg, func(o *Options) { o.GCMinAge = time.Millisecond })
	admin := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, admin, "shop", appManifest(""))
	deployedSource(t, admin, "shop", appManifest("# second\n"))
	deployedSource(t, admin, "shop", appManifest("# third\n"))
	_, agent := limitedToken(t, admin, addrs, "shop")

	// Its manifest is not to throw away production's way back, nor keep its preview for ever.
	files := appManifest("keep_releases = 1\n\n[previews]\nttl = \"8760h\"\n")
	if d := deployFrom(t, agent, "shop", "agent-work", files); d.Status != apitypes.StatusLive {
		t.Fatalf("%+v", d)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := admin.Rollback(ctx, "shop", "production", 1); err != nil {
		t.Errorf("rolling production back, after that: %v", err)
	}
	e, err := admin.Environment(ctx, "shop", "agent-work")
	if err != nil {
		t.Fatal(err)
	}
	if e.ExpiresAt.IsZero() || time.Until(e.ExpiresAt) > 30*24*time.Hour {
		t.Errorf("it expires at %v", e.ExpiresAt)
	}

	// Its name is not to become something with data and access of its own.
	declared := appManifest("\n[environments.agent-work]\nbranch = \"main\"\n\n[environments.staging]\nbranch = \"staging\"\n")
	deployedSource(t, admin, "shop", declared)
	if e, err := admin.Environment(ctx, "shop", "agent-work"); err != nil || e.Kind != "preview" || e.Branch == "main" {
		t.Errorf("the name it took: %+v %v", e, err)
	}
	if e, err := admin.Environment(ctx, "shop", "staging"); err != nil || e.Kind != "named" {
		t.Errorf("the name it did not: %+v %v", e, err)
	}
	deps, err := admin.Deployments(ctx, "shop", "production", 1)
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(filepath.Join(cfg.DataDir, "logs", "deployments", "shop", deps[0].ID+".log"))
	if err != nil || !strings.Contains(string(log), "the environment agent-work is not made") {
		t.Errorf("the deployment does not say so: %v %s", err, log)
	}
}

// TestALimitedTokenMakesOnlySoManyPreviews holds it to what a branch's previews are held to.
func TestALimitedTokenMakesOnlySoManyPreviews(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	admin := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, admin, "shop", appManifest(""))
	_, agent := limitedToken(t, admin, addrs, "shop")
	site := map[string]string{"index.html": "<p>hello</p>\n", "moorage.toml": "[app]\nruntime = \"static\"\n"}
	var refused error
	made := 0
	for i := 0; i < 12 && refused == nil; i++ {
		_, _, refused = agent.DeploySource(ctx, "shop", fmt.Sprintf("agent-%d", i), writeSite(t, site), nil)
		if refused == nil {
			made++
		}
	}
	if made != 10 || status(refused) != http.StatusConflict {
		t.Errorf("%d previews, then %v", made, refused)
	}
	// One that exists can still be deployed to.
	if _, _, err := agent.DeploySource(ctx, "shop", "agent-0", writeSite(t, site), nil); err != nil {
		t.Errorf("deploying again: %v", err)
	}
	// It does not make an app, or anything else, by deploying.
	if _, _, err := agent.DeploySource(ctx, "none", "agent-new", writeSite(t, site), nil); err == nil {
		t.Error("deploying to an app that it was not limited to")
	}
	if _, err := admin.Environments(ctx, "none"); status(err) != http.StatusNotFound {
		t.Errorf("that app: %v", err)
	}
}

// TestAnUploadThatTakesItsTimeMakesOnlyAPreview: an upload can take as long as its sender likes, and what it may make is decided when it finishes. Deleting the preview
// while it was under way used to leave a named environment, with the variables of every environment and no end to it.
func TestAnUploadThatTakesItsTimeMakesOnlyAPreview(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	admin := apiClient(t, cfg, addrs)
	api := caller{t, cfg, addrs}
	ctx := context.Background()
	deployedSource(t, admin, "shop", appManifest(""))
	if _, err := admin.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{AllEnvs: true, Set: []apitypes.VarSet{{Name: "SHARED_PLAIN", Value: "app-wide-plain-value"}}}); err != nil {
		t.Fatal(err)
	}
	secret, agent := limitedToken(t, admin, addrs, "shop")
	site := map[string]string{"index.html": "<p>hello</p>\n", "moorage.toml": "[app]\nruntime = \"static\"\n"}
	if d := deployFrom(t, agent, "shop", "agent-slow", site); d.Status != apitypes.StatusLive {
		t.Fatalf("%+v", d)
	}

	// An upload, of which the last of it is held back.
	var packed bytes.Buffer
	body := multipart.NewWriter(&packed)
	part, err := body.CreateFormField(apitypes.SourcePart)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := archive.Write(part, writeSite(t, site), nil); err != nil {
		t.Fatal(err)
	}
	body.Close()
	reader, writer := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, "http://"+addrs.HTTP.String()+"/api/v1/apps/shop/envs/agent-slow/deployments", reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = cfg.ConsoleHost
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", body.FormDataContentType())
	answered := make(chan *http.Response, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			answered <- nil
			return
		}
		answered <- res
	}()
	data := packed.Bytes()
	if _, err := writer.Write(data[:len(data)/2]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// Under its feet, the preview goes.
	if code, got := api.do(secret, http.MethodDelete, "/api/v1/apps/shop/envs/agent-slow", ""); code/100 != 2 {
		t.Fatalf("deleting it: %d %s", code, got)
	}
	if _, err := writer.Write(data[len(data)/2:]); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	res := <-answered
	if res == nil {
		return
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("the upload: %d", res.StatusCode)
	}
	e, err := admin.Environment(ctx, "shop", "agent-slow")
	if err != nil || e.Kind != "preview" || !e.StandsAlone || e.ExpiresAt.IsZero() {
		t.Errorf("what it made: %+v %v", e, err)
	}

	// Nor can it deploy to what has become something else while it was at it.
	if _, err := admin.CreateEnvironment(ctx, "shop", apitypes.NewEnvironment{Name: "agent-named"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.DeploySource(ctx, "shop", "agent-named", writeSite(t, site), nil); status(err) != http.StatusForbidden {
		t.Errorf("deploying to a named one: %v", err)
	}
}

// TestAHostIsNotALimitedTokensToRead: what a server's own numbers say is for those who look after it, and a token limited to previews is not, whoever made it.
func TestAHostIsNotALimitedTokensToRead(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	admin := apiClient(t, cfg, addrs)
	ctx := context.Background()
	made, err := admin.CreateToken(ctx, apitypes.NewToken{Name: "agent", Scopes: []string{"deploy"}, Envs: "previews"})
	if err != nil {
		t.Fatal(err)
	}
	full, err := admin.CreateToken(ctx, apitypes.NewToken{Name: "full", Scopes: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	read := func(secret string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/metrics", nil)
		req.Host = cfg.ConsoleHost
		req.Header.Set("Authorization", "Bearer "+secret)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return string(body)
	}
	waitUntil(t, "the server's numbers", func() bool { return strings.Contains(read(full.Secret), "moorage_host_") })
	if got := read(made.Secret); strings.Contains(got, "moorage_host_") {
		t.Errorf("%.300s", got)
	}
}
