package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/robert-mcdermott/moorage/internal/config"
)

// fakeProvider is an OpenID Connect provider, which signs in whoever the test says.
type fakeProvider struct {
	// issuerEnd is put at the end of the address it calls itself by, and userInfo is what it says when asked.
	issuerEnd string
	userInfo  map[string]any
	t         *testing.T
	srv       *httptest.Server
	key       *rsa.PrivateKey
	mu        sync.Mutex
	claims    map[string]any
	// What it heard, for the test to look at, and what a test wants done wrong.
	challenge, nonce string
	mutate           func(jwt.Claims, map[string]any) (jwt.Claims, map[string]any)
	signWith         *rsa.PrivateKey
	refuse           string
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{t: t, key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.srv.URL + p.issuerEnd, "authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token", "jwks_uri": p.srv.URL + "/keys",
			"userinfo_endpoint":        p.srv.URL + "/userinfo",
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.userInfo == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p.userInfo)
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: key.Public(), KeyID: "one", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p.mu.Lock()
		p.challenge, p.nonce = q.Get("code_challenge"), q.Get("nonce")
		refuse := p.refuse
		p.mu.Unlock()
		if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "moorage-client" || q.Get("response_type") != "code" || !strings.Contains(q.Get("scope"), "openid") {
			t.Errorf("the request: %s", r.URL)
		}
		back := q.Get("redirect_uri") + "?state=" + url.QueryEscape(q.Get("state"))
		if refuse != "" {
			back += "&error=" + refuse
		} else {
			back += "&code=the-code"
		}
		http.Redirect(w, r, back, http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		user, pass, _ := r.BasicAuth()
		if user == "" {
			user, pass = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
		}
		if user != "moorage-client" || pass != "client-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		p.mu.Lock()
		defer p.mu.Unlock()
		if base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge || r.PostFormValue("code") != "the-code" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		standard := jwt.Claims{Issuer: p.srv.URL + p.issuerEnd, Subject: p.claims["sub"].(string), Audience: jwt.Audience{"moorage-client"}, Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour)), IssuedAt: jwt.NewNumericDate(time.Now())}
		extra := map[string]any{"nonce": p.nonce}
		for k, v := range p.claims {
			if k != "sub" {
				extra[k] = v
			}
		}
		if p.mutate != nil {
			standard, extra = p.mutate(standard, extra)
		}
		signing := p.key
		if p.signWith != nil {
			signing = p.signWith
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: signing, KeyID: "one"}}, (&jose.SignerOptions{}).WithType("JWT"))
		if err != nil {
			t.Error(err)
			return
		}
		token, err := jwt.Signed(signer).Claims(standard).Claims(extra).Serialize()
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": token})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProvider) signsIn(claims map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claims, p.mutate, p.signWith, p.refuse = claims, nil, nil, ""
}

// ssoBrowser goes through signing in as a browser does, by its redirects, and says where it ended up.
type ssoBrowser struct {
	*consoleUser
}

func newSSOBrowser(t *testing.T, cfg config.Config, addrs Addrs) *ssoBrowser {
	return &ssoBrowser{&consoleUser{t: t, addr: addrs.HTTP, host: cfg.ConsoleHost}}
}

// signIn starts at the console, goes to the provider and comes back. It answers with what the console
// finally said.
func (b *ssoBrowser) signIn(next string) (*http.Response, string) {
	b.t.Helper()
	res, page := b.get("/login/sso?next=" + url.QueryEscape(next))
	if res.StatusCode != http.StatusSeeOther {
		return res, page
	}
	return b.followProvider(res.Header.Get("Location"))
}

func (b *ssoBrowser) followProvider(to string) (*http.Response, string) {
	b.t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	at, err := client.Get(to)
	if err != nil {
		b.t.Fatal(err)
	}
	at.Body.Close()
	back, err := url.Parse(at.Header.Get("Location"))
	if err != nil {
		b.t.Fatal(err)
	}
	return b.get(back.RequestURI())
}

func setUpSSO(t *testing.T, owner *consoleUser, p *fakeProvider, more url.Values) {
	t.Helper()
	form := url.Values{"csrf": {owner.token()}, "issuer": {p.srv.URL}, "client_id": {"moorage-client"}, "client_secret": {"client-secret"}, "label": {"Acme login"}}
	for k, v := range more {
		form[k] = v
	}
	if res, page := owner.post("/settings/sign-in", form); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d\n%s", res.StatusCode, page)
	}
}

