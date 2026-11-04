package server

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
)

// dropped is a file of a folder, as a browser sends it.
type dropped struct{ path, body string }

// drop posts a folder as the page's form does: each file's path is its file name.
func drop(t *testing.T, b *consoleUser, address, token string, files []dropped) (*http.Response, string) {
	t.Helper()
	return dropWith(t, b, address, token, files, func(mw *multipart.Writer, path string) string { return path })
}

// dropByScript posts a folder as the page's script does: each file's path comes before it.
func dropByScript(t *testing.T, b *consoleUser, address, token string, files []dropped) (*http.Response, string) {
	t.Helper()
	return dropWith(t, b, address, token, files, func(mw *multipart.Writer, path string) string {
		if err := mw.WriteField("path", path); err != nil {
			t.Fatal(err)
		}
		return filepath.Base(path)
	})
}

func dropWith(t *testing.T, b *consoleUser, address, token string, files []dropped, named func(*multipart.Writer, string) string) (*http.Response, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("csrf", token); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := named(mw, f.path)
		part, err := mw.CreatePart(textproto.MIMEHeader{"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename=%q`, name)}, "Content-Type": {"application/octet-stream"}})
		if err != nil {
			t.Fatal(err)
		}
		part.Write([]byte(f.body))
	}
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, "http://"+b.addr.String()+address, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return b.do(req)
}

func wantServed(t *testing.T, addrs Addrs, host, path, want string) {
	t.Helper()
	res, body := get(t, addrs.HTTP, host, path)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, want) {
		t.Errorf("%s%s: %d %q, want %q", host, path, res.StatusCode, body, want)
	}
}

func wantNotServed(t *testing.T, addrs Addrs, host, path string) {
	t.Helper()
	if res, body := get(t, addrs.HTTP, host, path); res.StatusCode != http.StatusNotFound {
		t.Errorf("%s%s: %d %q, want it not found", host, path, res.StatusCode, body)
	}
}

