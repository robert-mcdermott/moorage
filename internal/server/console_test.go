package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
)

// consoleUser signs in to a server's console, with what a browser sends: the console's
// host, the session's cookie and the form's token.
type consoleUser struct {
	t       *testing.T
	addr    net.Addr
	host    string
	cookies []*http.Cookie
}

func (b *consoleUser) do(req *http.Request) (*http.Response, string) {
	b.t.Helper()
	req.Host = b.host
	for _, cookie := range b.cookies {
		req.AddCookie(cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if cookies := res.Cookies(); len(cookies) > 0 {
		b.cookies = cookies
	}
	return res, string(body)
}

func (b *consoleUser) get(path string) (*http.Response, string) {
	b.t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+b.addr.String()+path, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	return b.do(req)
}

func (b *consoleUser) post(path string, form url.Values) (*http.Response, string) {
	b.t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+b.addr.String()+path, strings.NewReader(form.Encode()))
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.do(req)
}

var formToken = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// token is the session's token for forms, as a page has it.
func (b *consoleUser) token() string {
	b.t.Helper()
	_, body := b.get("/")
	m := formToken.FindStringSubmatch(body)
	if m == nil {
		b.t.Fatalf("no token in: %s", body)
	}
	return m[1]
}

// signIn claims the server, as its first account.
func signIn(t *testing.T, cfg config.Config, addrs Addrs) *consoleUser {
	t.Helper()
	code, err := os.ReadFile(NewLayout(cfg.DataDir).ClaimCode)
	if err != nil {
		t.Fatal(err)
	}
	b := &consoleUser{t: t, addr: addrs.HTTP, host: cfg.ConsoleHost}
	passphrase := "a long and unusual passphrase"
	res, body := b.post("/setup", url.Values{
		"code": {strings.TrimSpace(string(code))}, "email": {"owner@example.com"}, "password": {passphrase}, "confirm": {passphrase},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("claiming the server: %d %s", res.StatusCode, body)
	}
	return b
}

func contains(t *testing.T, what, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("%s has no %q:\n%s", what, want, body)
		}
	}
}

func oneOf(pids []apitypes.Instance) map[int]bool {
	out := map[int]bool{}
	for _, in := range pids {
		out[in.PID] = true
	}
	return out
}

