package console

import (
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/backup"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/notify"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func (f *fixture) event(kind, app string, data map[string]any) store.Event {
	f.t.Helper()
	e := store.Event{ID: ids.New(ids.Event), At: time.Now(), Type: kind, App: app, Env: "production", Actor: "a@example.com", Data: data}
	if err := f.db.AddEvent(f.ctx, e); err != nil {
		f.t.Fatal(err)
	}
	return e
}

func (f *fixture) signedInAdmin() {
	f.t.Helper()
	f.addUser("a@example.com", store.RoleAdmin)
	f.signIn("a@example.com")
}

func TestPagesNeedSigningIn(t *testing.T) {
	f := newFixture(t)
	f.addUser("a@example.com", store.RoleMember)
	for path, next := range map[string]string{"/activity?app=shop": "%2Factivity%3Fapp%3Dshop", "/welcome": "%2Fwelcome"} {
		res, body := f.get(path)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login?next="+next {
			t.Errorf("%s: %d %s", path, res.StatusCode, res.Header.Get("Location"))
		}
		if strings.Contains(body, "Signed in as") {
			t.Errorf("%s: %s", path, body)
		}
	}
}

// TestPagesAreStrict checks the policy that keeps a page's own content from running
// as code: every page keeps to it, so no page has an inline script or style.
func TestPagesAreStrict(t *testing.T) {
	f := newFixture(t)
	f.signedInAdmin()
	f.db.EnsureApp(f.ctx, "shop")
	for _, path := range []string{"/", "/activity", "/welcome", "/new", "/apps/shop", "/apps/shop/deployments", "/apps/shop/deploy", "/apps/brand-new/deploy", "/apps/shop/logs", "/notifications", "/settings/notifications", "/settings/backups", "/apps/shop/variables", "/apps/shop/variables/edit", "/apps/shop/settings", "/account", "/account/keys", "/account/tokens", "/account/sessions", "/settings", "/settings/users", "/settings/registries", "/settings/system", "/settings/audit", "/projects", "/projects/default", "/previews", "/apps/shop/environments", "/apps/shop/access", "/apps/shop/cron", "/apps/shop/metrics"} {
		res, body := f.get(path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
		csp := res.Header.Get("Content-Security-Policy")
		for _, want := range []string{"script-src 'self'", "style-src 'self'", "frame-ancestors 'none'", "default-src 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: no %q in %q", path, want, csp)
			}
		}
		if strings.Contains(csp, "unsafe") {
			t.Errorf("%s: %q", path, csp)
		}
		if strings.Contains(body, "<style") || regexp.MustCompile(`\sstyle="`).MatchString(body) {
			t.Errorf("%s has an inline style", path)
		}
		for _, tag := range regexp.MustCompile(`<script[^>]*>`).FindAllString(body, -1) {
			if !strings.Contains(tag, "src=") && !strings.Contains(tag, `type="application/json"`) {
				t.Errorf("%s has an inline script: %s", path, tag)
			}
		}
		if regexp.MustCompile(`\son[a-z]+="`).MatchString(body) {
			t.Errorf("%s has an inline event handler", path)
		}
	}
}

