package console

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/notify"
	"github.com/robert-mcdermott/moorage/internal/store"
)

const password = "a long and unusual passphrase"

type fixture struct {
	t     *testing.T
	svc   *auth.Service
	db    *store.DB
	url   string
	ctx   context.Context
	http  *http.Client
	extra http.Header
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}
	svc := &auth.Service{DB: db, LocalToken: auth.Hash("moor_local"), ClaimFile: filepath.Join(dir, "keys", "claim-code"), Limits: auth.NewLimiter()}
	cfg := config.Config{Domain: "apps.test", ConsoleHost: "moorage.apps.test", Routing: "host", DataDir: dir,
		Listen: config.Listen{HTTP: "127.0.0.1:80"}, TLS: config.TLS{Mode: "off"}, Proxy: config.Proxy{ExternalScheme: "http"}}
	pages, err := New(Options{Auth: svc, DB: db, Config: cfg, Log: slog.New(slog.DiscardHandler), Notifier: &notify.Service{DB: db, Log: slog.New(slog.DiscardHandler)}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(pages)
	t.Cleanup(ts.Close)
	f := &fixture{t: t, svc: svc, db: db, url: ts.URL, ctx: ctx}
	f.signOutClient()
	return f
}

// signOutClient starts with a browser that has no cookies.
func (f *fixture) signOutClient() {
	jar, _ := cookiejar.New(nil)
	f.http = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (f *fixture) get(path string) (*http.Response, string) {
	f.t.Helper()
	res, err := f.http.Get(f.url + path)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

func (f *fixture) post(path string, form url.Values) (*http.Response, string) {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.url+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range f.extra {
		req.Header[k] = v
	}
	res, err := f.http.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

func (f *fixture) addUser(email, role string) store.User {
	f.t.Helper()
	u, err := f.svc.NewUser(f.ctx, email, "", role, password)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.svc.DB.CreateUser(f.ctx, u); err != nil {
		f.t.Fatal(err)
	}
	return u
}

func (f *fixture) signIn(email string) {
	f.t.Helper()
	res, body := f.post("/login", url.Values{"email": {email}, "password": {password}})
	if res.StatusCode != http.StatusSeeOther {
		f.t.Fatalf("signing in: %d %s", res.StatusCode, body)
	}
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func csrfOf(t *testing.T, body string) string {
	t.Helper()
	m := csrfField.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no CSRF field in:\n%s", body)
	}
	return m[1]
}

func TestClaim(t *testing.T) {
	f := newFixture(t)
	code, err := f.svc.PrepareClaim(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, body := f.get("/"); !strings.Contains(body, "no accounts yet") {
		t.Errorf("the start page of an unclaimed server: %s", body)
	}
	if res, _ := f.get("/login"); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Errorf("signing in with no accounts: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	for _, path := range []string{"/setup", "/setup?code=wrong"} {
		if res, body := f.get(path); res.StatusCode != http.StatusForbidden || !strings.Contains(body, "not valid") {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	res, body := f.get("/setup?code=" + code)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Claim this server") {
		t.Fatalf("the setup page: %d", res.StatusCode)
	}

	form := url.Values{"code": {code}, "email": {"Admin@Example.com"}, "name": {"Ada"}, "password": {password}, "confirm": {"different"}}
	res, body = f.post("/setup", form)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "differ") || !strings.Contains(body, `value="Admin@Example.com"`) {
		t.Errorf("passwords that differ: %d", res.StatusCode)
	}
	if strings.Contains(body, "different") {
		t.Error("the page repeats a password")
	}
	form.Set("password", "short")
	form.Set("confirm", "short")
	if res, body = f.post("/setup", form); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "12 characters") {
		t.Errorf("a short password: %d", res.StatusCode)
	}
	form.Set("password", password)
	form.Set("confirm", password)
	form.Set("email", "nonsense")
	if res, _ = f.post("/setup", form); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a bad address: %d", res.StatusCode)
	}
	form.Set("email", "Admin@Example.com")
	form.Set("code", "wrong")
	if res, _ = f.post("/setup", form); res.StatusCode != http.StatusForbidden {
		t.Errorf("a wrong code: %d", res.StatusCode)
	}
	if n, _ := f.svc.DB.CountUsers(f.ctx); n != 0 {
		t.Fatalf("%d users after refusals", n)
	}

	form.Set("code", code)
	f.extra = http.Header{"Sec-Fetch-Site": {"cross-site"}}
	if res, _ = f.post("/setup", form); res.StatusCode != http.StatusForbidden {
		t.Errorf("from another site: %d", res.StatusCode)
	}
	f.extra = nil
	res, _ = f.post("/setup", form)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/welcome" {
		t.Fatalf("the claim: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	cookie := res.Cookies()[0]
	if cookie.Name != "moorage_session" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Errorf("the cookie: %+v", cookie)
	}
	u, err := f.svc.DB.UserByEmail(f.ctx, "admin@example.com")
	if err != nil || u.Role != store.RoleAdmin || u.Name != "Ada" {
		t.Errorf("the first user: %+v %v", u, err)
	}
	if _, body = f.get("/"); !strings.Contains(body, "admin@example.com") {
		t.Errorf("after the claim, the start page: %s", body)
	}

	// The link is spent.
	if _, err := os.Stat(f.svc.ClaimFile); !os.IsNotExist(err) {
		t.Errorf("the code remains: %v", err)
	}
	if res, _ = f.get("/setup?code=" + code); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
		t.Errorf("the link again: %d", res.StatusCode)
	}
	f.signOutClient()
	form.Set("email", "second@example.com")
	if res, _ = f.post("/setup", form); res.StatusCode != http.StatusForbidden {
		t.Errorf("claiming twice: %d", res.StatusCode)
	}
	if n, _ := f.svc.DB.CountUsers(f.ctx); n != 1 {
		t.Errorf("%d users", n)
	}
}

func TestClaimGuessingIsSlowed(t *testing.T) {
	f := newFixture(t)
	code, _ := f.svc.PrepareClaim(f.ctx)
	for range 6 {
		f.get("/setup?code=wrong")
	}
	res, body := f.get("/setup?code=" + code)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" || !strings.Contains(body, "Too many") {
		t.Errorf("after guessing: %d %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
}

func TestSignInAndOut(t *testing.T) {
	f := newFixture(t)
	f.addUser("a@example.com", store.RoleMember)
	if res, _ := f.get("/"); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
		t.Errorf("the start page, signed out: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	res, body := f.post("/login", url.Values{"email": {"a@example.com"}, "password": {"wrong"}})
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "wrong") || !strings.Contains(body, `value="a@example.com"`) {
		t.Errorf("a wrong password: %d", res.StatusCode)
	}
	res, unknown := f.post("/login", url.Values{"email": {"nobody@example.com"}, "password": {"wrong"}})
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(unknown, "The email address or password is wrong") {
		t.Errorf("an unknown address: %d", res.StatusCode)
	}

	// After signing in, only paths on this site are followed.
	for next, want := range map[string]string{
		"/device?code=X":        "/device?code=X",
		"//evil.example.com/":   "/",
		"https://evil.example/": "/",
		`/\evil.example.com`:    "/",
		"":                      "/",
	} {
		f.signOutClient()
		res, _ := f.post("/login", url.Values{"email": {"a@example.com"}, "password": {password}, "next": {next}})
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != want {
			t.Errorf("next %q: %d %q", next, res.StatusCode, res.Header.Get("Location"))
		}
	}
	// Signed in already, the page goes on.
	if res, _ := f.get("/login?next=/device"); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/device" {
		t.Errorf("signed in already: %d", res.StatusCode)
	}

	_, body = f.get("/")
	if !strings.Contains(body, "Signed in as") {
		t.Fatalf("signed in: %s", body)
	}
	if res, _ := f.post("/logout", url.Values{"csrf": {"wrong"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("signing out without the token: %d", res.StatusCode)
	}
	if _, body := f.get("/"); !strings.Contains(body, "Signed in as") {
		t.Error("that signed out")
	}
	res, _ = f.post("/logout", url.Values{"csrf": {csrfOf(t, body)}})
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("signing out: %d", res.StatusCode)
	}
	if _, body := f.get("/"); strings.Contains(body, "Signed in as") {
		t.Error("still signed in")
	}
}

func TestSignInLimit(t *testing.T) {
	f := newFixture(t)
	f.addUser("a@example.com", store.RoleMember)
	for range 6 {
		f.post("/login", url.Values{"email": {"a@example.com"}, "password": {"wrong"}})
	}
	res, body := f.post("/login", url.Values{"email": {"a@example.com"}, "password": {password}})
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" || !strings.Contains(body, "Too many") {
		t.Errorf("after failures: %d", res.StatusCode)
	}
}

func TestInvitation(t *testing.T) {
	f := newFixture(t)
	u, _ := f.svc.NewUser(f.ctx, "new@example.com", "", store.RoleMember, "")
	if err := f.svc.DB.CreateUser(f.ctx, u); err != nil {
		t.Fatal(err)
	}
	code, err := f.svc.Invite(f.ctx, u.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := f.get("/invite?code=wrong"); res.StatusCode != http.StatusNotFound {
		t.Errorf("a wrong link: %d", res.StatusCode)
	}
	if res, body := f.get("/invite?code=" + code); res.StatusCode != http.StatusOK || !strings.Contains(body, "new@example.com") {
		t.Errorf("the page: %d", res.StatusCode)
	}
	form := url.Values{"code": {code}, "password": {password}, "confirm": {"other"}}
	if res, _ := f.post("/invite", form); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("passwords that differ: %d", res.StatusCode)
	}
	form.Set("password", "password1234")
	form.Set("confirm", "password1234")
	if res, body := f.post("/invite", form); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "most common") {
		t.Errorf("a common password: %d", res.StatusCode)
	}
	form.Set("password", password)
	form.Set("confirm", password)
	if res, _ := f.post("/invite", form); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("accepting: %d", res.StatusCode)
	}
	if _, body := f.get("/"); !strings.Contains(body, "new@example.com") {
		t.Errorf("signed in afterwards: %s", body)
	}
	if res, _ := f.get("/invite?code=" + code); res.StatusCode != http.StatusNotFound {
		t.Errorf("the link again: %d", res.StatusCode)
	}
	if res, _ := f.post("/invite", form); res.StatusCode != http.StatusNotFound {
		t.Errorf("using it again: %d", res.StatusCode)
	}
}