// TestConsoleDrivesAnApp does from the console's pages what they offer, to an app with
// real instances.
func TestConsoleDrivesAnApp(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()

	if _, err := deployApp(t, c, "shop", appPlan(nil)); err != nil {
		t.Fatal(err)
	}
	_, list := b.get("/")
	contains(t, "the apps", list, `data-app-row="shop"`, `href="/apps/shop"`, "Healthy", "1 of 1", "v1")
	res, page := b.get("/apps/shop")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, page)
	}
	contains(t, "the app", page, "web.1", "Healthy", `name="process.web"`, "/apps/shop/restart", "/apps/shop/redeploy", `data-refresh-on="shop"`)
	token := b.token()

	// Restarting: new instances, and it says so.
	before := oneOf(instances(t, c, "shop"))
	res, body := b.post("/apps/shop/restart", url.Values{"csrf": {token}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/apps/shop?done=restarted" {
		t.Fatalf("restart: %d %s %s", res.StatusCode, res.Header.Get("Location"), body)
	}
	for _, in := range instances(t, c, "shop") {
		if before[in.PID] {
			t.Errorf("the same process: %+v", in)
		}
	}
	_, page = b.get("/apps/shop?done=restarted")
	contains(t, "the app, restarted", page, "Its instances were replaced.", "Restarted by owner@example.com")

	// Scaling up.
	res, body = b.post("/apps/shop/scale", url.Values{"csrf": {token}, "process.web": {"2"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/apps/shop?done=scaled" {
		t.Fatalf("scale: %d %s %s", res.StatusCode, res.Header.Get("Location"), body)
	}
	if n := len(instances(t, c, "shop")); n != 2 {
		t.Errorf("%d instances", n)
	}
	state, err := c.Environment(ctx, "shop", "production")
	if err != nil || state.Formation["web"] != 2 || state.Release == nil || state.Release.Version != 1 {
		t.Errorf("%+v %v", state, err)
	}
	_, page = b.get("/apps/shop")
	contains(t, "the app, scaled", page, "web.2", "the manifest says 1", `value="2"`, "Scaled to web ×2 by owner@example.com")

	// The same number changes nothing; what cannot be is refused, and says why.
	before = oneOf(instances(t, c, "shop"))
	if res, _ = b.post("/apps/shop/scale", url.Values{"csrf": {token}, "process.web": {"2"}}); res.Header.Get("Location") != "/apps/shop?done=unchanged" {
		t.Errorf("the same number: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	for _, tc := range []struct{ field, value, want string }{
		{"process.nothing", "1", "has no process nothing"},
		{"process.web", "21", "between 0 and 20"},
		{"process.web", "-1", "between 0 and 20"},
		{"process.web", "many", "not a number"},
	} {
		res, body = b.post("/apps/shop/scale", url.Values{"csrf": {token}, tc.field: {tc.value}})
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, tc.want) || !strings.Contains(body, `role="alert"`) {
			t.Errorf("%s=%s: %d\n%s", tc.field, tc.value, res.StatusCode, body)
		}
	}
	for _, in := range instances(t, c, "shop") {
		if !before[in.PID] {
			t.Errorf("an instance was replaced: %+v", in)
		}
	}

	// The number stays through a restart and a new release, whose log says so.
	if _, err := c.Restart(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if n := len(instances(t, c, "shop")); n != 2 {
		t.Errorf("%d instances after a restart", n)
	}
	d, err := deployApp(t, c, "shop", appPlan(map[string]string{"TESTAPP_BODY": "the second"}))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(instances(t, c, "shop")); n != 2 {
		t.Errorf("%d instances after a deploy", n)
	}

	// The deployment's page: how it went, its log, what it runs and what changed.
	res, page = b.get("/deployments/" + d.ID)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, page)
	}
	contains(t, "the deployment", page, "Live", "v2", "rollout", "web ×2", "✔ live", "What changed since v1", "Resolved manifest", "TESTAPP_BODY")
	if strings.Contains(page, "data-log-follow") || strings.Contains(page, "http-equiv") {
		t.Errorf("a finished deployment has nothing to follow:\n%s", page)
	}
	_, history := b.get("/apps/shop/deployments")
	contains(t, "the history", history, d.ID, "v1", "v2", "serving")

	// Rolling back: from the app's page, to a deployment that says what it did.
	if !strings.Contains(page, "Redeploy") || strings.Contains(page, "Roll back to v2") {
		t.Errorf("the deployment that serves:\n%s", page)
	}
	res, body = b.post("/apps/shop/rollback", url.Values{"csrf": {token}, "version": {"1"}})
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/deployments/dep_") {
		t.Fatalf("rollback: %d %s %s", res.StatusCode, res.Header.Get("Location"), body)
	}
	_, page = b.get(res.Header.Get("Location"))
	contains(t, "the rollback", page, "Roll back", "v3", "✔ live", "What changed since v2", "not needed")
	if _, served := get(t, addrs.HTTP, "shop.localhost", "/"); strings.Contains(served, "the second") {
		t.Errorf("still the second: %q", served)
	}
	res, body = b.post("/apps/shop/rollback", url.Values{"csrf": {token}, "version": {"3"}})
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "already the current release") {
		t.Errorf("rolling back to itself: %d\n%s", res.StatusCode, body)
	}
	_, page = b.get("/deployments/" + d.ID)
	contains(t, "an earlier deployment", page, "Roll back to v2", "With the variables it ran with")

	// Scaling down: what goes finishes, and the app still answers.
	res, body = b.post("/apps/shop/scale", url.Values{"csrf": {token}, "process.web": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("scale down: %d %s", res.StatusCode, body)
	}
	if ins := instances(t, c, "shop"); len(ins) != 1 || ins[0].Index != 1 {
		t.Errorf("%+v", ins)
	}
	if got, _ := get(t, addrs.HTTP, "shop.localhost", "/"); got.StatusCode != http.StatusOK {
		t.Errorf("after scaling down: %d", got.StatusCode)
	}

	// Scaling to none: an app that is not running, and says so.
	if res, body = b.post("/apps/shop/scale", url.Values{"csrf": {token}, "process.web": {"0"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("scale to zero: %d %s", res.StatusCode, body)
	}
	if ins := instances(t, c, "shop"); len(ins) != 0 {
		t.Errorf("%+v", ins)
	}
	_, list = b.get("/")
	contains(t, "the apps", list, "Scaled to zero")

	// A release uploaded ready to run has nothing to build.
	res, body = b.post("/apps/shop/redeploy", url.Values{"csrf": {token}})
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "ready to run") {
		t.Errorf("redeploy: %d\n%s", res.StatusCode, body)
	}
	_, activity := b.get("/activity?app=shop")
	contains(t, "the activity", activity, "Restarted by owner@example.com", "Scaled to web ×0 by owner@example.com", "Rolled back by owner@example.com")
}

// TestScalingThatFailsGoesBack keeps an app as it was when a new instance cannot start.
func TestScalingThatFailsGoesBack(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()

	if _, err := deployApp(t, c, "shop", appPlan(map[string]string{"TESTAPP_FAIL_BEYOND": "1"})); err != nil {
		t.Fatal(err)
	}
	first := instances(t, c, "shop")[0]

	_, err := c.Scale(ctx, "shop", "production", map[string]int{"web": 2})
	var problem *client.APIError
	if !errors.As(err, &problem) || problem.Code != "scale_failed" || !strings.Contains(problem.Detail, "as many instances as before") {
		t.Fatalf("%v", err)
	}
	ins := instances(t, c, "shop")
	if len(ins) != 1 || ins[0].ID != first.ID {
		t.Errorf("%+v", ins)
	}
	state, err := c.Environment(ctx, "shop", "production")
	if err != nil || state.Formation["web"] != 1 {
		t.Errorf("%+v %v", state, err)
	}

	res, body := b.post("/apps/shop/scale", url.Values{"csrf": {b.token()}, "process.web": {"2"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "keeps running as it was") || !strings.Contains(body, `role="alert"`) {
		t.Errorf("%d\n%s", res.StatusCode, body)
	}
	if got, _ := get(t, addrs.HTTP, "shop.localhost", "/"); got.StatusCode != http.StatusOK {
		t.Errorf("after the failure: %d", got.StatusCode)
	}
}

// followed waits for a queued deployment to end.
func followed(t *testing.T, c *client.Client, id string) apitypes.Deployment {
	t.Helper()
	d, err := c.FollowDeployment(context.Background(), id, func(apitypes.LogLine) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestRedeploy builds an upload again from the copy kept with its build.
func TestRedeploy(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()

	d, _ := deploySource(t, c, "docs", map[string]string{"index.html": "<h1>the docs</h1>"})
	if d.Status != "live" {
		t.Fatalf("%+v", d)
	}
	kept := filepath.Join(cfg.DataDir, "builds", "docs", d.Build, "source.tar.gz")
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the source is not kept: %v", err)
	}
	for version := 2; version <= 3; version++ {
		again, err := c.Redeploy(ctx, "docs", "production")
		if err != nil {
			t.Fatal(err)
		}
		done := followed(t, c, again.ID)
		if done.Status != "live" || done.Source != "source-upload" || done.Release == nil || done.Release.Version != version || done.Build == d.Build {
			t.Errorf("redeployed: %+v", done)
		}
		if _, body := get(t, addrs.HTTP, "docs.localhost", "/"); !strings.Contains(body, "the docs") {
			t.Errorf("v%d serves %q", version, body)
		}
		d = done
	}

	// A build from before sources were kept says what to do instead.
	if err := os.Remove(filepath.Join(cfg.DataDir, "builds", "docs", d.Build, "source.tar.gz")); err != nil {
		t.Fatal(err)
	}
	_, err := c.Redeploy(ctx, "docs", "production")
	var problem *client.APIError
	if !errors.As(err, &problem) || problem.Status != http.StatusConflict || problem.Code != "cannot_redeploy" || !strings.Contains(problem.Detail, "deploy it again") {
		t.Errorf("%v", err)
	}
	if _, err := c.Redeploy(ctx, "nothing", "production"); !errors.As(err, &problem) || problem.Status != http.StatusNotFound {
		t.Errorf("%v", err)
	}
}

// TestReleaseDetails reads what a release runs, and what changed between two, which
// says nothing of a variable's value.
func TestReleaseDetails(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()

	if _, err := deployApp(t, c, "shop", appPlan(map[string]string{"FIRST": "1"})); err != nil {
		t.Fatal(err)
	}
	setVars(t, c, "shop", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "VISIBLE", Value: "plain-value"}}})
	secretValue := "a-secret-value-" + strconv.Itoa(os.Getpid())
	setVars(t, c, "shop", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "HIDDEN", Value: secretValue, Secret: true}}})
	if _, err := deployApp(t, c, "shop", appPlan(map[string]string{"SECOND": "2"})); err != nil {
		t.Fatal(err)
	}

	rel, err := c.Release(ctx, "shop", "production", 4)
	if err != nil || rel.Version != 4 || !strings.Contains(rel.Manifest, "SECOND=") || len(rel.Plan) == 0 {
		t.Fatalf("%+v %v", rel, err)
	}

	same, err := c.ReleaseDiff(ctx, "shop", "production", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if same.BuildChanged || len(same.Vars) != 1 || same.Vars[0].Name != "VISIBLE" || same.Vars[0].Change != apitypes.DiffAddedVar {
		t.Errorf("%+v", same)
	}

	diff, err := c.ReleaseDiff(ctx, "shop", "production", 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !diff.BuildChanged || diff.From != 1 || diff.To != 4 {
		t.Errorf("%+v", diff)
	}
	var added, removed bool
	for _, l := range diff.Manifest {
		added = added || (l.Op == apitypes.DiffAdded && strings.Contains(l.Text, "SECOND="))
		removed = removed || (l.Op == apitypes.DiffRemoved && strings.Contains(l.Text, "FIRST="))
	}
	if !added || !removed {
		t.Errorf("%+v", diff.Manifest)
	}
	var secret bool
	for _, v := range diff.Vars {
		secret = secret || (v.Name == "HIDDEN" && v.Secret && v.Change == apitypes.DiffAddedVar)
	}
	if !secret {
		t.Errorf("%+v", diff.Vars)
	}

	// Neither the API's answer nor the console's page has a value.
	for _, path := range []string{"/api/v1/apps/shop/envs/production/releases/1/diff/4", "/api/v1/apps/shop/envs/production/releases/4"} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+path, nil)
		token, _ := os.ReadFile(NewLayout(cfg.DataDir).AdminToken)
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || strings.Contains(string(body), secretValue) || strings.Contains(string(body), "plain-value") {
			t.Errorf("%s: %d\n%s", path, res.StatusCode, body)
		}
	}
	deps, err := c.Deployments(ctx, "shop", "production", 1)
	if err != nil {
		t.Fatal(err)
	}
	b := signIn(t, cfg, addrs)
	_, page := b.get("/deployments/" + deps[0].ID)
	contains(t, "the deployment", page, "What changed since v3", "SECOND=")
	if strings.Contains(page, secretValue) || strings.Contains(page, "plain-value") {
		t.Errorf("a value on the page:\n%s", page)
	}

	for _, version := range []int{9} {
		var problem *client.APIError
		if _, err := c.Release(ctx, "shop", "production", version); !errors.As(err, &problem) || problem.Status != http.StatusNotFound {
			t.Errorf("%v", err)
		}
	}
}