func TestAssets(t *testing.T) {
	f := newFixture(t)
	f.addUser("a@example.com", store.RoleMember)
	_, body := f.get("/login")
	found := regexp.MustCompile(`/assets/[a-z]+\.[0-9a-f]{10}\.(css|js)`).FindAllString(body, -1)
	if len(found) < 2 {
		t.Fatalf("no fingerprinted assets in: %s", body)
	}
	for _, path := range found {
		res, content := f.get(path)
		if res.StatusCode != http.StatusOK || content == "" {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
		if res.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %v", path, res.Header)
		}
		ct := res.Header.Get("Content-Type")
		if strings.HasSuffix(path, ".css") && !strings.HasPrefix(ct, "text/css") {
			t.Errorf("%s: %s", path, ct)
		}
		if strings.HasSuffix(path, ".js") && !strings.Contains(ct, "javascript") {
			t.Errorf("%s: %s", path, ct)
		}
	}
	for _, path := range []string{"/assets/console.css", "/assets/console.0000000000.css", "/assets/nothing.js"} {
		if res, _ := f.get(path); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
}

func TestAppsPage(t *testing.T) {
	f := newFixture(t)
	f.signedInAdmin()
	_, body := f.get("/")
	if !strings.Contains(body, "No apps yet") || !strings.Contains(body, "moor deploy") || !strings.Contains(body, `aria-current="page"`) {
		t.Errorf("no apps: %s", body)
	}
	if _, _, err := f.db.EnsureApp(f.ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	_, body = f.get("/")
	for _, want := range []string{`data-app-row="shop"`, "http://shop.apps.test", "Not deployed", "a@example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in: %s", want, body)
		}
	}
	if strings.Contains(body, "No apps yet") {
		t.Errorf("%s", body)
	}
}

func TestPaletteOffersTheAppsAndActions(t *testing.T) {
	f := newFixture(t)
	f.signedInAdmin()
	f.db.EnsureApp(f.ctx, "shop")
	_, body := f.get("/activity")
	m := regexp.MustCompile(`(?s)<script type="application/json" id="palette-data">(.*?)</script>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no palette data in: %s", body)
	}
	var items []paletteItem
	if err := json.Unmarshal([]byte(m[1]), &items); err != nil {
		t.Fatalf("%v: %s", err, m[1])
	}
	want := map[string]bool{"Apps": false, "Activity": false, "Getting started": false, "shop": false, "Open shop": false, "Sign out": false, "Use the dark theme": false}
	for _, item := range items {
		if _, ok := want[item.Label]; ok {
			want[item.Label] = true
		}
		if item.Label == "Open shop" && (item.Href != "http://shop.apps.test" || !item.External) {
			t.Errorf("%+v", item)
		}
		if item.Label == "shop" && item.Href != "/apps/shop" {
			t.Errorf("%+v", item)
		}
		if item.Label == "Sign out" && item.Action != "signout" {
			t.Errorf("%+v", item)
		}
	}
	for label, found := range want {
		if !found {
			t.Errorf("no %q in %+v", label, items)
		}
	}
	// The form the palette's action submits is on the page.
	if !strings.Contains(body, `id="signout"`) {
		t.Errorf("no form to sign out with: %s", body)
	}
}

func TestActivity(t *testing.T) {
	f := newFixture(t)
	f.signedInAdmin()
	f.db.EnsureApp(f.ctx, "shop")
	f.db.EnsureApp(f.ctx, "blog")
	_, body := f.get("/activity")
	if !strings.Contains(body, "Nothing has happened yet") {
		t.Errorf("%s", body)
	}

	first := f.event(events.DeploymentLive, "shop", map[string]any{"version": 3})
	f.event(events.DeploymentFailed, "blog", map[string]any{"error": "the <build> failed"})
	f.event(events.ServerStarted, "", map[string]any{"version": "0.3.0"})
	_, body = f.get("/activity")
	for _, want := range []string{
		"Deployed v3 by a@example.com", "The server started, version 0.3.0",
		"the &lt;build&gt; failed", `class="dot fail"`, `class="dot ok"`,
		`data-timeline`, `data-id="` + first.ID + `"`, `<option value="shop">`,
		`data-stream="/api/v1/events/stream?after=`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in: %s", want, body)
		}
	}
	if strings.Contains(body, "the <build>") {
		t.Error("an event's text was not escaped")
	}
	// Newest first.
	if strings.Index(body, "The server started") > strings.Index(body, "Deployed v3") {
		t.Error("oldest first")
	}

	_, body = f.get("/activity?app=shop")
	if !strings.Contains(body, "Deployed v3") || strings.Contains(body, "failed") || !strings.Contains(body, `<option value="shop" selected>`) || !strings.Contains(body, `data-app="shop"`) {
		t.Errorf("for one app: %s", body)
	}
}