// TestDropAFolder deploys a folder from the deploy page, in both ways that a browser sends one.
func TestDropAFolder(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	token := owner.token()

	// A folder, as a form chooses one: the folder itself is not a part of its site, and what is
	// hidden in it stays out of it.
	res, page := drop(t, owner, "/apps/dropped/deploy/site", token, []dropped{
		{"site/index.html", "<p>dropped home</p>"},
		{"site/css/main.css", "body { color: teal }"},
		{"site/.well-known/security.txt", "kept"},
		{"site/.env", "SECRET=1"},
		{"site/.env.local", "SECRET=2"},
		{"site/.git/config", "secret too"},
		{"site/sub/.DS_Store", "noise"},
	})
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/deployments/dep_") {
		t.Fatalf("%d %s\n%s", res.StatusCode, res.Header.Get("Location"), page)
	}
	id := strings.TrimPrefix(res.Header.Get("Location"), "/deployments/")
	wantServed(t, addrs, "dropped.localhost", "/", "dropped home")
	wantServed(t, addrs, "dropped.localhost", "/css/main.css", "teal")
	wantServed(t, addrs, "dropped.localhost", "/.well-known/security.txt", "kept")
	for _, path := range []string{"/.env", "/.env.local", "/.git/config", "/sub/.DS_Store", "/site/index.html"} {
		wantNotServed(t, addrs, "dropped.localhost", path)
	}
	log, err := os.ReadFile(deploy.LogPath(cfg.DataDir, "dropped", id))
	if err != nil || !strings.Contains(string(log), "left out") || !strings.Contains(string(log), "site/.env") {
		t.Errorf("%s %v", log, err)
	}
	if _, page := owner.get("/deployments/" + id); !strings.Contains(page, "Uploaded site") {
		t.Errorf("%s", page)
	}
	rels, err := c.Releases(ctx, "dropped", "production")
	if err != nil || len(rels) != 1 || rels[0].Files != 3 {
		t.Fatalf("%+v %v", rels, err)
	}

	// From the script, with several things at the top, to another environment.
	res, page = dropByScript(t, owner, "/apps/dropped/deploy/site?env=staging", token, []dropped{
		{"index.html", "<p>staged</p>"},
		{"deep/er/page.txt", "deep"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d\n%s", res.StatusCode, page)
	}
	wantServed(t, addrs, "dropped--staging.localhost", "/", "staged")
	wantServed(t, addrs, "dropped--staging.localhost", "/deep/er/page.txt", "deep")
	wantServed(t, addrs, "dropped.localhost", "/", "dropped home")

	// What is wrong with what is dropped is said, and changes nothing.
	for _, tc := range []struct {
		what  string
		files []dropped
		want  string
	}{
		{"nothing", nil, "no files"},
		{"only what is left out", []dropped{{".env", "x"}}, "no files"},
		{"leaving the folder", []dropped{{"../evil.html", "x"}}, "leaves the destination"},
		{"deeper, leaving it", []dropped{{"a/../../evil.html", "x"}}, "leaves the destination"},
		{"from the top", []dropped{{"/etc/evil.html", "x"}}, "absolute"},
		{"a backslash", []dropped{{`a\b.html`, "x"}}, "backslash"},
		{"the same file twice", []dropped{{"a.html", "1"}, {"a.html", "2"}}, "twice"},
		{"a file where a folder goes", []dropped{{"a", "1"}, {"a/b.html", "2"}}, "should be"},
		{"a folder where a file goes", []dropped{{"a/b.html", "1"}, {"a", "2"}}, "twice"},
		{"an app that runs", []dropped{{"moorage.toml", "[processes.web]\ncommand = \"./run\"\n"}, {"run", "x"}}, "source"},
		{"an app that builds", []dropped{{"moorage.toml", "[build]\ncommand = \"make\"\n"}, {"index.html", "x"}}, "source"},
		{"an app in a runtime", []dropped{{"moorage.toml", "[app]\nruntime = \"python\"\n"}, {"index.html", "x"}}, "source"},
		{"a manifest that cannot be read", []dropped{{"moorage.toml", "[[["}, {"index.html", "x"}}, "moorage.toml"},
		{"a directory outside it", []dropped{{"moorage.toml", "[static]\ndir = \"../..\"\n"}, {"index.html", "x"}}, "dir"},
		{"a directory that is not there", []dropped{{"moorage.toml", "[static]\ndir = \"public\"\n"}, {"index.html", "x"}}, "dir"},
	} {
		res, page := dropByScript(t, owner, "/apps/refused/deploy/site", token, tc.files)
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, tc.want) {
			t.Errorf("%s: %d, want saying %q\n%s", tc.what, res.StatusCode, tc.want, page)
		}
		if strings.Contains(page, "invalid upload") {
			t.Errorf("%s: says it like an error of the API:\n%s", tc.what, page)
		}
	}
	if _, err := c.Releases(ctx, "refused", "production"); status(err) != http.StatusNotFound {
		t.Errorf("the app was made all the same: %v", err)
	}
	entries, _ := filepath.Glob(filepath.Join(cfg.DataDir, "builds", "refused", "*"))
	if len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}

	// A moorage.toml at the top says how to serve it, and is not served.
	res, page = drop(t, owner, "/apps/rules/deploy/site", token, []dropped{
		{"out/moorage.toml", "[static]\ndir = \"dist\"\nspa = true\n"},
		{"out/README.md", "not this"},
		{"out/dist/index.html", "<p>the app itself</p>"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d\n%s", res.StatusCode, page)
	}
	wantServed(t, addrs, "rules.localhost", "/any/path", "the app itself")
	wantServed(t, addrs, "rules.localhost", "/", "the app itself")
	if _, body := get(t, addrs.HTTP, "rules.localhost", "/moorage.toml"); strings.Contains(body, "spa = true") {
		t.Errorf("%q", body)
	}
	if _, body := get(t, addrs.HTTP, "rules.localhost", "/README.md"); strings.Contains(body, "not this") {
		t.Errorf("%q", body)
	}

	// Without an index.html the address finds nothing, which the deployment says.
	res, _ = drop(t, owner, "/apps/headless/deploy/site", token, []dropped{{"a.txt", "a"}, {"b.txt", "b"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", res.StatusCode)
	}
	log, err = os.ReadFile(deploy.LogPath(cfg.DataDir, "headless", strings.TrimPrefix(res.Header.Get("Location"), "/deployments/")))
	if err != nil || !strings.Contains(string(log), "no index.html") {
		t.Errorf("%s %v", log, err)
	}

	// It takes the form's token, and leave to deploy, and an environment that is not locked.
	if res, _ := drop(t, owner, "/apps/dropped/deploy/site", "wrong", []dropped{{"index.html", "x"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a wrong token: %d", res.StatusCode)
	}
	if _, err := c.LockEnvironment(ctx, "dropped", "production"); err != nil {
		t.Fatal(err)
	}
	if res, page := drop(t, owner, "/apps/dropped/deploy/site", token, []dropped{{"index.html", "changed"}}); res.StatusCode != http.StatusConflict || !strings.Contains(page, "locked") {
		t.Errorf("locked: %d\n%s", res.StatusCode, page)
	}
	wantServed(t, addrs, "dropped.localhost", "/", "dropped home")

	sam := addTeammate(t, cfg, addrs, owner, "sam@example.com")
	if res, body := owner.post("/projects/default/settings", url.Values{"csrf": {token}, "everyone": {"viewer"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if res, _ := drop(t, sam, "/apps/dropped/deploy/site", sam.token(), []dropped{{"index.html", "from a viewer"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer: %d", res.StatusCode)
	}
}

func deployVersion(t *testing.T, c *client.Client, app, env, body string) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte(body), 0o644)
	if _, _, err := c.DeployStatic(context.Background(), app, env, dir, manifest.Static{}, nil); err != nil {
		t.Fatal(err)
	}
}

// TestReleasePermalinks serves each kept release of a static site at its own address.
func TestReleasePermalinks(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	for v := 1; v <= 3; v++ {
		deployVersion(t, c, "docs", "production", "<p>docs "+strconv.Itoa(v)+"</p>")
	}
	wantServed(t, addrs, "docs.localhost", "/", "docs 3")
	wantServed(t, addrs, "docs--production--v1.localhost", "/", "docs 1")
	wantServed(t, addrs, "docs--production--v2.localhost", "/", "docs 2")
	wantServed(t, addrs, "docs--production--v3.localhost", "/", "docs 3")

	// It is not for search engines, even of production.
	if res, _ := get(t, addrs.HTTP, "docs--production--v1.localhost", "/"); res.Header.Get("X-Robots-Tag") == "" {
		t.Errorf("%v", res.Header)
	}
	if res, _ := get(t, addrs.HTTP, "docs.localhost", "/"); res.Header.Get("X-Robots-Tag") != "" {
		t.Errorf("%v", res.Header)
	}

	// Other environments have their own, and the numbers are theirs.
	deployVersion(t, c, "docs", "staging", "<p>staged 1</p>")
	wantServed(t, addrs, "docs--staging--v1.localhost", "/", "staged 1")
	wantNotServed(t, addrs, "docs--staging--v2.localhost", "/")

	// What is not there says so, and something that looks like it but says it another way is not it.
	res, body := get(t, addrs.HTTP, "docs--production--v9.localhost", "/")
	if res.StatusCode != http.StatusNotFound || !strings.Contains(body, "v9") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	for _, host := range []string{"docs--production--v01.localhost", "docs--production--v0.localhost", "docs--production--v.localhost", "docs--v1.localhost", "nothing--production--v1.localhost"} {
		wantNotServed(t, addrs, host, "/")
	}

	// A rollback is a release of its own, of the same files.
	if _, err := c.Rollback(ctx, "docs", "production", 2); err != nil {
		t.Fatal(err)
	}
	wantServed(t, addrs, "docs--production--v4.localhost", "/", "docs 2")

	// They say so where releases are listed, with no address for what has none.
	rels, err := c.Releases(ctx, "docs", "production")
	if err != nil || len(rels) != 4 {
		t.Fatalf("%+v %v", rels, err)
	}
	for _, r := range rels {
		if want := cfg.ReleaseURL("docs", "production", r.Version); r.URL != want || want == "" {
			t.Errorf("v%d: %q, want %q", r.Version, r.URL, want)
		}
	}
	if _, page := owner.get("/apps/docs/deployments"); !strings.Contains(page, `--v2.localhost`) {
		t.Errorf("no link in:\n%s", page)
	}

	// The same gate as its environment, with a way through it at this address.
	setGate(t, c, "docs", "production", apitypes.AccessChange{Mode: "password", Password: "correct horse"})
	v := newVisitor(t, addrs, "docs--production--v1.localhost")
	res, page := v.get("/")
	if res.StatusCode == http.StatusOK && strings.Contains(page, "docs 1") {
		t.Fatalf("not gated: %d\n%s", res.StatusCode, page)
	}
	if res.StatusCode != http.StatusUnauthorized && res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusForbidden {
		t.Errorf("%d", res.StatusCode)
	}
	res, _ = v.do(http.MethodPost, "/.moorage/gate/password", url.Values{"password": {"correct horse"}, "return": {"/"}}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the password: %d", res.StatusCode)
	}
	if res, page := v.get("/"); res.StatusCode != http.StatusOK || !strings.Contains(page, "docs 1") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	setGate(t, c, "docs", "production", apitypes.AccessChange{Mode: "public"})
	wantServed(t, addrs, "docs--production--v1.localhost", "/", "docs 1")

	// An app that runs has no such addresses, unless a release of it was a static site.
	if _, err := deployApp(t, c, "docs", appPlan(nil)); err != nil {
		t.Fatal(err)
	}
	wantServed(t, addrs, "docs--production--v1.localhost", "/", "docs 1")
	wantNotServed(t, addrs, "docs--production--v5.localhost", "/")
	if rels, err = c.Releases(ctx, "docs", "production"); err != nil || rels[0].Version != 5 || rels[0].URL != "" || rels[1].URL == "" {
		t.Errorf("%+v %v", rels, err)
	}

	// Once a release is no longer kept, neither is its address.
	for v := 0; v < 6; v++ {
		deployVersion(t, c, "kept", "production", "<p>kept "+strconv.Itoa(v+1)+"</p>")
	}
	wantServed(t, addrs, "kept--production--v6.localhost", "/", "kept 6")
	var oldest apitypes.Release
	waitUntil(t, "the oldest release to go", func() bool {
		all, err := c.Releases(ctx, "kept", "production")
		if err != nil || len(all) == 0 {
			t.Fatalf("%+v %v", all, err)
		}
		oldest = all[len(all)-1]
		return oldest.URL == ""
	})
	wantNotServed(t, addrs, "kept--production--v1.localhost", "/")
}

// TestPermalinkLabels fits every environment into one label of DNS, however long its name.
func TestPermalinkLabels(t *testing.T) {
	long := strings.Repeat("e", 39)
	for _, tc := range []struct{ app, env string }{
		{"docs", "production"}, {"docs", ""}, {"docs", "staging"},
		{strings.Repeat("a", 40), "production"}, {strings.Repeat("a", 40), long}, {"docs", long},
	} {
		label := names.PermalinkLabel(tc.app, tc.env, 9999999)
		if len(label) > 63 {
			t.Errorf("%q is %d long", label, len(label))
		}
		base, version, ok := names.ParsePermalink(label)
		if !ok || version != 9999999 || base != names.PermalinkBase(tc.app, tc.env) {
			t.Errorf("%q: %q %d %v", label, base, version, ok)
		}
	}
	if a, b := names.PermalinkBase("a", strings.Repeat("e", 39)+"x"), names.PermalinkBase("a", strings.Repeat("e", 39)+"y"); len(a) > 53 && a == b {
		t.Errorf("%q", a)
	}
	if a, b := names.PermalinkBase(strings.Repeat("a", 40), strings.Repeat("e", 30)+"x"), names.PermalinkBase(strings.Repeat("a", 40), strings.Repeat("e", 30)+"y"); a == b {
		t.Errorf("two environments, one address: %q", a)
	}
	for _, label := range []string{"docs", "docs--production", "--v1", "docs--v", "docs--v0", "docs--v01", "docs--v12345678", "docs--vx", "docs--v-1", "docs--v1x"} {
		if base, version, ok := names.ParsePermalink(label); ok && base != "docs" {
			t.Errorf("%q: %q %d", label, base, version)
		} else if ok && label != "docs--v1" {
			t.Errorf("%q: %q %d", label, base, version)
		}
	}
}

// TestPathRoutingHasNoPermalinks: with no hostnames of their own, apps' releases have none either.
func TestPathRoutingHasNoPermalinks(t *testing.T) {
	cfg := testConfig(t)
	cfg.Routing = "path"
	cfg.ConsoleHost = "console.localhost"
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	deployVersion(t, c, "docs", "production", "<p>docs 1</p>")
	rels, err := c.Releases(context.Background(), "docs", "production")
	if err != nil || len(rels) != 1 || rels[0].URL != "" {
		t.Errorf("%+v %v", rels, err)
	}
	wantNotServed(t, addrs, "docs--production--v1.localhost", "/")
}