// sourceArchive is a .tar.gz of one file.
func sourceArchive(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// TestConsoleUpload creates an app by uploading its source from the console's form,
// whose token comes before the file.
func TestConsoleUpload(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)

	upload := func(app, token string, archive []byte) (*http.Response, string) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		mw.WriteField("csrf", token)
		part, _ := mw.CreateFormFile("archive", "app.tar.gz")
		part.Write(archive)
		mw.Close()
		req, _ := http.NewRequest(http.MethodPost, "http://"+addrs.HTTP.String()+"/apps/"+app+"/deploy/archive", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		return b.do(req)
	}
	token := b.token()
	archive := sourceArchive(t, "index.html", "<h1>from the console</h1>")

	if res, _ := upload("site", "wrong", archive); res.StatusCode != http.StatusForbidden {
		t.Errorf("a wrong token: %d", res.StatusCode)
	}
	if res, body := upload("site", token, []byte("not an archive")); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `role="alert"`) {
		t.Errorf("not an archive: %d\n%s", res.StatusCode, body)
	}
	res, body := upload("site", token, archive)
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/deployments/dep_") {
		t.Fatalf("%d %s\n%s", res.StatusCode, res.Header.Get("Location"), body)
	}
	id := strings.TrimPrefix(res.Header.Get("Location"), "/deployments/")
	if d := followed(t, c, id); d.Status != "live" || d.CreatedBy != "owner@example.com" {
		t.Errorf("%+v", d)
	}
	if _, served := get(t, addrs.HTTP, "site.localhost", "/"); !strings.Contains(served, "from the console") {
		t.Errorf("serves %q", served)
	}
	_, page := b.get("/apps/site")
	contains(t, "a static app", page, "Live", "runs no instances", "Uploaded source")
	if strings.Contains(page, "/apps/site/restart") || strings.Contains(page, "/apps/site/scale") {
		t.Errorf("a static site has no instances to act on:\n%s", page)
	}
	res, body = b.post("/apps/site/restart", url.Values{"csrf": {token}})
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "static site") {
		t.Errorf("restarting a static site: %d\n%s", res.StatusCode, body)
	}
	res, body = b.post("/apps/site/scale", url.Values{"csrf": {token}, "process.web": {"2"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "static site") {
		t.Errorf("scaling a static site: %d\n%s", res.StatusCode, body)
	}

	// An image needs a name that could be one.
	res, body = b.post("/apps/other/deploy/image", url.Values{"csrf": {token}, "image": {"not an image"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `role="alert"`) || !strings.Contains(body, "not an image") {
		t.Errorf("%d\n%s", res.StatusCode, body)
	}
	if res, _ := b.get("/apps/other"); res.StatusCode != http.StatusNotFound {
		t.Errorf("nothing was created: %d", res.StatusCode)
	}
}