func TestActivityPages(t *testing.T) {
	f := newFixture(t)
	f.signedInAdmin()
	f.db.EnsureApp(f.ctx, "shop")
	for i := 1; i <= activityPage+5; i++ {
		f.event(events.DeploymentLive, "shop", map[string]any{"version": i})
	}
	_, body := f.get("/activity?app=shop")
	if n := strings.Count(body, "<li data-id="); n != activityPage {
		t.Errorf("the first page has %d", n)
	}
	m := regexp.MustCompile(`href="(/activity\?[^"]*before=[^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no way to older ones: %s", body)
	}
	older := html.UnescapeString(m[1])
	if u, err := url.Parse(older); err != nil || u.Query().Get("app") != "shop" {
		t.Errorf("%s: %v", older, err)
	}
	_, body = f.get(older)
	if n := strings.Count(body, "<li data-id="); n != 5 {
		t.Errorf("the second page has %d", n)
	}
	if strings.Contains(body, "before=") {
		t.Error("a way on from the last page")
	}
	// An older page does not grow by itself: new events are not of that page.
	if strings.Contains(body, "data-timeline") {
		t.Error("an older page takes live events")
	}
}

func TestGettingStarted(t *testing.T) {
	f := newFixture(t)
	f.signedInAdmin()
	_, body := f.get("/welcome")
	for _, want := range []string{
		"signed in as", "Check the server", "does not resolve", html.EscapeString(installUnix), html.EscapeString(installWindows),
		"moor login http://moorage.apps.test", "git push moorage main", "Your first deployment",
		"http://&lt;app&gt;.apps.test",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in: %s", want, body)
		}
	}
	if !strings.Contains(body, "data-waiting") {
		t.Errorf("%s", body)
	}

	// A member cannot change the server, and is not shown its insides.
	f.signOutClient()
	f.addUser("m@example.com", store.RoleMember)
	f.signIn("m@example.com")
	_, body = f.get("/welcome")
	if strings.Contains(body, "Check the server") || !strings.Contains(body, "moor deploy") {
		t.Errorf("for a member: %s", body)
	}
}

// TestDevelopersReadTheSourceTree is moor server run --dev: a change shows on the
// next request, with nothing to rebuild.
func TestDevelopersReadTheSourceTree(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"templates", "assets"} {
		entries, err := embedded.ReadDir(sub)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			data, _ := embedded.ReadFile(sub + "/" + e.Name())
			if err := os.WriteFile(filepath.Join(dir, sub, e.Name()), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	f := newFixture(t)
	f.addUser("a@example.com", store.RoleMember)
	pages, err := New(Options{Auth: f.svc, DB: f.db, Log: slog.New(slog.DiscardHandler), Dir: dir, Notifier: &notify.Service{DB: f.db, Log: slog.New(slog.DiscardHandler)}, Backups: &backup.Service{DB: f.db, DataDir: dir, Log: slog.New(slog.DiscardHandler)}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(pages)
	defer ts.Close()
	get := func(path string) string {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return string(data)
	}
	if body := get("/login"); !strings.Contains(body, `href="/assets/console.css"`) {
		t.Errorf("%s", body)
	}
	account := filepath.Join(dir, "templates", "account.html")
	data, _ := os.ReadFile(account)
	if err := os.WriteFile(account, []byte(strings.Replace(string(data), "<h1>Sign in</h1>", "<h1>Changed just now</h1>", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if body := get("/login"); !strings.Contains(body, "Changed just now") {
		t.Errorf("the change does not show: %s", body)
	}
	css := filepath.Join(dir, "assets", "console.css")
	if err := os.WriteFile(css, []byte("/* changed */"), 0o644); err != nil {
		t.Fatal(err)
	}
	if body := get("/assets/console.css"); body != "/* changed */" {
		t.Errorf("%q", body)
	}
}

func TestAppPagesAreGuarded(t *testing.T) {
	f := newFixture(t)
	f.db.EnsureApp(f.ctx, "shop")
	f.addUser("a@example.com", store.RoleMember)

	// Signed out: to the sign-in, and back.
	for _, path := range []string{"/new", "/apps/shop", "/apps/shop/deployments", "/apps/shop/deploy", "/deployments/dep_01M6JAE1CDN5QDNB0X4803DRW3"} {
		res, _ := f.get(path)
		if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/login?next=") {
			t.Errorf("%s: %d %s", path, res.StatusCode, res.Header.Get("Location"))
		}
	}
	for _, path := range []string{"/apps/shop/restart", "/apps/shop/redeploy", "/apps/shop/rollback", "/apps/shop/scale", "/apps/shop/deploy/image"} {
		res, _ := f.post(path, url.Values{})
		if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/login") {
			t.Errorf("%s: %d %s", path, res.StatusCode, res.Header.Get("Location"))
		}
	}

	f.signIn("a@example.com")
	_, page := f.get("/apps/shop/deploy")
	token := csrfOf(t, page)

	// Signed in, but without the session's token, or from another site: refused.
	for _, path := range []string{"/apps/shop/restart", "/apps/shop/redeploy", "/apps/shop/rollback", "/apps/shop/scale", "/apps/shop/deploy/image"} {
		res, body := f.post(path, url.Values{"csrf": {"wrong"}, "image": {"nginx:1"}})
		if res.StatusCode != http.StatusForbidden || !strings.Contains(body, "refused") {
			t.Errorf("%s with a wrong token: %d", path, res.StatusCode)
		}
	}
	f.extra = http.Header{"Sec-Fetch-Site": {"cross-site"}}
	for _, path := range []string{"/apps/shop/restart", "/apps/shop/deploy/image", "/apps/shop/deploy/archive"} {
		res, _ := f.post(path, url.Values{"csrf": {token}, "image": {"nginx:1"}})
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s from another site: %d", path, res.StatusCode)
		}
	}
	f.extra = nil

	// An app that does not exist has no actions to take; a name that could not be
	// one has no page.
	for _, path := range []string{"/apps/none/restart", "/apps/Not_A_Name/restart"} {
		if res, _ := f.post(path, url.Values{"csrf": {token}}); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	for _, path := range []string{"/apps/none", "/apps/none/deployments", "/apps/Not_A_Name/deploy", "/deployments/nothing", "/deployments/dep_01M6JAE1CDN5QDNB0X4803DRW3"} {
		if res, _ := f.get(path); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
}

func TestNewApp(t *testing.T) {
	f := newFixture(t)
	f.signedInAdmin()
	res, body := f.get("/new")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `name="name"`) || !strings.Contains(body, `<option value="default"`) {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	token := csrfOf(t, body)
	res, body = f.post("/new", url.Values{"csrf": {token}, "name": {"Not_A_Name"}, "project": {"default"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `role="alert"`) || !strings.Contains(body, `value="Not_A_Name"`) {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if res, _ = f.post("/new", url.Values{"csrf": {token}, "name": {"brand-new"}, "project": {"nothing"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("no such project: %d", res.StatusCode)
	}
	res, _ = f.post("/new", url.Values{"csrf": {token}, "name": {"brand-new"}, "project": {"default"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/apps/brand-new/deploy" {
		t.Errorf("%d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ = f.post("/new", url.Values{"csrf": {token}, "name": {"brand-new"}}); res.StatusCode != http.StatusConflict {
		t.Errorf("the same name: %d", res.StatusCode)
	}
	_, body = f.get("/apps/brand-new/deploy")
	for _, want := range []string{"moor deploy --app brand-new", "moor git remote --app brand-new", "git push moorage main", `data-wait-deployment="brand-new"`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in: %s", want, body)
		}
	}
	if res, body := f.get("/apps/brand-new"); res.StatusCode != http.StatusOK || !strings.Contains(body, "Nothing is deployed yet") {
		t.Errorf("%d", res.StatusCode)
	}
	// An app that does not exist yet can still be told how to come about.
	if res, _ := f.get("/apps/not-yet/deploy"); res.StatusCode != http.StatusOK {
		t.Errorf("%d", res.StatusCode)
	}
}