func TestDeviceApproval(t *testing.T) {
	f := newFixture(t)
	u := f.addUser("a@example.com", store.RoleMember)
	code, userCode, err := f.svc.StartDevice(f.ctx, "moor on <script>alert(1)</script>", "192.0.2.5")
	if err != nil {
		t.Fatal(err)
	}

	// Signed out, the page asks for a sign-in, then comes back.
	res, _ := f.get("/device?code=" + userCode)
	want := "/login?next=" + url.QueryEscape("/device?code="+userCode)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != want {
		t.Fatalf("signed out: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if _, body := f.get(want); !strings.Contains(body, userCode) {
		t.Errorf("the sign-in page lost where to go next: %s", body)
	}
	f.signIn("a@example.com")

	if res, body := f.get("/device"); res.StatusCode != http.StatusOK || !strings.Contains(body, "Enter the code") {
		t.Errorf("no code: %d", res.StatusCode)
	}
	if res, body := f.get("/device?code=WRONG-CODE"); res.StatusCode != http.StatusNotFound || !strings.Contains(body, "not valid") {
		t.Errorf("a wrong code: %d", res.StatusCode)
	}
	// A code may be typed in lower case, without its dash.
	typed := strings.ToLower(strings.ReplaceAll(userCode, "-", ""))
	res, body := f.get("/device?code=" + typed)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, userCode) || !strings.Contains(body, "192.0.2.5") || !strings.Contains(body, "a@example.com") {
		t.Fatalf("the confirmation: %d %s", res.StatusCode, body)
	}
	if strings.Contains(body, "<script>") {
		t.Error("the client's name is not escaped")
	}
	csrf := csrfOf(t, body)

	// Nothing is decided without the token.
	form := url.Values{"csrf": {"wrong"}, "code": {userCode}, "decision": {"approve"}}
	if res, _ := f.post("/device", form); res.StatusCode != http.StatusForbidden {
		t.Errorf("a wrong CSRF token: %d", res.StatusCode)
	}
	if _, _, err := f.svc.PollDevice(f.ctx, code, "x"); !errors.Is(err, auth.ErrDevicePending) && !errors.Is(err, auth.ErrDeviceSlowDown) {
		t.Errorf("after a refused approval: %v", err)
	}

	form.Set("csrf", csrf)
	if res, body := f.post("/device", form); res.StatusCode != http.StatusOK || !strings.Contains(body, "go back to your terminal") {
		t.Fatalf("approving: %d %s", res.StatusCode, body)
	}
	token, got, err := f.svc.PollDevice(f.ctx, code, "moor login")
	if err != nil || got.ID != u.ID {
		t.Fatalf("the token: %q %+v %v", token, got, err)
	}
	if res, _ := f.post("/device", form); res.StatusCode != http.StatusNotFound {
		t.Errorf("approving again: %d", res.StatusCode)
	}

	// Denying.
	code, userCode, _ = f.svc.StartDevice(f.ctx, "moor", "192.0.2.5")
	form = url.Values{"csrf": {csrf}, "code": {userCode}, "decision": {"deny"}}
	if res, body := f.post("/device", form); res.StatusCode != http.StatusOK || !strings.Contains(body, "refused") {
		t.Errorf("denying: %d", res.StatusCode)
	}
	if _, _, err := f.svc.PollDevice(f.ctx, code, "x"); !errors.Is(err, auth.ErrDeviceDenied) {
		t.Errorf("after denying: %v", err)
	}

	// Signed out, a decision goes nowhere.
	code, userCode, _ = f.svc.StartDevice(f.ctx, "moor", "192.0.2.5")
	f.signOutClient()
	form = url.Values{"csrf": {csrf}, "code": {userCode}, "decision": {"approve"}}
	if res, _ := f.post("/device", form); res.StatusCode != http.StatusSeeOther {
		t.Errorf("signed out: %d", res.StatusCode)
	}
	if _, _, err := f.svc.PollDevice(f.ctx, code, "x"); !errors.Is(err, auth.ErrDevicePending) {
		t.Errorf("after a signed-out approval: %v", err)
	}
}

func TestHeaders(t *testing.T) {
	f := newFixture(t)
	f.addUser("a@example.com", store.RoleMember)
	for _, path := range []string{"/login", "/nope"} {
		res, body := f.get(path)
		h := res.Header
		if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") || !strings.Contains(h.Get("Content-Security-Policy"), "form-action 'self'") ||
			h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %v", path, h)
		}
		if !strings.Contains(body, `rel="icon"`) || !strings.Contains(body, "<svg") {
			t.Errorf("%s has no icon", path)
		}
	}
	if res, _ := f.get("/nope"); res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown path: %d", res.StatusCode)
	}
}