// TestSingleSignOn signs in through a provider, and tries what should not work.
func TestSingleSignOn(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	p := newFakeProvider(t)
	db := testAuth(t, cfg).DB

	// Nothing to offer until it is set up.
	anon := newSSOBrowser(t, cfg, addrs)
	if _, page := anon.get("/login"); strings.Contains(page, "/login/sso") {
		t.Errorf("%s", page)
	}
	if res, _ := anon.get("/login/sso"); res.StatusCode != http.StatusForbidden {
		t.Errorf("%d", res.StatusCode)
	}

	// Setting it up finds the provider first, and says what to tell it.
	for _, tc := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"issuer": {"http://login.example.com"}, "client_id": {"x"}}, "https"},
		{url.Values{"issuer": {""}, "client_id": {"x"}}, "address"},
		{url.Values{"issuer": {"http://127.0.0.1:1"}, "client_id": {"x"}}, "cannot be reached"},
		{url.Values{"issuer": {p.srv.URL}, "client_id": {"x"}, "group_rules": {"nonsense"}}, "Line 1"},
	} {
		tc.form.Set("csrf", owner.token())
		if res, page := owner.post("/settings/sign-in", tc.form); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, tc.want) {
			t.Errorf("%v: %d, want saying %q\n%s", tc.form, res.StatusCode, tc.want, page)
		}
	}
	setUpSSO(t, owner, p, url.Values{"provision": {"1"}, "email_domains": {"example.com"}})
	if _, page := owner.get("/settings/sign-in"); !strings.Contains(page, "/login/sso/callback") || strings.Contains(page, "client-secret") {
		t.Errorf("%s", page)
	}
	var sealed []byte
	if err := db.Reader.QueryRowContext(ctx, `SELECT sealed_secret FROM sso_config`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "client-secret") {
		t.Error("the client secret is kept in the clear")
	}
	if _, page := anon.get("/login"); !strings.Contains(page, "Sign in with Acme login") || !strings.Contains(page, `name="password"`) {
		t.Errorf("%s", page)
	}

	// Someone new gets an account, and goes where they were going.
	p.signsIn(map[string]any{"sub": "subject-ada", "email": "Ada@Example.com", "email_verified": true, "name": "Ada"})
	ada := newSSOBrowser(t, cfg, addrs)
	res, page := ada.signIn("/account")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/account" {
		t.Fatalf("%d %s\n%s", res.StatusCode, res.Header.Get("Location"), page)
	}
	if res, page := ada.get("/account"); res.StatusCode != http.StatusOK || !strings.Contains(page, "ada@example.com") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	u, err := db.UserByEmail(ctx, "ada@example.com")
	if err != nil || u.Name != "Ada" || u.Role != "member" || u.PasswordHash != "" {
		t.Errorf("%+v %v", u, err)
	}
	// Their subject is who they are, whatever address the provider has for them later.
	p.signsIn(map[string]any{"sub": "subject-ada", "email": "ada.lovelace@elsewhere.org", "email_verified": true})
	if res, page := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("after the address changed: %d\n%s", res.StatusCode, page)
	}

	// An account that is already there joins up by its address, if the provider vouches for it.
	p.signsIn(map[string]any{"sub": "subject-owner", "email": "owner@example.com", "email_verified": false})
	if res, page := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusForbidden || !strings.Contains(page, "not verified") {
		t.Errorf("an unverified address taking over an account: %d\n%s", res.StatusCode, page)
	}
	p.signsIn(map[string]any{"sub": "subject-owner", "email": "owner@example.com", "email_verified": true})
	again := newSSOBrowser(t, cfg, addrs)
	if res, page := again.signIn("/settings"); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/settings" {
		t.Errorf("%d %s\n%s", res.StatusCode, res.Header.Get("Location"), page)
	}
	if res, _ := again.get("/settings"); res.StatusCode != http.StatusOK {
		t.Errorf("the owner's account: %d", res.StatusCode)
	}

	// Whose addresses, and accounts that are closed.
	p.signsIn(map[string]any{"sub": "subject-eve", "email": "eve@evil.test", "email_verified": true})
	if res, page := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusForbidden || !strings.Contains(page, "not one that may use") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	p.signsIn(map[string]any{"sub": "subject-nobody", "email_verified": true})
	if res, page := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusForbidden || !strings.Contains(page, "email address") {
		t.Errorf("no address: %d\n%s", res.StatusCode, page)
	}
	if err := db.SetUserDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	p.signsIn(map[string]any{"sub": "subject-ada", "email": "ada@example.com", "email_verified": true})
	if res, page := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusForbidden || !strings.Contains(page, "disabled") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}

	// A next that goes elsewhere stays here.
	p.signsIn(map[string]any{"sub": "subject-owner", "email": "owner@example.com", "email_verified": true})
	if res, _ := newSSOBrowser(t, cfg, addrs).signIn("https://evil.test/"); res.Header.Get("Location") != "/" {
		t.Errorf("%s", res.Header.Get("Location"))
	}

	// What a provider sends is not taken on trust.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, wrong := range map[string]func(){
		"signed with another key": func() { p.signWith = other },
		"for another client": func() {
			p.mutate = func(s jwt.Claims, e map[string]any) (jwt.Claims, map[string]any) {
				s.Audience = jwt.Audience{"another-client"}
				return s, e
			}
		},
		"from another issuer": func() {
			p.mutate = func(s jwt.Claims, e map[string]any) (jwt.Claims, map[string]any) {
				s.Issuer = "https://evil.test"
				return s, e
			}
		},
		"out of date": func() {
			p.mutate = func(s jwt.Claims, e map[string]any) (jwt.Claims, map[string]any) {
				s.Expiry = jwt.NewNumericDate(time.Now().Add(-time.Hour))
				return s, e
			}
		},
		"for another sign-in": func() {
			p.mutate = func(s jwt.Claims, e map[string]any) (jwt.Claims, map[string]any) {
				e["nonce"] = "another-nonce"
				return s, e
			}
		},
	} {
		p.signsIn(map[string]any{"sub": "subject-owner", "email": "owner@example.com", "email_verified": true})
		p.mu.Lock()
		wrong()
		p.mu.Unlock()
		b := newSSOBrowser(t, cfg, addrs)
		if res, page := b.signIn("/settings"); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d\n%s", name, res.StatusCode, page)
		}
		if res, _ := b.get("/settings"); res.StatusCode != http.StatusSeeOther {
			t.Errorf("%s: signed in: %d", name, res.StatusCode)
		}
	}

	// A sign-in belongs to the browser that started it.
	p.signsIn(map[string]any{"sub": "subject-owner", "email": "owner@example.com", "email_verified": true})
	first := newSSOBrowser(t, cfg, addrs)
	res, _ = first.get("/login/sso")
	to := res.Header.Get("Location")
	victim := newSSOBrowser(t, cfg, addrs)
	if res, page := victim.followProvider(to); res.StatusCode != http.StatusForbidden {
		t.Errorf("another browser finishing it: %d\n%s", res.StatusCode, page)
	}
	if res, page := first.get("/login/sso/callback?code=the-code&state=wrong"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a wrong state: %d\n%s", res.StatusCode, page)
	}
	// And it works once: that try used it up.
	if res, page := first.followProvider(to); res.StatusCode != http.StatusForbidden {
		t.Errorf("a second try: %d\n%s", res.StatusCode, page)
	}
	refused := newSSOBrowser(t, cfg, addrs)
	p.mu.Lock()
	p.refuse = "access_denied"
	p.mu.Unlock()
	if res, page := refused.signIn("/"); res.StatusCode != http.StatusForbidden || !strings.Contains(page, "access_denied") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}

	// The API has the same, and it is for administrators.
	got, err := c.SSO(ctx)
	if err != nil || got.Issuer != p.srv.URL || !got.HasSecret || got.ClientSecret != "" || !strings.HasSuffix(got.RedirectURL, "/login/sso/callback") {
		t.Errorf("%+v %v", got, err)
	}
}

