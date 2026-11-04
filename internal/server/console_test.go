package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/store"
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

// postRaw is post, with a body of its own.
func (b *consoleUser) postRaw(path, contentType string, body []byte) (*http.Response, string) {
	b.t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+b.addr.String()+path, bytes.NewReader(body))
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	return b.do(req)
}

// postJSON is post, as a script on a page does it, reading the answer.
func (b *consoleUser) postJSON(path string, form url.Values, out any) error {
	b.t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+b.addr.String()+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	_, body := b.do(req)
	return json.Unmarshal([]byte(body), out)
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
	contains(t, "the app", page, "web.1", "Healthy", `name="process.web"`, "/apps/shop/restart", "/apps/shop/redeploy", `data-refresh-on="shop"`, `/apps/shop/logs?of=web.1`, `<a href="/deployments/dep_`)
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

// TestConsoleLogs reads an app's logs from its page and from the API behind it.
func TestConsoleLogs(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()

	if _, err := deployApp(t, c, "shop", appPlan(map[string]string{"TESTAPP_PRINT": "MOORAGE_APP"})); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the app's first line", func() bool {
		var got []apitypes.LogLine
		c.Logs(ctx, "shop", "production", client.LogQuery{Tail: 10}, func(l apitypes.LogLine) error {
			got = append(got, l)
			return nil
		})
		return len(got) > 0
	})
	if res, _ := get(t, addrs.HTTP, "shop.localhost", "/health"); res.StatusCode != http.StatusOK {
		t.Errorf("%d", res.StatusCode)
	}

	res, page := b.get("/apps/shop/logs")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, page)
	}
	contains(t, "the logs page", page, "web.1", "data-logs-stream=", "follow=1", "Download", `value="stderr"`, `value="build"`, `value="access"`, `value="events"`)

	// The form is a plain one: what it asks for is in the address.
	_, page = b.get("/apps/shop/logs?source=build")
	contains(t, "the builds", page, "s-build", "rollout", "source=build")
	if strings.Contains(page, "s-stdout") {
		t.Errorf("not asked for:\n%s", page)
	}
	waitUntil(t, "the access log", func() bool {
		_, page = b.get("/apps/shop/logs?source=access")
		return strings.Contains(page, "GET /health 200")
	})
	_, page = b.get("/apps/shop/logs?source=events")
	contains(t, "the events", page, "s-events", "Deployed v1")
	_, page = b.get("/apps/shop/logs?q=no+such+line")
	contains(t, "a search with no result", page, "No lines match that.")
	_, page = b.get("/apps/shop/logs?of=web.1&range=1h&problems=1")
	contains(t, "the form as it was asked", page, `value="web.1" selected`, `value="1h" selected`, `name="problems" value="1" checked`, "problems=1", "since=")
	res, page = b.get("/apps/shop/logs?range=custom&from=soon")
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, `role="alert"`) {
		t.Errorf("a time that is none: %d\n%s", res.StatusCode, page)
	}
	res, page = b.get("/apps/shop/logs?range=custom&from=2026-11-03T10:00&to=2026-11-03T09:00&tz=480")
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, "before its start") {
		t.Errorf("a range the wrong way round: %d\n%s", res.StatusCode, page)
	}
	_, page = b.get("/apps/shop/logs?range=custom&from=2020-01-01T10:00&to=2020-01-02T10:00")
	if strings.Contains(page, "data-logs-stream") {
		t.Errorf("a range that has ended has nothing to follow:\n%s", page)
	}
	if res, _ := b.get("/apps/none/logs"); res.StatusCode != http.StatusNotFound {
		t.Errorf("%d", res.StatusCode)
	}

	// Download, with a session as a browser has one.
	res, body := b.get("/api/v1/apps/shop/envs/production/logs?download=1&format=text&source=build")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Disposition"), `attachment; filename="shop-`) ||
		!strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") || !strings.Contains(body, " build build ") {
		t.Errorf("%d %v\n%s", res.StatusCode, res.Header, body)
	}
	// The API refuses what is not a source or a time, and says so.
	for _, path := range []string{"source=nothing", "since=soon"} {
		res, body := b.get("/api/v1/apps/shop/envs/production/logs?" + path)
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", path, res.StatusCode, body)
		}
	}
}

// TestConsoleVariables changes an app's variables from its page.
func TestConsoleVariables(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()

	if _, err := deployApp(t, c, "shop", appPlan(map[string]string{"FROM_MANIFEST": "m"})); err != nil {
		t.Fatal(err)
	}
	served := func(name string) string {
		t.Helper()
		_, body := get(t, addrs.HTTP, "shop.localhost", "/env?name="+name)
		return body
	}
	token := b.token()
	_, page := b.get("/apps/shop/variables")
	contains(t, "the variables", page, "FROM_MANIFEST", "moorage.toml", "Add a variable", "Edit as text")

	// Set: released, and running.
	res, body := b.post("/apps/shop/variables/set", url.Values{"csrf": {token}, "name": {"PLAIN"}, "value": {"one"}})
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "done=released") {
		t.Fatalf("%d %s %s", res.StatusCode, res.Header.Get("Location"), body)
	}
	if got := served("PLAIN"); got != "one" {
		t.Errorf("the app has %q", got)
	}
	_, page = b.get(res.Header.Get("Location"))
	contains(t, "the page after", page, "The change is released", "/deployments/dep_", "PLAIN", "one", "This environment")

	// A secret: running, and never on a page.
	secretValue := "a-long-secret-value-" + strconv.Itoa(os.Getpid())
	res, _ = b.post("/apps/shop/variables/set", url.Values{"csrf": {token}, "name": {"HIDDEN"}, "value": {secretValue}, "secret": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", res.StatusCode)
	}
	if got := served("HIDDEN"); got != secretValue {
		t.Errorf("the app has %q", got)
	}
	_, page = b.get("/apps/shop/variables")
	contains(t, "the variables", page, "HIDDEN", "Secret", "••••••••")
	if strings.Contains(page, secretValue) {
		t.Errorf("a secret on the page:\n%s", page)
	}
	res, body = b.post("/apps/shop/variables/set", url.Values{"csrf": {token}, "name": {"HIDDEN"}, "value": {""}, "secret": {"1"}, "was_secret": {"1"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "a new value") {
		t.Errorf("a secret left empty: %d", res.StatusCode)
	}

	// What cannot be is refused, and says why.
	for name, want := range map[string]string{"PORT": "set by Moorage", "1NO": "not a valid variable name"} {
		res, body := b.post("/apps/shop/variables/set", url.Values{"csrf": {token}, "name": {name}, "value": {"x"}})
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, want) {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}

	// Saved without releasing: the page says so, and offers to.
	res, _ = b.post("/apps/shop/variables/set", url.Values{"csrf": {token}, "name": {"LATER"}, "value": {"soon"}, "hold": {"1"}})
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "done=saved") {
		t.Fatalf("%d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if got := served("LATER"); got != "" {
		t.Errorf("released already: %q", got)
	}
	_, page = b.get("/apps/shop/variables")
	contains(t, "the variables", page, "Not released yet", "<code>LATER</code> added", "Release them")
	res, _ = b.post("/apps/shop/variables/release", url.Values{"csrf": {token}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", res.StatusCode)
	}
	if got := served("LATER"); got != "soon" {
		t.Errorf("not released: %q", got)
	}
	_, page = b.get("/apps/shop/variables")
	if strings.Contains(page, "Not released yet") {
		t.Errorf("still unreleased:\n%s", page)
	}

	// The text editor holds the environment's own, secrets masked.
	_, page = b.get("/apps/shop/variables/edit")
	contains(t, "the editor", page, "PLAIN=one", "HIDDEN=••••••••", "LATER=soon", "See what changes")
	if strings.Contains(page, secretValue) || strings.Contains(page, "FROM_MANIFEST=") {
		t.Errorf("the editor:\n%s", page)
	}

	// A mistake says where; nothing changes.
	res, body = b.post("/apps/shop/variables/preview", url.Values{"csrf": {token}, "text": {"PLAIN=one\nno equals sign\n"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Line 2") {
		t.Errorf("%d\n%s", res.StatusCode, body)
	}
	res, body = b.post("/apps/shop/variables/preview", url.Values{"csrf": {token}, "text": {"NEW=••••••••\n"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "NEW has no value to keep") {
		t.Errorf("%d\n%s", res.StatusCode, body)
	}

	// The preview says what would change, and changes nothing.
	text := "PLAIN=two\nHIDDEN=••••••••\nADDED=" + secretValue + "x\n"
	_, page = b.post("/apps/shop/variables/preview", url.Values{"csrf": {token}, "text": {text}, "secret_new": {"1"}})
	contains(t, "the preview", page, "What this would change", "PLAIN", "changed", "one", "two", "ADDED", "added", "its new value is sealed", "LATER", "removed", "soon", "1 left as they are", "Apply and release")
	if got := served("PLAIN"); got != "one" {
		t.Errorf("a preview changed it: %q", got)
	}
	_, page = b.post("/apps/shop/variables/preview", url.Values{"csrf": {token}, "text": {"PLAIN=one\nHIDDEN=••••••••\nLATER=soon\n"}})
	contains(t, "a preview of nothing", page, "Nothing would change")

	// Applying it: one release of it all, the secret kept, the new one sealed.
	res, body = b.post("/apps/shop/variables/apply", url.Values{"csrf": {token}, "text": {text}, "secret_new": {"1"}})
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "done=released") {
		t.Fatalf("%d %s %s", res.StatusCode, res.Header.Get("Location"), body)
	}
	if served("PLAIN") != "two" || served("HIDDEN") != secretValue || served("ADDED") != secretValue+"x" || served("LATER") != "" {
		t.Errorf("%q %q %q %q", served("PLAIN"), served("HIDDEN"), served("ADDED"), served("LATER"))
	}
	vars, err := c.Vars(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vars {
		if v.Name == "ADDED" && (!v.Secret || v.Value != "") {
			t.Errorf("%+v", v)
		}
	}
	// A secret's value typed again is a new one; left masked, it is kept.
	same := "PLAIN=two\nHIDDEN=••••••••\nADDED=••••••••\n"
	if res, _ := b.post("/apps/shop/variables/apply", url.Values{"csrf": {token}, "text": {same}}); !strings.Contains(res.Header.Get("Location"), "done=none") {
		t.Errorf("nothing to do: %s", res.Header.Get("Location"))
	}

	// Removing one.
	res, _ = b.post("/apps/shop/variables/unset", url.Values{"csrf": {token}, "name": {"PLAIN"}})
	if res.StatusCode != http.StatusSeeOther || served("PLAIN") != "" {
		t.Errorf("%d %q", res.StatusCode, served("PLAIN"))
	}

	// A manifest's variable that the server overrides says so.
	b.post("/apps/shop/variables/set", url.Values{"csrf": {token}, "name": {"FROM_MANIFEST"}, "value": {"server"}})
	_, page = b.get("/apps/shop/variables")
	contains(t, "the variables", page, "Overridden")
	if got := served("FROM_MANIFEST"); got != "server" {
		t.Errorf("%q", got)
	}

	// A form of many long variables fits.
	long := strings.Repeat("x", 60<<10)
	res, body = b.post("/apps/shop/variables/set", url.Values{"csrf": {token}, "name": {"LONG"}, "value": {long}})
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d %s", res.StatusCode, body[:min(len(body), 400)])
	}
}

// TestConsoleAccount manages one's own account from its pages.
func TestConsoleAccount(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	b := signIn(t, cfg, addrs)
	token := b.token()

	_, page := b.get("/account")
	contains(t, "the account", page, "owner@example.com", "Administrator", "Change your password")
	_, page = b.get("/")
	contains(t, "every page", page, `href="/account"`, `href="/settings"`)

	// A second session, to see it end.
	other := &consoleUser{t: t, addr: addrs.HTTP, host: cfg.ConsoleHost}
	if res, body := other.post("/login", url.Values{"email": {"owner@example.com"}, "password": {"a long and unusual passphrase"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	_, page = b.get("/account/sessions")
	contains(t, "the sessions", page, "This one", "End the others")
	if n := strings.Count(page, `name="session"`); n != 2 {
		t.Errorf("%d sessions", n)
	}

	// The password: the current one, then twice the same, then strong enough.
	for _, tc := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"current": {"wrong"}, "new": {"another long passphrase"}, "confirm": {"another long passphrase"}}, "not your current password"},
		{url.Values{"current": {"a long and unusual passphrase"}, "new": {"another long passphrase"}, "confirm": {"different"}}, "differ"},
		{url.Values{"current": {"a long and unusual passphrase"}, "new": {"short"}, "confirm": {"short"}}, "at least"},
	} {
		tc.form.Set("csrf", token)
		res, body := b.post("/account/password", tc.form)
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, tc.want) {
			t.Errorf("%v: %d\n%s", tc.form, res.StatusCode, body)
		}
	}
	res, body := b.post("/account/password", url.Values{"csrf": {token}, "current": {"a long and unusual passphrase"}, "new": {"another long passphrase"}, "confirm": {"another long passphrase"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	_, page = b.get("/account?done=password")
	contains(t, "the account", page, "Your password is changed")
	if res, _ := other.get("/account"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("the other session goes on: %d", res.StatusCode)
	}
	if res, _ := other.post("/login", url.Values{"email": {"owner@example.com"}, "password": {"a long and unusual passphrase"}}); res.StatusCode == http.StatusSeeOther {
		t.Errorf("the old password still works")
	}
	if res, _ := other.post("/login", url.Values{"email": {"owner@example.com"}, "password": {"another long passphrase"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("the new password does not work: %d", res.StatusCode)
	}

	// Ending the others, then this one.
	if res, _ := b.post("/account/sessions/end-others", url.Values{"csrf": {token}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	if res, _ := other.get("/account"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("the other session goes on: %d", res.StatusCode)
	}

	// SSH keys.
	_, page = b.get("/account/keys")
	contains(t, "no keys", page, "No SSH keys yet", "moor keys add")
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJh1Ckl6QZyXNzTc0aJ9Y0xXDq7Fv0m9o4vM1qkY1b2P laptop"
	res, body = b.post("/account/keys/add", url.Values{"csrf": {token}, "key": {"not a key"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "not a public key") || !strings.Contains(body, "not a key") {
		t.Errorf("%d\n%s", res.StatusCode, body)
	}
	if res, body = b.post("/account/keys/add", url.Values{"csrf": {token}, "key": {key}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	_, page = b.get("/account/keys")
	contains(t, "the keys", page, "laptop", "SHA256:", "Never")
	if res, _ = b.post("/account/keys/add", url.Values{"csrf": {token}, "key": {key}}); res.StatusCode != http.StatusConflict {
		t.Errorf("the same key: %d", res.StatusCode)
	}
	if res, _ = b.post("/account/keys/remove", url.Values{"csrf": {token}, "key": {"laptop"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	_, page = b.get("/account/keys")
	contains(t, "the keys", page, "No SSH keys yet")

	// Tokens: shown once, and they work.
	res, body = b.post("/account/tokens/create", url.Values{"csrf": {token}, "name": {"ci"}, "scope": {"deploy"}, "expires": {"30d"}})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	secret := regexp.MustCompile(`moor_[A-Za-z0-9_-]+`).FindString(body)
	if secret == "" || !strings.Contains(body, "only time") {
		t.Fatalf("no token in:\n%s", body)
	}
	tokenClient, err := client.New("http://"+addrs.HTTP.String(), secret, "")
	if err != nil {
		t.Fatal(err)
	}
	if me, err := tokenClient.Me(context.Background()); err != nil || me.Email != "owner@example.com" {
		t.Errorf("%+v %v", me, err)
	}
	_, page = b.get("/account/tokens")
	contains(t, "the tokens", page, "ci", "deploy", "Every app")
	if strings.Contains(page, secret) {
		t.Errorf("the secret again:\n%s", page)
	}
	if res, body = b.post("/account/tokens/create", url.Values{"csrf": {token}, "name": {"ci"}, "scope": {"read"}}); res.StatusCode != http.StatusConflict {
		t.Errorf("the same name: %d", res.StatusCode)
	}
	if res, body = b.post("/account/tokens/create", url.Values{"csrf": {token}, "name": {"none"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("no scope: %d", res.StatusCode)
	}
	id := regexp.MustCompile(`name="token" value="([^"]+)"`).FindStringSubmatch(page)
	if id == nil {
		t.Fatalf("no form:\n%s", page)
	}
	if res, _ = b.post("/account/tokens/revoke", url.Values{"csrf": {token}, "token": {id[1]}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	if _, err := tokenClient.Me(context.Background()); err == nil {
		t.Errorf("a revoked token still works")
	}

	// Ending this session signs out.
	_, page = b.get("/account/sessions")
	session := regexp.MustCompile(`name="session" value="([^"]+)"`).FindStringSubmatch(page)
	if session == nil {
		t.Fatalf("no form:\n%s", page)
	}
	res, _ = b.post("/account/sessions/end", url.Values{"csrf": {token}, "session": {session[1]}})
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	b.cookies = res.Cookies()
	if res, _ := b.get("/account"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("still signed in: %d", res.StatusCode)
	}
}

// TestConsoleSettings is the administrator's pages, and who may see them.
func TestConsoleSettings(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	b := signIn(t, cfg, addrs)
	token := b.token()

	_, page := b.get("/settings")
	contains(t, "the settings", page, "moorage.localhost", "Routing", "Off: plain HTTP", "moor server restart")
	_, page = b.get("/settings/system")
	contains(t, "the system", page, "checks passed", "listen.http", "Version", "Healthy instances", "moor server doctor")

	// Users.
	_, page = b.get("/settings/users")
	contains(t, "the users", page, "owner@example.com", "You", "Add someone")
	res, body := b.post("/settings/users/create", url.Values{"csrf": {token}, "email": {"not an address"}, "role": {"member"}})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	res, body = b.post("/settings/users/create", url.Values{"csrf": {token}, "email": {"member@example.com"}, "role": {"member"}})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	contains(t, "the new user", body, "A link for member@example.com", "/invite?code=", "Invited")
	link := regexp.MustCompile(`/invite\?code=[^<"\s]+`).FindString(body)
	if res, _ = b.post("/settings/users/create", url.Values{"csrf": {token}, "email": {"member@example.com"}, "role": {"member"}}); res.StatusCode != http.StatusConflict {
		t.Errorf("the same address: %d", res.StatusCode)
	}

	// They choose a password with the link, and are no admin.
	member := &consoleUser{t: t, addr: addrs.HTTP, host: cfg.ConsoleHost}
	if res, _ := member.get(strings.ReplaceAll(link, "&amp;", "&")); res.StatusCode != http.StatusOK {
		t.Fatalf("the link: %d", res.StatusCode)
	}
	code := strings.TrimPrefix(link, "/invite?code=")
	if res, body := member.post("/invite", url.Values{"code": {code}, "password": {"a passphrase of their own"}, "confirm": {"a passphrase of their own"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	_, page = member.get("/")
	if strings.Contains(page, `href="/settings"`) {
		t.Errorf("a member is offered the settings:\n%s", page)
	}
	memberToken := member.token()
	for _, path := range []string{"/settings", "/settings/users", "/settings/registries", "/settings/system"} {
		if res, _ := member.get(path); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s for a member: %d", path, res.StatusCode)
		}
	}
	for _, path := range []string{"/settings/users/create", "/settings/users/change", "/settings/users/invite", "/settings/registries/set", "/settings/registries/remove"} {
		if res, _ := member.post(path, url.Values{"csrf": {memberToken}, "email": {"x@example.com"}, "role": {"admin"}}); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s for a member: %d", path, res.StatusCode)
		}
	}

	// Their role, and whether they may sign in.
	_, page = b.get("/settings/users")
	ids := regexp.MustCompile(`name="user" value="([^"]+)"`).FindAllStringSubmatch(page, -1)
	if len(ids) == 0 {
		t.Fatalf("no users:\n%s", page)
	}
	var memberID, ownerID string
	for _, m := range ids {
		row := page[strings.Index(page, m[1])-400 : strings.Index(page, m[1])]
		if strings.Contains(row, "member@example.com") {
			memberID = m[1]
		} else if strings.Contains(row, "owner@example.com") {
			ownerID = m[1]
		}
	}
	if memberID == "" || ownerID == "" {
		t.Fatalf("%q %q\n%s", memberID, ownerID, page)
	}
	res, body = b.post("/settings/users/change", url.Values{"csrf": {token}, "user": {ownerID}, "change": {"role"}, "role": {"member"}})
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "no administrator") {
		t.Errorf("the last admin: %d\n%s", res.StatusCode, body)
	}
	res, _ = b.post("/settings/users/change", url.Values{"csrf": {token}, "user": {memberID}, "change": {"role"}, "role": {"admin"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	if res, _ := member.get("/settings"); res.StatusCode != http.StatusOK {
		t.Errorf("an admin now: %d", res.StatusCode)
	}
	res, _ = b.post("/settings/users/change", url.Values{"csrf": {token}, "user": {memberID}, "change": {"disable"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	if res, _ := member.get("/settings"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("a disabled account goes on: %d", res.StatusCode)
	}
	_, page = b.get("/settings/users")
	contains(t, "the users", page, "Disabled", "Enable")
	res, _ = b.post("/settings/users/change", url.Values{"csrf": {token}, "user": {memberID}, "change": {"enable"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	res, body = b.post("/settings/users/invite", url.Values{"csrf": {token}, "user": {memberID}})
	if res.StatusCode != http.StatusCreated || !strings.Contains(body, "/invite?code=") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if res, _ := b.post("/settings/users/invite", url.Values{"csrf": {token}, "user": {"usr_none"}}); res.StatusCode != http.StatusNotFound {
		t.Errorf("no such user: %d", res.StatusCode)
	}

	// Registries.
	_, page = b.get("/settings/registries")
	contains(t, "no registries", page, "No registries yet")
	res, body = b.post("/settings/registries/set", url.Values{"csrf": {token}, "host": {"Not A Host"}, "username": {"u"}, "password": {"p"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "ghcr.io") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	res, body = b.post("/settings/registries/set", url.Values{"csrf": {token}, "host": {"ghcr.io"}, "username": {"u"}})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("no password: %d", res.StatusCode)
	}
	res, body = b.post("/settings/registries/set", url.Values{"csrf": {token}, "host": {"GHCR.io"}, "username": {"someone"}, "password": {"a-registry-secret"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	_, page = b.get("/settings/registries")
	contains(t, "the registries", page, "ghcr.io", "someone")
	if strings.Contains(page, "a-registry-secret") {
		t.Errorf("the password on the page:\n%s", page)
	}
	if res, _ := b.post("/settings/registries/remove", url.Values{"csrf": {token}, "host": {"ghcr.io"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	_, page = b.get("/settings/registries")
	contains(t, "no registries", page, "No registries yet")

	// The same checks through the API.
	res, body = b.get("/api/v1/server/doctor")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"listen.http"`) {
		t.Errorf("%d %s", res.StatusCode, body)
	}
}

// addTeammate makes an account, through the pages, and signs its owner in.
func addTeammate(t *testing.T, cfg config.Config, addrs Addrs, admin *consoleUser, email string) *consoleUser {
	t.Helper()
	res, body := admin.post("/settings/users/create", url.Values{"csrf": {admin.token()}, "email": {email}, "role": {"member"}})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	link := regexp.MustCompile(`/invite\?code=([^<"\s]+)`).FindStringSubmatch(body)
	if link == nil {
		t.Fatalf("no link in: %s", body)
	}
	u := &consoleUser{t: t, addr: addrs.HTTP, host: cfg.ConsoleHost}
	if res, body := u.post("/invite", url.Values{"code": {link[1]}, "password": {"a passphrase of their own"}, "confirm": {"a passphrase of their own"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	return u
}

// TestProjectsAndRoles has someone go through each role in a project, in the pages.
func TestProjectsAndRoles(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	ownerToken := owner.token()

	// A project, an app in it, and an app in the default project.
	res, body := owner.post("/projects", url.Values{"csrf": {ownerToken}, "name": {"payments"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/projects/payments" {
		t.Fatalf("%d %s %s", res.StatusCode, res.Header.Get("Location"), body)
	}
	if res, _ := owner.post("/projects", url.Values{"csrf": {ownerToken}, "name": {"payments"}}); res.StatusCode != http.StatusConflict {
		t.Errorf("the same name: %d", res.StatusCode)
	}
	if res, _ := owner.post("/projects", url.Values{"csrf": {ownerToken}, "name": {"Not A Name"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a bad name: %d", res.StatusCode)
	}
	if res, _ := owner.post("/new", url.Values{"csrf": {ownerToken}, "name": {"ledger"}, "project": {"payments"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", res.StatusCode)
	}
	plan := appPlan(nil)
	if _, err := deployApp(t, c, "ledger", plan); err != nil {
		t.Fatal(err)
	}
	if _, err := deployApp(t, c, "shop", plan); err != nil {
		t.Fatal(err)
	}
	if err := setSecret(c, "ledger", "API_KEY", "a-long-secret-of-the-ledger"); err != nil {
		t.Fatal(err)
	}
	_, list := owner.get("/")
	contains(t, "the owner's apps", list, `data-app-row="ledger"`, `data-app-row="shop"`, "payments")

	// Someone who is nothing in the project sees nothing of it.
	ana := addTeammate(t, cfg, addrs, owner, "ana@example.com")
	anaToken := ana.token()
	_, list = ana.get("/")
	contains(t, "her apps", list, `data-app-row="shop"`)
	if strings.Contains(list, "ledger") {
		t.Errorf("an app she has no role with, on every page:\n%s", list)
	}
	for _, path := range []string{"/apps/ledger", "/apps/ledger/logs", "/apps/ledger/variables", "/apps/ledger/deployments", "/apps/ledger/deploy", "/projects/payments", "/activity?app=ledger"} {
		if res, _ := ana.get(path); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	for _, path := range []string{"/apps/ledger/restart", "/apps/ledger/redeploy", "/apps/ledger/rollback", "/apps/ledger/scale", "/apps/ledger/variables/set", "/apps/ledger/deploy/image", "/projects/payments/settings"} {
		if res, _ := ana.post(path, url.Values{"csrf": {anaToken}, "image": {"nginx:1"}, "name": {"X"}}); res.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: %d", path, res.StatusCode)
		}
	}
	if res, _ := ana.post("/projects", url.Values{"csrf": {anaToken}, "name": {"mine"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a member making a project: %d", res.StatusCode)
	}
	deps, err := c.Deployments(ctx, "ledger", "production", 1)
	if err != nil || len(deps) == 0 {
		t.Fatalf("%v %v", deps, err)
	}
	if res, _ := ana.get("/deployments/" + deps[0].ID); res.StatusCode != http.StatusNotFound {
		t.Errorf("its deployment: %d", res.StatusCode)
	}
	if res, _ := ana.get("/api/v1/deployments/" + deps[0].ID); res.StatusCode != http.StatusNotFound {
		t.Errorf("its deployment, through the API: %d", res.StatusCode)
	}
	_, activity := ana.get("/activity")
	if strings.Contains(activity, "ledger") {
		t.Errorf("its events:\n%s", activity)
	}
	_, events := ana.get("/api/v1/events")
	if strings.Contains(events, "ledger") || !strings.Contains(events, "shop") {
		t.Errorf("its events, through the API:\n%s", events)
	}
	_, apps := ana.get("/api/v1/apps")
	if strings.Contains(apps, "ledger") {
		t.Errorf("the API's list:\n%s", apps)
	}
	_, projects := ana.get("/projects")
	if strings.Contains(projects, "payments") {
		t.Errorf("a project she has no role in:\n%s", projects)
	}
	if strings.Contains(projects, `name="name"`) {
		t.Errorf("a member is offered to make a project:\n%s", projects)
	}

	// A viewer sees it and its logs, and acts on nothing.
	res, _ = owner.post("/projects/payments/members/set", url.Values{"csrf": {ownerToken}, "email": {"ana@example.com"}, "role": {"viewer"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", res.StatusCode)
	}
	res, body = owner.post("/projects/payments/members/set", url.Values{"csrf": {ownerToken}, "email": {"nobody@example.com"}, "role": {"viewer"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "no account") {
		t.Errorf("no such account: %d", res.StatusCode)
	}
	_, page := ana.get("/apps/ledger")
	contains(t, "the app for a viewer", page, "web.1", "you are viewer", `/apps/ledger/logs`)
	for _, hidden := range []string{"/apps/ledger/restart", "/apps/ledger/redeploy", "/apps/ledger/scale", "/apps/ledger/variables", `/apps/ledger/deploy"`} {
		if strings.Contains(page, hidden) {
			t.Errorf("a viewer is offered %s:\n%s", hidden, page)
		}
	}
	if res, _ := ana.post("/apps/ledger/restart", url.Values{"csrf": {anaToken}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer restarting: %d", res.StatusCode)
	}
	if res, _ := ana.get("/apps/ledger/logs"); res.StatusCode != http.StatusOK {
		t.Errorf("a viewer's logs: %d", res.StatusCode)
	}
	if res, _ := ana.get("/apps/ledger/variables"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer's variables: %d", res.StatusCode)
	}
	if res, _ := ana.post("/projects/payments/members/set", url.Values{"csrf": {anaToken}, "email": {"ana@example.com"}, "role": {"maintainer"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer making herself a maintainer: %d", res.StatusCode)
	}
	_, page = ana.get("/projects/payments")
	contains(t, "the project for a viewer", page, "ana@example.com", "ledger")
	if strings.Contains(page, "members/set") {
		t.Errorf("a viewer is offered the members:\n%s", page)
	}

	// Logs can be kept from viewers.
	res, _ = owner.post("/projects/payments/settings", url.Values{"csrf": {ownerToken}, "everyone": {"none"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", res.StatusCode)
	}
	if res, _ := ana.get("/apps/ledger/logs"); res.StatusCode != http.StatusForbidden {
		t.Errorf("logs kept from a viewer: %d", res.StatusCode)
	}
	_, page = ana.get("/deployments/" + deps[0].ID)
	contains(t, "a deployment for such a viewer", page, "may not read logs")
	if strings.Contains(page, "▸ rollout") {
		t.Errorf("the log:\n%s", page)
	}
	if res, _ := ana.get("/api/v1/deployments/" + deps[0].ID + "/log"); res.StatusCode != http.StatusForbidden {
		t.Errorf("the log, through the API: %d", res.StatusCode)
	}
	owner.post("/projects/payments/settings", url.Values{"csrf": {ownerToken}, "everyone": {"none"}, "viewers_see_logs": {"1"}})

	// A deployer acts on it, and sees its variables without changing them.
	owner.post("/projects/payments/members/set", url.Values{"csrf": {ownerToken}, "email": {"ana@example.com"}, "role": {"deployer"}})
	if res, _ := ana.post("/apps/ledger/restart", url.Values{"csrf": {anaToken}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("a deployer restarting: %d", res.StatusCode)
	}
	_, page = ana.get("/apps/ledger/variables")
	contains(t, "the variables for a deployer", page, "API_KEY", "••••••••")
	for _, hidden := range []string{"variables/set", "variables/unset", "variables/reveal", "variables/edit"} {
		if strings.Contains(page, hidden) {
			t.Errorf("a deployer is offered %s:\n%s", hidden, page)
		}
	}
	for _, path := range []string{"/apps/ledger/variables/set", "/apps/ledger/variables/unset", "/apps/ledger/variables/apply", "/apps/ledger/variables/reveal"} {
		if res, _ := ana.post(path, url.Values{"csrf": {anaToken}, "name": {"API_KEY"}, "value": {"x"}}); res.StatusCode != http.StatusForbidden {
			t.Errorf("a deployer on %s: %d", path, res.StatusCode)
		}
	}
	if res, _ := ana.get("/apps/ledger/variables/edit"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a deployer's editor: %d", res.StatusCode)
	}

	// A maintainer changes them, reveals a secret, and answers for the members.
	owner.post("/projects/payments/members/set", url.Values{"csrf": {ownerToken}, "email": {"ana@example.com"}, "role": {"maintainer"}})
	res, body = ana.post("/apps/ledger/variables/reveal", url.Values{"csrf": {anaToken}, "name": {"API_KEY"}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "a-long-secret-of-the-ledger") {
		t.Errorf("revealing: %d", res.StatusCode)
	}
	_, page = ana.get("/apps/ledger/variables")
	if strings.Contains(page, "a-long-secret-of-the-ledger") {
		t.Errorf("the secret again:\n%s", page)
	}
	if res, body := ana.post("/apps/ledger/variables/reveal", url.Values{"csrf": {anaToken}, "name": {"NOTHING"}}); res.StatusCode != http.StatusConflict {
		t.Errorf("no such secret: %d %s", res.StatusCode, body)
	}
	revealed, err := c.RevealVar(ctx, "ledger", "production", "API_KEY")
	if err != nil || revealed.Value != "a-long-secret-of-the-ledger" {
		t.Errorf("%+v %v", revealed, err)
	}
	var problem *client.APIError
	if _, err := c.RevealVar(ctx, "ledger", "production", "NOTHING"); !errors.As(err, &problem) || problem.Status != http.StatusNotFound {
		t.Errorf("%v", err)
	}
	res, _ = ana.post("/projects/payments/members/set", url.Values{"csrf": {anaToken}, "email": {"owner@example.com"}, "role": {"viewer"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("a maintainer adding someone: %d", res.StatusCode)
	}
	_, page = ana.get("/projects/payments")
	contains(t, "the project for a maintainer", page, "owner@example.com", "members/remove", "Everyone else")

	// Without a role in the default project, nobody makes an app in it by deploying.
	owner.post("/projects/default/settings", url.Values{"csrf": {ownerToken}, "everyone": {"none"}, "viewers_see_logs": {"1"}})
	_, list = ana.get("/")
	if strings.Contains(list, `data-app-row="shop"`) {
		t.Errorf("shop, with no role left:\n%s", list)
	}
	if res, _ := ana.post("/new", url.Values{"csrf": {anaToken}, "name": {"her-app"}, "project": {"default"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("making an app: %d", res.StatusCode)
	}
	if res, _ := ana.post("/new", url.Values{"csrf": {anaToken}, "name": {"her-app"}, "project": {"payments"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("making an app where she may: %d", res.StatusCode)
	}

	// Deleting a project: empty, and not the default.
	if res, body := owner.post("/projects/payments/delete", url.Values{"csrf": {ownerToken}}); res.StatusCode != http.StatusConflict || !strings.Contains(body, "still has apps") {
		t.Errorf("%d", res.StatusCode)
	}
	if res, _ := owner.post("/projects/default/delete", url.Values{"csrf": {ownerToken}}); res.StatusCode != http.StatusConflict {
		t.Errorf("the default: %d", res.StatusCode)
	}
	if _, err := c.CreateProject(ctx, "empty"); err != nil {
		t.Fatal(err)
	}
	if res, _ := owner.post("/projects/empty/delete", url.Values{"csrf": {ownerToken}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	if res, _ := ana.post("/projects/payments/delete", url.Values{"csrf": {anaToken}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a maintainer deleting a project: %d", res.StatusCode)
	}

	// Who did what.
	ana.post("/login", url.Values{"email": {"ana@example.com"}, "password": {"wrong"}})
	_, page = owner.get("/settings/audit")
	contains(t, "the audit log", page, "ana@example.com", "secret.reveal", "signin", "/apps/ledger/restart", "POST /projects", "refused", "failed")
	_, page = owner.get("/settings/audit?actor=ana@example.com&action=secret")
	contains(t, "a search of it", page, "secret.reveal", "ledger/production/API_KEY")
	if strings.Contains(page, "POST /new") {
		t.Errorf("not asked for:\n%s", page)
	}
	if strings.Contains(page, "a-long-secret-of-the-ledger") {
		t.Errorf("a secret in the log:\n%s", page)
	}
	if res, _ := ana.get("/settings/audit"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a member reading it: %d", res.StatusCode)
	}
	res, export := owner.get("/api/v1/audit?format=ndjson&actor=ana@example.com")
	if res.StatusCode != http.StatusOK || !strings.Contains(export, `"action":"secret.reveal"`) || !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("the export: %d\n%s", res.StatusCode, export)
	}
	entries, err := c.Audit(ctx, client.AuditQuery{Limit: 200, Action: "signin"})
	if err != nil || len(entries) == 0 {
		t.Errorf("%v %v", entries, err)
	}
}

func setSecret(c *client.Client, app, name, value string) error {
	_, err := c.ChangeVars(context.Background(), app, "production", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: name, Value: value, Secret: true}}})
	return err
}

// TestGitByRole keeps pushes to those who may deploy.
func TestGitByRole(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	if _, err := c.CreateProject(context.Background(), "payments"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateApp(context.Background(), "ledger", "payments"); err != nil {
		t.Fatal(err)
	}
	owner := signIn(t, cfg, addrs)
	addTeammate(t, cfg, addrs, owner, "ana@example.com")
	authn := testAuth(t, cfg)
	ana, err := authn.DB.UserByEmail(context.Background(), "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{UserID: ana.ID, Name: ana.Email}
	allowed := func(push bool) bool { return allowGit(context.Background(), authn, p, "ledger", push) == nil }
	if allowed(false) || allowed(true) {
		t.Error("no role, and she reaches it")
	}
	if _, err := c.SetMember(context.Background(), "payments", ana.Email, store.RoleViewer); err != nil {
		t.Fatal(err)
	}
	if !allowed(false) || allowed(true) {
		t.Errorf("a viewer: reads %v, pushes %v", allowed(false), allowed(true))
	}
	if _, err := c.SetMember(context.Background(), "payments", ana.Email, store.RoleDeployer); err != nil {
		t.Fatal(err)
	}
	if !allowed(false) || !allowed(true) {
		t.Errorf("a deployer: reads %v, pushes %v", allowed(false), allowed(true))
	}
}

// testAuth opens the server's database a second time, as its checks read it.
func testAuth(t *testing.T, cfg config.Config) *auth.Service {
	t.Helper()
	db, err := store.Open(context.Background(), NewLayout(cfg.DataDir).Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &auth.Service{DB: db}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestRenameAndDeleteAnApp takes an app with real instances through its last changes.
func TestRenameAndDeleteAnApp(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()
	token := b.token()
	if _, err := deployApp(t, c, "shop", appPlan(map[string]string{"TESTAPP_BODY": "the first"})); err != nil {
		t.Fatal(err)
	}
	if err := setSecret(c, "shop", "API_KEY", "a-long-secret-value"); err != nil {
		t.Fatal(err)
	}
	if _, err := deployApp(t, c, "shop", appPlan(map[string]string{"TESTAPP_BODY": "the second"})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Scale(ctx, "shop", "production", map[string]int{"web": 2}); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(cfg.DataDir, "data")
	if err := os.MkdirAll(filepath.Join(data, "shop", "production"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "shop", "production", "kept.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := c.AppInfo(ctx, "shop")
	if err != nil || info.Project != "default" || info.Data != 4 || info.Logs == 0 {
		t.Errorf("%+v %v", info, err)
	}

	// The page says what there is, and asks first.
	_, page := b.get("/apps/shop/settings")
	contains(t, "the settings", page, "Delete this app", "4 B", "Rename it", "Move it", "To go on, type")
	if res, body := b.post("/apps/shop/settings/delete", url.Values{"csrf": {token}, "confirm": {"wrong"}}); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Nothing was deleted") {
		t.Errorf("the wrong name: %d", res.StatusCode)
	}
	if res, body := b.post("/apps/shop/settings/rename", url.Values{"csrf": {token}, "name": {"Not A Name"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a bad name: %d %s", res.StatusCode, body[:min(200, len(body))])
	}

	// Renaming: every part of it follows, and it serves under the new name.
	if _, err := c.CreateApp(ctx, "other", ""); err != nil {
		t.Fatal(err)
	}
	var problem *client.APIError
	if _, err := c.RenameApp(ctx, "shop", "other"); !errors.As(err, &problem) || problem.Code != "name_taken" {
		t.Errorf("a name in use: %v", err)
	}
	res, body := b.post("/apps/shop/settings/rename", url.Values{"csrf": {token}, "name": {"store"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/apps/store/settings?done=renamed" {
		t.Fatalf("%d %s\n%s", res.StatusCode, res.Header.Get("Location"), body)
	}
	if got, served := get(t, addrs.HTTP, "store.localhost", "/"); got.StatusCode != http.StatusOK || !strings.Contains(served, "the second") {
		t.Errorf("under the new name: %d %q", got.StatusCode, served)
	}
	if got, _ := get(t, addrs.HTTP, "shop.localhost", "/"); got.StatusCode != http.StatusNotFound {
		t.Errorf("under the old name: %d", got.StatusCode)
	}
	if _, served := get(t, addrs.HTTP, "store.localhost", "/env?name=API_KEY"); served != "a-long-secret-value" {
		t.Errorf("its secret: %q", served)
	}
	if _, served := get(t, addrs.HTTP, "store.localhost", "/env?name=MOORAGE_APP"); served != "store" {
		t.Errorf("its name: %q", served)
	}
	if ins := instances(t, c, "store"); len(ins) != 2 {
		t.Errorf("its number of instances: %+v", ins)
	}
	if ins := instances(t, c, "shop"); len(ins) != 0 {
		t.Errorf("under the old name: %+v", ins)
	}
	if !exists(filepath.Join(data, "store", "production", "kept.txt")) || exists(filepath.Join(data, "shop")) {
		t.Errorf("its data did not move")
	}
	_, page = b.get("/apps/store")
	contains(t, "the app", page, "web.1", "Healthy", "Renamed from shop by owner@example.com")
	if res, _ := b.get("/apps/shop"); res.StatusCode != http.StatusNotFound {
		t.Errorf("the old page: %d", res.StatusCode)
	}
	waitUntil(t, "its logs", func() bool {
		_, page := b.get("/apps/store/logs")
		return strings.Contains(page, "web.1")
	})
	deps, err := c.Deployments(ctx, "store", "production", 5)
	if err != nil || len(deps) == 0 {
		t.Fatalf("%v %v", deps, err)
	}
	_, page = b.get("/deployments/" + deps[0].ID)
	contains(t, "a deployment from before", page, "✔ live")

	// Rolling back reaches a build from before, where it has moved to.
	if _, err := c.Rollback(ctx, "store", "production", 1); err != nil {
		t.Fatalf("a rollback after renaming: %v", err)
	}
	if _, served := get(t, addrs.HTTP, "store.localhost", "/"); !strings.Contains(served, "the first") {
		t.Errorf("served %q", served)
	}

	// Moving it.
	if _, err := c.CreateProject(ctx, "payments"); err != nil {
		t.Fatal(err)
	}
	if res, _ := b.post("/apps/store/settings/move", url.Values{"csrf": {token}, "project": {"payments"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	if info, err := c.AppInfo(ctx, "store"); err != nil || info.Project != "payments" {
		t.Errorf("%+v %v", info, err)
	}
	if res, _ := b.post("/apps/store/settings/move", url.Values{"csrf": {token}, "project": {"nothing"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("no such project: %d", res.StatusCode)
	}

	// A deployment under way waits for nothing to be done to the app.
	db, err := store.Open(ctx, NewLayout(cfg.DataDir).Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	running := store.Deployment{ID: "dep_01M6JAE1CDN5QDNB0X4803DRW3", App: "store", Env: "production", SourceType: "source-upload", Status: store.Building, CreatedAt: time.Now()}
	if err := db.CreateDeployment(ctx, running); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteApp(ctx, "store", false); !errors.As(err, &problem) || problem.Code != "app_busy" {
		t.Errorf("during a deployment: %v", err)
	}
	if res, body := b.post("/apps/store/settings/delete", url.Values{"csrf": {token}, "confirm": {"store"}}); res.StatusCode != http.StatusConflict || !strings.Contains(body, "under way") {
		t.Errorf("during a deployment: %d", res.StatusCode)
	}
	if err := db.FinishDeployment(ctx, running.ID, store.Failed, "stopped", nil); err != nil {
		t.Fatal(err)
	}

	// Deleting it, keeping its data.
	pids := instances(t, c, "store")
	res, body = b.post("/apps/store/settings/delete", url.Values{"csrf": {token}, "confirm": {"store"}, "keep_data": {"1"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/?done=deleted" {
		t.Fatalf("%d %s\n%s", res.StatusCode, res.Header.Get("Location"), body)
	}
	_, list := b.get("/?done=deleted")
	contains(t, "the apps", list, "The app is deleted.")
	if strings.Contains(list, `data-app-row="store"`) {
		t.Errorf("still listed:\n%s", list)
	}
	if got, _ := get(t, addrs.HTTP, "store.localhost", "/"); got.StatusCode != http.StatusNotFound {
		t.Errorf("still served: %d", got.StatusCode)
	}
	if _, err := c.AppInfo(ctx, "store"); !errors.As(err, &problem) || problem.Status != http.StatusNotFound {
		t.Errorf("%v", err)
	}
	for _, in := range pids {
		if platformAlive(in.PID) {
			t.Errorf("an instance is still running: %d", in.PID)
		}
	}
	for _, gone := range []string{
		filepath.Join(cfg.DataDir, "builds", "store"), filepath.Join(cfg.DataDir, "logs", "apps", "store"),
		filepath.Join(cfg.DataDir, "logs", "deployments", "store"), filepath.Join(cfg.DataDir, "logs", "router", "store"),
	} {
		if exists(gone) {
			t.Errorf("%s is still there", gone)
		}
	}
	if !exists(filepath.Join(data, "store", "production", "kept.txt")) {
		t.Errorf("its data was not kept")
	}

	// A new app of that name starts afresh, with the data that was kept.
	if _, err := deployApp(t, c, "store", appPlan(nil)); err != nil {
		t.Fatal(err)
	}
	vars, err := c.Vars(ctx, "store", "production")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vars {
		if v.Name == "API_KEY" {
			t.Errorf("a variable of the old app: %+v", v)
		}
	}
	rels, err := c.Releases(ctx, "store", "production")
	if err != nil || len(rels) != 1 || rels[0].Version != 1 {
		t.Errorf("%+v %v", rels, err)
	}

	// And everything goes without being asked to keep it.
	if err := c.DeleteApp(ctx, "store", false); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(data, "store")) {
		t.Errorf("its data is still there")
	}
	if err := c.DeleteApp(ctx, "store", false); !errors.As(err, &problem) || problem.Status != http.StatusNotFound {
		t.Errorf("deleting it twice: %v", err)
	}

	// A static site has no instances to start again.
	if d, _ := deploySource(t, c, "docs", map[string]string{"index.html": "<h1>the docs</h1>"}); d.Status != "live" {
		t.Fatalf("%+v", d)
	}
	if _, err := c.RenameApp(ctx, "docs", "manual"); err != nil {
		t.Fatal(err)
	}
	if _, served := get(t, addrs.HTTP, "manual.localhost", "/"); !strings.Contains(served, "the docs") {
		t.Errorf("served %q", served)
	}
	_, audit := b.get("/settings/audit?action=delete")
	contains(t, "who did it", audit, "POST /apps/{app}/settings/delete", "owner@example.com")
}

func platformAlive(pid int) bool {
	if pid == 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		defer process.Release()
		return false // FindProcess fails there for what has gone; there is no signal to ask with
	}
	return process.Signal(syscall.Signal(0)) == nil
}