// TestSSOGroups gives people what their groups say, and takes it away when they no longer say it.
func TestSSOGroups(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	db := testAuth(t, cfg).DB
	p := newFakeProvider(t)
	if res, body := owner.post("/projects", url.Values{"csrf": {owner.token()}, "name": {"web"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	setUpSSO(t, owner, p, url.Values{"provision": {"1"}, "group_rules": {"ops = admin\r\nweb-team = project web deployer\r\nweb-leads = project web maintainer\r\n# a comment\r\ngone = project nothing viewer"}})
	signInAs := func(groups ...any) {
		t.Helper()
		if groups == nil {
			groups = []any{} // that they have none, which is something else from not saying
		}
		p.signsIn(map[string]any{"sub": "subject-sam", "email": "sam@example.com", "email_verified": true, "groups": groups})
		if res, page := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("%d\n%s", res.StatusCode, page)
		}
	}
	state := func() (string, string) {
		t.Helper()
		u, err := db.UserByEmail(ctx, "sam@example.com")
		if err != nil {
			t.Fatal(err)
		}
		members, err := db.Members(ctx, "web")
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range members {
			if m.UserID == u.ID {
				return u.Role, m.Role
			}
		}
		return u.Role, ""
	}
	signInAs("web-team", "something-else")
	if role, member := state(); role != "member" || member != "deployer" {
		t.Errorf("%s %s", role, member)
	}
	signInAs("web-team", "web-leads", "ops")
	if role, member := state(); role != "admin" || member != "maintainer" {
		t.Errorf("the highest of what they have: %s %s", role, member)
	}
	signInAs()
	if role, member := state(); role != "member" || member != "" {
		t.Errorf("having left the groups: %s %s", role, member)
	}
}

// TestSSOOnly leaves signing in to the provider, with a way back from the server's own machine.
func TestSSOOnly(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	p := newFakeProvider(t)
	setUpSSO(t, owner, p, url.Values{"passwords_off": {"1"}})

	anon := newSSOBrowser(t, cfg, addrs)
	if _, page := anon.get("/login"); strings.Contains(page, `name="password"`) || !strings.Contains(page, "Sign in with Acme login") {
		t.Errorf("%s", page)
	}
	if res, page := anon.post("/login", url.Values{"email": {"owner@example.com"}, "password": {"a long and unusual passphrase"}}); res.StatusCode != http.StatusForbidden || !strings.Contains(page, "provider") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	if status, body := apiLogin(t, addrs, cfg, map[string]string{"email": "owner@example.com", "password": "a long and unusual passphrase"}); status != http.StatusForbidden || !strings.Contains(body, "passwords_off") {
		t.Errorf("%d %s", status, body)
	}
	// Tokens are not passwords, and whoever is signed in stays so.
	if res, _ := owner.get("/account"); res.StatusCode != http.StatusOK {
		t.Errorf("%d", res.StatusCode)
	}
	if _, err := c.SSO(ctx); err != nil {
		t.Error(err)
	}
	// The way back, with what the server's own machine has.
	if err := c.TurnOffSSO(ctx); err != nil {
		t.Fatal(err)
	}
	if _, res := freshBrowser(t, cfg, addrs, "owner@example.com", "a long and unusual passphrase"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	if got, err := c.SSO(ctx); err != nil || got.Issuer != "" {
		t.Errorf("%+v %v", got, err)
	}
}

// proxied is a request as a proxy that signs people in passes it on.
func proxied(t *testing.T, addrs Addrs, cfg config.Config, path string, headers map[string]string, cookies []*http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = cfg.ConsoleHost
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
}

// TestTrustedHeaderSignIn believes a proxy that signs people in, and nobody else.
func TestTrustedHeaderSignIn(t *testing.T) {
	cfg := testConfig(t)
	cfg.Proxy.Trusted = []string{"127.0.0.1"}
	cfg.Auth = config.Auth{TrustedHeader: "X-Auth-Email", TrustedNameHeader: "X-Auth-Name", TrustedProvision: true}
	addrs, _ := start(t, cfg)
	signIn(t, cfg, addrs) // claims the server
	ctx := context.Background()
	db := testAuth(t, cfg).DB
	saying := func(email string) map[string]string {
		return map[string]string{"X-Auth-Email": email, "X-Auth-Name": "Someone Named"}
	}

	// Without the header, it is as ever.
	if res := proxied(t, addrs, cfg, "/account", nil, nil); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	// With it, they are signed in at once, with an account if they had none.
	res := proxied(t, addrs, cfg, "/account", saying("ada@example.com"), nil)
	if res.StatusCode != http.StatusOK || len(res.Cookies()) == 0 {
		t.Fatalf("%d %v", res.StatusCode, res.Cookies())
	}
	u, err := db.UserByEmail(ctx, "ada@example.com")
	if err != nil || u.Name != "Someone Named" || u.Role != "member" {
		t.Errorf("%+v %v", u, err)
	}
	// The session stands from then on, without making another.
	session := res.Cookies()
	again := proxied(t, addrs, cfg, "/account", saying("ada@example.com"), session)
	if again.StatusCode != http.StatusOK || len(again.Cookies()) != 0 {
		t.Errorf("%d %v", again.StatusCode, again.Cookies())
	}
	// If the proxy says someone else now, it is them, and not whoever the cookie was.
	other := proxied(t, addrs, cfg, "/account", saying("bob@example.com"), session)
	if other.StatusCode != http.StatusOK || len(other.Cookies()) == 0 {
		t.Errorf("%d %v", other.StatusCode, other.Cookies())
	}
	// Someone disabled gets nowhere, and nor does what is not an address.
	if err := db.SetUserDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if res := proxied(t, addrs, cfg, "/account", saying("ada@example.com"), nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("disabled: %d", res.StatusCode)
	}
	if res := proxied(t, addrs, cfg, "/account", saying("not an address"), nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("not an address: %d", res.StatusCode)
	}
}

func TestTrustedHeaderWithoutProvisioning(t *testing.T) {
	cfg := testConfig(t)
	cfg.Proxy.Trusted = []string{"127.0.0.1"}
	cfg.Auth = config.Auth{TrustedHeader: "X-Auth-Email"}
	addrs, _ := start(t, cfg)
	signIn(t, cfg, addrs)
	if res := proxied(t, addrs, cfg, "/account", map[string]string{"X-Auth-Email": "stranger@example.com"}, nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("someone with no account: %d", res.StatusCode)
	}
	if res := proxied(t, addrs, cfg, "/account", map[string]string{"X-Auth-Email": "owner@example.com"}, nil); res.StatusCode != http.StatusOK {
		t.Errorf("someone with one: %d", res.StatusCode)
	}
}

// TestTrustedHeaderFromAnyoneElse: from anything but the proxy, the header is just something a client said.
func TestTrustedHeaderFromAnyoneElse(t *testing.T) {
	cfg := testConfig(t)
	cfg.Proxy.Trusted = []string{"10.9.8.7"}
	cfg.Auth = config.Auth{TrustedHeader: "X-Auth-Email", TrustedProvision: true}
	addrs, _ := start(t, cfg)
	signIn(t, cfg, addrs)
	if res := proxied(t, addrs, cfg, "/account", map[string]string{"X-Auth-Email": "owner@example.com"}, nil); res.StatusCode != http.StatusSeeOther {
		t.Errorf("a client saying that it is someone: %d", res.StatusCode)
	}
	if _, err := testAuth(t, cfg).DB.UserByEmail(context.Background(), "stranger@example.com"); err == nil {
		t.Error("an account was made")
	}
	// A header on its own, with nobody to trust, stops the server from starting at all.
	broken := testConfig(t)
	broken.Auth = config.Auth{TrustedHeader: "X-Auth-Email"}
	broken.Proxy.Trusted = nil
	if err := broken.Validate(); err == nil || !strings.Contains(err.Error(), "proxy.trusted") {
		t.Errorf("%v", err)
	}
}

// TestSSOAddressHasToBeVerified: an address that the provider does not call verified cannot join an account.
// Otherwise, where anyone can set an address at the provider, anyone could be an administrator here.
func TestSSOAddressHasToBeVerified(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	owner := signIn(t, cfg, addrs)
	p := newFakeProvider(t)
	setUpSSO(t, owner, p, url.Values{})

	p.signsIn(map[string]any{"sub": "subject-eve", "email": "owner@example.com"})
	if res, page := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusForbidden || !strings.Contains(page, "verified") {
		t.Errorf("no word on whether the address is verified: %d\n%s", res.StatusCode, page)
	}
	if _, err := testAuth(t, cfg).DB.UserByIdentity(context.Background(), p.srv.URL, "subject-eve"); err == nil {
		t.Error("the subject joined the account")
	}
	// Nor does a domain allowed become another, by the rules of a language.
	setUpSSO(t, owner, p, url.Values{"provision": {"1"}, "email_domains": {"inc.com"}})
	p.signsIn(map[string]any{"sub": "subject-other", "email": "someone@İnc.com", "email_verified": true})
	if res, _ := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a domain that only looks allowed: %d", res.StatusCode)
	}
	setUpSSO(t, owner, p, url.Values{})
	// A different address, by the rules of a language and not of mail, is not the same address.
	p.signsIn(map[string]any{"sub": "subject-eve", "email": "ownİr@example.com", "email_verified": true})
	if res, _ := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusForbidden {
		t.Errorf("%d", res.StatusCode)
	}

	// With the server told to take its word, it does, and it still never believes "unverified".
	setUpSSO(t, owner, p, url.Values{"trust_emails": {"1"}})
	p.signsIn(map[string]any{"sub": "subject-eve", "email": "owner@example.com", "email_verified": false})
	if res, _ := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusForbidden {
		t.Errorf("%d", res.StatusCode)
	}
	p.signsIn(map[string]any{"sub": "subject-owner", "email": "owner@example.com"})
	if res, page := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
}

// TestSSOProvisionedCanOnlyLook: everyone is a maintainer of the default project, and someone nobody chose to give
// an account should not be able to deploy there.
func TestSSOProvisionedCanOnlyLook(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	owner := signIn(t, cfg, addrs)
	p := newFakeProvider(t)
	setUpSSO(t, owner, p, url.Values{"provision": {"1"}})
	p.signsIn(map[string]any{"sub": "subject-new", "email": "new@example.com", "email_verified": true})
	if res, page := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d\n%s", res.StatusCode, page)
	}
	ctx := context.Background()
	authn := testAuth(t, cfg)
	u, err := authn.DB.UserByEmail(ctx, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	project, err := authn.DB.ProjectByName(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if role, err := authn.DB.MemberRole(ctx, project.ID, u.ID); err != nil || role != "viewer" {
		t.Errorf("%q %v", role, err)
	}
}

// TestSSOGroupRuleKeepsThemToALook: a rule says that a group decides a project. Someone not in the group should not
// end up with what the project gives everyone, which in the default project is more than the group would give.
func TestSSOGroupRuleKeepsThemToALook(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	owner := signIn(t, cfg, addrs)
	p := newFakeProvider(t)
	setUpSSO(t, owner, p, url.Values{"provision": {"1"}, "group_rules": {"web-team = project default deployer"}})
	signsIn := func(groups ...string) {
		t.Helper()
		if groups == nil {
			groups = []string{}
		}
		p.signsIn(map[string]any{"sub": "subject-new", "email": "new@example.com", "email_verified": true, "groups": groups})
		if res, page := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("%d\n%s", res.StatusCode, page)
		}
	}
	ctx := context.Background()
	db := testAuth(t, cfg).DB
	role := func() string {
		t.Helper()
		u, err := db.UserByEmail(ctx, "new@example.com")
		if err != nil {
			t.Fatal(err)
		}
		project, err := db.ProjectByName(ctx, "default")
		if err != nil {
			t.Fatal(err)
		}
		got, err := db.MemberRole(ctx, project.ID, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	signsIn("contractors")
	if got := role(); got != "viewer" {
		t.Errorf("not in the group, the first time: %q", got)
	}
	signsIn("web-team")
	if got := role(); got != "deployer" {
		t.Errorf("in the group: %q", got)
	}
	signsIn()
	if got := role(); got != "viewer" {
		t.Errorf("having left it: %q", got)
	}
}

// TestSSOAskingForUserInfo: a provider need not put someone's address or groups in its token.
func TestSSOAskingForUserInfo(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	owner := signIn(t, cfg, addrs)
	p := newFakeProvider(t)
	setUpSSO(t, owner, p, url.Values{"provision": {"1"}, "group_rules": {"ops = admin"}})
	p.signsIn(map[string]any{"sub": "subject-new"})
	p.mu.Lock()
	p.userInfo = map[string]any{"sub": "subject-new", "email": "new@example.com", "email_verified": true, "name": "New", "groups": []string{"ops"}}
	p.mu.Unlock()
	if res, page := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d\n%s", res.StatusCode, page)
	}
	ctx := context.Background()
	db := testAuth(t, cfg).DB
	u, err := db.UserByEmail(ctx, "new@example.com")
	if err != nil || u.Role != "admin" {
		t.Fatalf("%+v %v", u, err)
	}

	// Some providers leave groups out for someone in none, and someone just removed from their last should lose
	// what it gave them.
	p.mu.Lock()
	p.userInfo = map[string]any{"sub": "subject-new", "email": "new@example.com", "email_verified": true}
	p.mu.Unlock()
	if res, _ := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", res.StatusCode)
	}
	if u, err = db.UserByEmail(ctx, "new@example.com"); err != nil || u.Role != "member" {
		t.Errorf("hearing nothing left their role: %+v %v", u, err)
	}
	// Someone else's user info counts for nothing.
	p.signsIn(map[string]any{"sub": "subject-other"})
	p.mu.Lock()
	p.userInfo = map[string]any{"sub": "subject-new", "email": "new@example.com", "email_verified": true}
	p.mu.Unlock()
	if res, _ := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusForbidden {
		t.Errorf("%d", res.StatusCode)
	}
}

// TestSSOAddressEndingInASlash: a provider's address has to match what it calls itself, to the last character, and
// nobody should have to know whether that is a slash.
func TestSSOAddressEndingInASlash(t *testing.T) {
	for _, c := range []struct{ says, typed string }{{"/", ""}, {"/", "/"}, {"", "/"}, {"", ""}} {
		t.Run("says "+c.says+" typed "+c.typed, func(t *testing.T) {
			cfg := testConfig(t)
			addrs, _ := start(t, cfg)
			owner := signIn(t, cfg, addrs)
			p := newFakeProvider(t)
			p.issuerEnd = c.says
			setUpSSO(t, owner, p, url.Values{"issuer": {p.srv.URL + c.typed}})
			p.signsIn(map[string]any{"sub": "subject-owner", "email": "owner@example.com", "email_verified": true})
			if res, page := newSSOBrowser(t, cfg, addrs).signIn("/"); res.StatusCode != http.StatusSeeOther {
				t.Errorf("%d\n%s", res.StatusCode, page)
			}
		})
	}
}

// TestTrustedHeaderWhileUnclaimed: until someone has claimed a server, its setup link has to work through the
// proxy, and the first person to come by should not make it theirs.
func TestTrustedHeaderWhileUnclaimed(t *testing.T) {
	cfg := testConfig(t)
	cfg.Proxy.Trusted = []string{"127.0.0.1"}
	cfg.Auth = config.Auth{TrustedHeader: "X-Auth-Email", TrustedProvision: true}
	addrs, _ := start(t, cfg)
	code, err := os.ReadFile(NewLayout(cfg.DataDir).ClaimCode)
	if err != nil {
		t.Fatal(err)
	}
	res := proxied(t, addrs, cfg, "/setup?code="+strings.TrimSpace(string(code)), map[string]string{"X-Auth-Email": "first@example.com"}, nil)
	if res.StatusCode != http.StatusOK || len(res.Cookies()) != 0 {
		t.Errorf("%d %v", res.StatusCode, res.Cookies())
	}
	if _, err := testAuth(t, cfg).DB.UserByEmail(context.Background(), "first@example.com"); err == nil {
		t.Error("the first to come by got an account")
	}
	owner := signIn(t, cfg, addrs)
	_ = owner
	res = proxied(t, addrs, cfg, "/account", map[string]string{"X-Auth-Email": "first@example.com"}, nil)
	if res.StatusCode != http.StatusOK {
		t.Errorf("once claimed: %d", res.StatusCode)
	}
	// A request for an asset leaves no session behind.
	res = proxied(t, addrs, cfg, "/assets/console.css", map[string]string{"X-Auth-Email": "first@example.com"}, nil)
	if len(res.Cookies()) != 0 {
		t.Errorf("%v", res.Cookies())
	}
}

// TestInvitationsWithPasswordsOff: choosing a password is signing in with one.
func TestInvitationsWithPasswordsOff(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	owner := signIn(t, cfg, addrs)
	res, body := owner.post("/settings/users/create", url.Values{"csrf": {owner.token()}, "email": {"late@example.com"}, "role": {"member"}})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	link := regexp.MustCompile(`/invite\?code=([^<"\s]+)`).FindStringSubmatch(body)
	if link == nil {
		t.Fatalf("no link in: %s", body)
	}
	p := newFakeProvider(t)
	setUpSSO(t, owner, p, url.Values{"passwords_off": {"1"}})
	u := &consoleUser{t: t, addr: addrs.HTTP, host: cfg.ConsoleHost}
	res, _ = u.post("/invite", url.Values{"code": {link[1]}, "password": {"a passphrase of their own"}, "confirm": {"a passphrase of their own"}})
	if res.StatusCode == http.StatusSeeOther {
		t.Errorf("a link gave someone a password: %d", res.StatusCode)
	}
	got, err := testAuth(t, cfg).DB.UserByEmail(context.Background(), "late@example.com")
	if err != nil || got.PasswordHash != "" {
		t.Errorf("%v %v", got.PasswordHash != "", err)
	}
}
