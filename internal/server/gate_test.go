package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
)

// visitor is a browser on the app's hostname: it keeps its cookies, and follows no redirects.
type visitor struct {
	t      *testing.T
	addr   string
	host   string
	jar    http.CookieJar
	client *http.Client
}

func newVisitor(t *testing.T, addrs Addrs, host string) *visitor {
	jar, _ := cookiejar.New(nil)
	addr := addrs.HTTP.String()
	return &visitor{t: t, addr: addr, host: host, jar: jar, client: &http.Client{
		Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}, DisableKeepAlives: true},
	}}
}

// do sends a request as a browser going to a page; with the host's real name in the address, so
// that the jar applies, and the connection going to the server.
func (v *visitor) do(method, path string, form url.Values, header map[string]string) (*http.Response, string) {
	v.t.Helper()
	var body *strings.Reader = strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, "http://"+v.host+portOf(v.addr)+path, body)
	if err != nil {
		v.t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, val := range header {
		req.Header.Set(k, val)
	}
	res, err := v.client.Do(req)
	if err != nil {
		v.t.Fatal(err)
	}
	defer res.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res, b.String()
}

func (v *visitor) get(path string) (*http.Response, string) {
	v.t.Helper()
	return v.do(http.MethodGet, path, nil, nil)
}

func portOf(addr string) string { return addr[strings.LastIndex(addr, ":"):] }

func setGate(t *testing.T, c *client.Client, app, env string, change apitypes.AccessChange) apitypes.Access {
	t.Helper()
	a, err := c.SetAccess(context.Background(), app, env, change)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func gatedApp(t *testing.T, cfg config.Config, addrs Addrs, c *client.Client, app string) {
	t.Helper()
	if _, err := deployApp(t, c, app, appPlan(map[string]string{"TESTAPP_BODY": "the secret page"})); err != nil {
		t.Fatal(err)
	}
}

// TestPasswordGate goes through the gate as a browser and as a script, and tries the ways round it.
func TestPasswordGate(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	gatedApp(t, cfg, addrs, c, "shop")
	const host = "shop.localhost"

	// Set by a maintainer, and at once.
	if _, err := c.SetAccess(context.Background(), "shop", "production", apitypes.AccessChange{Mode: "password"}); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("the gate without a password: %v", err)
	}
	if _, err := c.SetAccess(context.Background(), "shop", "production", apitypes.AccessChange{Mode: "password", Password: "short"}); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("a short password: %v", err)
	}
	a := setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "password", Password: "correct horse"})
	if a.Mode != "password" || a.Source != "setting" || !a.PasswordSet {
		t.Errorf("the gate: %+v", a)
	}

	// Nothing of the app without it, at any path, however it is asked.
	browser := newVisitor(t, addrs, host)
	for _, path := range []string{"/", "/env?name=TESTAPP_BODY", "/deep/path?x=1"} {
		res, page := browser.get(path)
		if res.StatusCode != http.StatusUnauthorized || strings.Contains(page, "the secret page") {
			t.Errorf("%s: %d\n%s", path, res.StatusCode, page)
		}
		contains(t, "the form", page, `action="/.moorage/gate/password"`, `type="password"`)
		if res.Header.Get("X-Robots-Tag") == "" || !strings.Contains(res.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("the form's headers: %v", res.Header)
		}
	}
	if res, body := get(t, addrs.HTTP, host, "/"); res.StatusCode != http.StatusUnauthorized || !strings.Contains(res.Header.Get("WWW-Authenticate"), "Basic") || strings.Contains(body, "secret") {
		t.Errorf("a script: %d %v", res.StatusCode, res.Header)
	}

	// A wrong password does nothing; the right one lets in, and goes back where they were going.
	if res, page := browser.do(http.MethodPost, "/.moorage/gate/password", url.Values{"password": {"wrong"}, "return": {"/"}}, nil); res.StatusCode != http.StatusUnauthorized || !strings.Contains(page, "not the password") {
		t.Errorf("a wrong password: %d", res.StatusCode)
	}
	res, _ := browser.do(http.MethodPost, "/.moorage/gate/password", url.Values{"password": {"correct horse"}, "return": {"/env?name=TESTAPP_BODY"}}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/env?name=TESTAPP_BODY" {
		t.Fatalf("the right password: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, ck := range res.Cookies() {
		if strings.HasPrefix(ck.Name, "moorage_gate_env_") {
			cookie = ck
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Domain != "" || cookie.Path != "/" {
		t.Fatalf("the cookie: %+v", cookie)
	}
	if res, body := browser.get("/"); res.StatusCode != http.StatusOK || body != "the secret page" {
		t.Errorf("once in: %d %q", res.StatusCode, body)
	}

	// The app never hears of the gate's cookie, and hears others as they were.
	if _, body := browser.do(http.MethodGet, "/headers", nil, map[string]string{"Cookie": "mine=1; " + cookie.Name + "=" + cookie.Value + "; other=2"}); strings.Contains(body, "moorage_gate") || !strings.Contains(body, "mine=1") || !strings.Contains(body, "other=2") {
		t.Errorf("what the app heard: %s", body)
	}

	// A return address cannot go anywhere else.
	for _, evil := range []string{"https://evil.example/", "//evil.example/", "/\\evil.example", "/.moorage/health"} {
		res, _ := newVisitor(t, addrs, host).do(http.MethodPost, "/.moorage/gate/password", url.Values{"password": {"correct horse"}, "return": {evil}}, nil)
		if res.Header.Get("Location") != "/" {
			t.Errorf("going back to %q: %s", evil, res.Header.Get("Location"))
		}
	}
	// Nor is a form from another site taken.
	if res, _ := newVisitor(t, addrs, host).do(http.MethodPost, "/.moorage/gate/password", url.Values{"password": {"correct horse"}}, map[string]string{"Origin": "https://evil.example"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a form from elsewhere: %d", res.StatusCode)
	}

	// A cookie of another environment, altered or made up is nothing.
	other := newVisitor(t, addrs, host)
	for _, forged := range []string{"x", cookie.Value + "0", "djF8ZW52X3h8MXw5OTk5OTk5OTk5fHBhc3N3b3JkfA." + strings.Repeat("0", 64)} {
		if res, _ := other.do(http.MethodGet, "/", nil, map[string]string{"Cookie": cookie.Name + "=" + forged}); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("the cookie %q: %d", forged, res.StatusCode)
		}
	}
	setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "public"})
	if _, err := deployApp(t, c, "other", appPlan(map[string]string{"TESTAPP_BODY": "another secret"})); err != nil {
		t.Fatal(err)
	}
	setGate(t, c, "other", "production", apitypes.AccessChange{Mode: "password", Password: "another password"})
	if res, _ := other.do(http.MethodGet, "/", nil, nil); res.StatusCode != http.StatusOK {
		t.Errorf("shop is public now: %d", res.StatusCode)
	}
	// Its own cookie's value, under the name the other app's gate looks for.
	stranger := newVisitor(t, addrs, "other.localhost")
	otherEnv := testEnvID(t, cfg, "other")
	if res, body := stranger.do(http.MethodGet, "/", nil, map[string]string{"Cookie": "moorage_gate_" + otherEnv + "=" + cookie.Value}); res.StatusCode != http.StatusUnauthorized || strings.Contains(body, "another secret") {
		t.Errorf("shop's cookie on another app: %d", res.StatusCode)
	}

	// Changing the gate ends who was in; a new password too.
	setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "password", Password: "correct horse"})
	if res, _ := browser.get("/"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("the old cookie, after the gate changed: %d", res.StatusCode)
	}

	// A script sends it with HTTP Basic, which the app never hears of.
	script := func(password, path string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+path, nil)
		req.Host = host
		req.SetBasicAuth("anyone", password)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := res.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return res, b.String()
	}
	if res, body := script("correct horse", "/"); res.StatusCode != http.StatusOK || body != "the secret page" {
		t.Errorf("HTTP Basic: %d %q", res.StatusCode, body)
	}
	if _, body := script("correct horse", "/headers"); strings.Contains(body, "Authorization: B") {
		t.Errorf("the app heard the password: %s", body)
	}
	if res, _ := script("wrong", "/"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("HTTP Basic, wrongly: %d", res.StatusCode)
	}

	// Too many wrong passwords make it wait.
	limited := false
	for range 12 {
		if res, _ := script("wrong", "/"); res.StatusCode == http.StatusTooManyRequests {
			limited = res.Header.Get("Retry-After") != ""
			break
		}
	}
	if !limited {
		t.Errorf("guessing goes on without limit")
	}
	// A password that was right lately is no guess, even so.
	if res, _ := script("correct horse", "/"); res.StatusCode != http.StatusOK {
		t.Errorf("the right password, remembered, while wrong ones wait: %d", res.StatusCode)
	}
}

func testEnvID(t *testing.T, cfg config.Config, app string) string {
	t.Helper()
	id, err := testAuth(t, cfg).DB.EnvID(context.Background(), app, "production")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestGatesAgainstClients are what an independent review found could be got round, kept as tests.
func TestGatesAgainstClients(t *testing.T) {
	cfg := testConfig(t)
	cfg.Proxy.Trusted = []string{"127.0.0.1", "::1"}
	cfg.Proxy.ExternalScheme = "http"
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	gatedApp(t, cfg, addrs, c, "shop")
	const host = "shop.localhost"
	send := func(header map[string]string, path string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+path, nil)
		req.Host = host
		for k, v := range header {
			req.Header[k] = []string{v}
		}
		res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res, string(body)
	}

	// A list of addresses goes by what the proxy saw, not by what the client told it: a proxy
	// adds the address it saw to the end of what it was sent.
	setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "public", Allow: new([]string{"203.0.113.7"})})
	if res, _ := send(map[string]string{"X-Forwarded-For": "203.0.113.7, 198.51.100.9"}, "/"); res.StatusCode != http.StatusForbidden {
		t.Errorf("claiming to be on the list: %d", res.StatusCode)
	}
	if res, body := send(map[string]string{"X-Forwarded-For": "198.51.100.9, 203.0.113.7"}, "/"); res.StatusCode != http.StatusOK || body != "the secret page" {
		t.Errorf("on the list: %d", res.StatusCode)
	}
	if _, heard := send(map[string]string{"X-Forwarded-For": "1.2.3.4, 203.0.113.7"}, "/headers"); !strings.Contains(heard, "X-Real-Ip: 203.0.113.7") {
		t.Errorf("who the app was told it was: %s", heard)
	}

	// What is only Moorage's cannot be said to the app, however it is spelled, and it is told
	// what shows that the request came through here.
	setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "public", Allow: new([]string{})})
	_, heard := send(map[string]string{"X_Moorage_User": "boss", "X-Moorage-User": "boss", "X_Real_IP": "9.9.9.9", "X-Moorage-Proxy-Token": "made-up"}, "/headers")
	if strings.Contains(heard, "boss") || strings.Contains(heard, "9.9.9.9") || strings.Contains(heard, "made-up") {
		t.Errorf("what a client told the app: %s", heard)
	}
	_, token := send(nil, "/env?name=MOORAGE_PROXY_TOKEN")
	if len(token) < 32 || !strings.Contains(heard, "X-Moorage-Proxy-Token: "+token) {
		t.Errorf("the app's token %q, and what it heard: %s", token, heard)
	}

	// A Moorage token is not for the app, even where no gate wants it.
	if _, heard := send(map[string]string{"Authorization": "Bearer moor_secret"}, "/headers"); strings.Contains(heard, "moor_secret") {
		t.Errorf("the app heard a token: %s", heard)
	}
	if _, heard := send(map[string]string{"Authorization": "Bearer its-own-scheme"}, "/headers"); !strings.Contains(heard, "its-own-scheme") {
		t.Errorf("the app's own header was taken away: %s", heard)
	}

	// A burst of guesses is turned away, and not all checked at once.
	setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "password", Password: "correct horse"})
	var wg sync.WaitGroup
	var checked, turnedAway atomic.Int32
	begun := time.Now()
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/", nil)
			req.Host = host
			req.SetBasicAuth("x", "a wrong guess")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			res.Body.Close()
			switch res.StatusCode {
			case http.StatusUnauthorized:
				checked.Add(1)
			case http.StatusTooManyRequests:
				turnedAway.Add(1)
			}
		}()
	}
	wg.Wait()
	if turnedAway.Load() < 30 {
		t.Errorf("a burst of 40: %d checked, %d turned away, in %v", checked.Load(), turnedAway.Load(), time.Since(begun))
	}

	// Bad links lock nobody out: they are not there to be guessed. Try them from many addresses.
	setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "share"})
	for i := range 120 {
		send(map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.%d.%d", i%200, i%250)}, "/.moorage/share/nope"+strconv.Itoa(i))
	}
	link, err := c.CreateShareLink(ctx, "shop", "production", apitypes.NewShareLink{})
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := send(map[string]string{"X-Forwarded-For": "192.0.2.200"}, mustPath(t, link.URL)); res.StatusCode != http.StatusSeeOther {
		t.Errorf("a good link, from elsewhere, after many bad ones: %d", res.StatusCode)
	}
}

// TestGateCookiesEnd shows that what let someone in ending ends their way in, however they use it.
func TestGateCookiesEnd(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	admin := signIn(t, cfg, addrs)
	gatedApp(t, cfg, addrs, c, "shop")
	setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "login", AnyUser: true})
	const host = "shop.localhost"
	member := addTeammate(t, cfg, addrs, admin, "member@example.com")

	signedIn := func() *visitor {
		t.Helper()
		v := newVisitor(t, addrs, host)
		res, _ := v.get("/")
		to, _ := url.Parse(res.Header.Get("Location"))
		res, _ = member.get(to.Path + "?" + to.RawQuery)
		back, _ := url.Parse(res.Header.Get("Location"))
		if res, _ := v.get(back.Path + "?" + back.RawQuery); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("the callback: %d", res.StatusCode)
		}
		if res, _ := v.get("/"); res.StatusCode != http.StatusOK {
			t.Fatalf("signed in: %d", res.StatusCode)
		}
		return v
	}
	v := signedIn()

	// A stream that was let in goes on while it should, and no longer.
	long := func(v *visitor) <-chan error {
		done := make(chan error, 1)
		go func() {
			req, _ := http.NewRequest(http.MethodGet, "http://"+host+portOf(v.addr)+"/slow?ms=20000", nil)
			res, err := v.client.Do(req)
			if err == nil {
				_, err = io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if err == nil && res.StatusCode >= 500 {
					err = errors.New(res.Status)
				}
			}
			done <- err
		}()
		return done
	}
	stream := long(v)
	select {
	case err := <-stream:
		t.Fatalf("a long request ended by itself: %v", err)
	case <-time.After(700 * time.Millisecond):
	}

	// Signing out of the console ends it, and the request that was going on.
	if res, _ := member.post("/logout", url.Values{"csrf": {member.token()}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("signing out: %d", res.StatusCode)
	}
	waitUntil(t, "signing out to end the way in", func() bool {
		res, _ := v.get("/")
		return res.StatusCode != http.StatusOK
	})
	select {
	case <-stream:
	case <-time.After(5 * time.Second):
		t.Errorf("the long request goes on after its way in ended")
	}
}

// TestTokenAtAGateEndsWithTheToken ends a long request when the token that let it in is revoked.
func TestTokenAtAGateEndsWithTheToken(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	admin := signIn(t, cfg, addrs)
	gatedApp(t, cfg, addrs, c, "shop")
	setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "login", AnyUser: true})
	secret := tokenOf(t, admin, "gate")
	req, err := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/slow?ms=30000", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "shop.localhost"
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Accept", "text/plain")
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}()
	select {
	case <-ended:
		t.Fatal("a long request ended by itself")
	case <-time.After(700 * time.Millisecond):
	}
	manager := tokenClient(t, c, tokenOf(t, admin, "admin"))
	all, err := manager.Tokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range all {
		if tok.Name == "for gate" {
			if _, err := manager.RevokeToken(context.Background(), tok.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Error("the long request goes on after its token was revoked")
	}
}

// TestShareDoesNotChangeWhoYouAre keeps someone signed in as themselves, whatever a page makes
// their browser fetch.
func TestShareDoesNotChangeWhoYouAre(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	admin := signIn(t, cfg, addrs)
	gatedApp(t, cfg, addrs, c, "shop")
	setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "login", AnyUser: true})
	link, err := c.CreateShareLink(context.Background(), "shop", "production", apitypes.NewShareLink{})
	if err != nil {
		t.Fatal(err)
	}
	path := mustPath(t, link.URL)
	v := newVisitor(t, addrs, "shop.localhost")
	res, _ := v.get("/")
	to, _ := url.Parse(res.Header.Get("Location"))
	res, _ = admin.get(to.Path + "?" + to.RawQuery)
	back, _ := url.Parse(res.Header.Get("Location"))
	v.get(back.Path + "?" + back.RawQuery)

	if res, _ := v.do(http.MethodGet, path, nil, map[string]string{"Sec-Fetch-Dest": "document"}); res.StatusCode != http.StatusSeeOther || cookieNamed(res, "moorage_gate_"+testEnvID(t, cfg, "shop")) != nil {
		t.Errorf("a link, when already in: %d %v", res.StatusCode, res.Header)
	}
	if _, heard := v.get("/headers"); !strings.Contains(heard, "owner@example.com") {
		t.Errorf("who they are: %s", heard)
	}
	// An image on someone's page does not use up a link.
	other := newVisitor(t, addrs, "shop.localhost")
	if res, _ := other.do(http.MethodGet, path, nil, map[string]string{"Sec-Fetch-Dest": "image"}); cookieNamed(res, "moorage_gate_"+testEnvID(t, cfg, "shop")) != nil {
		t.Errorf("a link fetched as an image: %v", res.Header)
	}
	// A form from elsewhere neither.
	if res, _ := other.do(http.MethodPost, path, url.Values{}, map[string]string{"Origin": "https://evil.example"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a link posted from elsewhere: %d", res.StatusCode)
	}
}

// TestLoginGate signs in through the console, and shows who is let in and who is not.
func TestLoginGate(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	admin := signIn(t, cfg, addrs)
	ctx := context.Background()
	gatedApp(t, cfg, addrs, c, "shop")
	setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "login"})
	if _, err := c.CreateProject(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MoveApp(ctx, "shop", "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ChangeProject(ctx, "team", apitypes.ProjectChange{Everyone: new("none")}); err != nil {
		t.Fatal(err)
	}
	member := addTeammate(t, cfg, addrs, admin, "member@example.com")
	if _, err := c.SetMember(ctx, "team", "member@example.com", "viewer"); err != nil {
		t.Fatal(err)
	}
	outsider := addTeammate(t, cfg, addrs, admin, "outsider@example.com")

	// Whoever is not signed in goes to sign in.
	const host = "shop.localhost"
	browser := newVisitor(t, addrs, host)
	res, page := browser.get("/some/page?x=1")
	if res.StatusCode != http.StatusFound || strings.Contains(page, "secret") {
		t.Fatalf("not signed in: %d\n%s", res.StatusCode, page)
	}
	to, err := url.Parse(res.Header.Get("Location"))
	if err != nil || to.Path != "/auth/gate" || to.Query().Get("host") != host || to.Query().Get("return") != "/some/page?x=1" || to.Query().Get("state") == "" {
		t.Fatalf("the redirect: %s %v", res.Header.Get("Location"), err)
	}
	// The console asks them to, if they have not.
	consolePath := to.Path + "?" + to.RawQuery
	stranger := &consoleUser{t: t, addr: addrs.HTTP, host: cfg.ConsoleHost}
	if res, _ := stranger.get(consolePath); res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/login") {
		t.Errorf("the console, not signed in: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// One without a role in its project is told so, and gets no code.
	res, page = outsider.get(consolePath)
	if res.StatusCode != http.StatusForbidden || strings.Contains(res.Header.Get("Location"), "code=") {
		t.Errorf("an outsider: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	contains(t, "the refusal", page, "no way into shop")

	// One with a role comes back with a code, which turns into a cookie of this hostname.
	res, _ = member.get(consolePath)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("a member: %d", res.StatusCode)
	}
	back, err := url.Parse(res.Header.Get("Location"))
	if err != nil || back.Path != "/.moorage/auth/callback" || back.Hostname() != host || back.Query().Get("code") == "" {
		t.Fatalf("back to the app: %s %v", res.Header.Get("Location"), err)
	}
	callback := back.Path + "?" + back.RawQuery

	// The code alone is not enough: another browser, that did not start this, gets nothing.
	other := newVisitor(t, addrs, host)
	if res, _ := other.get(callback); res.Header.Get("Set-Cookie") != "" && strings.Contains(res.Header.Get("Set-Cookie"), "moorage_gate=v") {
		t.Errorf("another browser was signed in: %v", res.Header)
	}
	if res, _ := other.get("/"); res.StatusCode == http.StatusOK {
		t.Errorf("another browser got in with the code alone")
	}

	res, _ = browser.get(callback)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/some/page?x=1" || res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("the callback: %d %v", res.StatusCode, res.Header)
	}
	if res, body := browser.get("/"); res.StatusCode != http.StatusOK || body != "the secret page" {
		t.Errorf("signed in: %d %q", res.StatusCode, body)
	}

	// The app hears who they are, and cannot be told otherwise.
	_, heard := browser.do(http.MethodGet, "/headers", nil, map[string]string{"X-Moorage-User-Email": "boss@example.com", "X-Moorage-User": "usr_boss"})
	contains(t, "what the app heard", heard, "member@example.com")
	if strings.Contains(heard, "boss") {
		t.Errorf("the app was lied to: %s", heard)
	}

	// A code is good once, and for its own hostname.
	if res, _ := browser.get(callback); res.StatusCode != http.StatusSeeOther {
		t.Errorf("the code a second time: %d", res.StatusCode)
	}
	if _, err := deployApp(t, c, "other", appPlan(map[string]string{"TESTAPP_BODY": "another secret"})); err != nil {
		t.Fatal(err)
	}
	setGate(t, c, "other", "production", apitypes.AccessChange{Mode: "login", AnyUser: true})
	second := newVisitor(t, addrs, "other.localhost")
	first, _ := second.get("/")
	secondTo, _ := url.Parse(first.Header.Get("Location"))
	res, _ = outsider.get(secondTo.Path + "?" + secondTo.RawQuery)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("anyone with an account, on another app: %d", res.StatusCode)
	}
	code, _ := url.Parse(res.Header.Get("Location"))
	wrongHost := newVisitor(t, addrs, host)
	begin, _ := wrongHost.get("/")
	wrongState, _ := url.Parse(begin.Header.Get("Location"))
	if res, _ := wrongHost.get("/.moorage/auth/callback?code=" + url.QueryEscape(code.Query().Get("code")) + "&state=" + url.QueryEscape(wrongState.Query().Get("state")) + "&return=/"); res.StatusCode == http.StatusOK || cookieNamed(res, "moorage_gate") != nil {
		t.Errorf("a code on another hostname: %d %v", res.StatusCode, res.Header)
	}

	// A token with the gate's scope gets a script through, and the app does not hear it.
	scoped := tokenOf(t, member, "gate")
	bearer := func(token, path string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+path, nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := res.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return res, b.String()
	}
	if res, body := bearer(scoped, "/"); res.StatusCode != http.StatusOK || body != "the secret page" {
		t.Errorf("a gate token: %d %q", res.StatusCode, body)
	}
	if _, body := bearer(scoped, "/headers"); strings.Contains(body, scoped) || !strings.Contains(body, "member@example.com") {
		t.Errorf("what the app heard of a token: %s", body)
	}
	readOnly := tokenOf(t, member, "read")
	if res, _ := bearer(readOnly, "/"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a token of another scope: %d", res.StatusCode)
	}
	if res, _ := bearer("moor_nonsense", "/"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a token that isn't: %d", res.StatusCode)
	}
	if res, body := bearer("somebody-elses-scheme", "/"); res.StatusCode != http.StatusUnauthorized || strings.Contains(body, "secret") {
		t.Errorf("another kind of bearer: %d", res.StatusCode)
	}
}

// TestShareLinks reaches an environment by a link, however it is gated, until the link ends.
func TestShareLinks(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()
	gatedApp(t, cfg, addrs, c, "shop")
	setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "share"})
	const host = "shop.localhost"

	browser := newVisitor(t, addrs, host)
	if res, page := browser.get("/"); res.StatusCode != http.StatusForbidden || strings.Contains(page, "secret") {
		t.Errorf("without a link: %d\n%s", res.StatusCode, page)
	}
	link, err := c.CreateShareLink(ctx, "shop", "production", apitypes.NewShareLink{Expires: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link.URL)
	if err != nil || u.Hostname() != host || !strings.HasPrefix(u.Path, "/.moorage/share/") {
		t.Fatalf("the link: %s %v", link.URL, err)
	}
	res, _ := browser.get(u.Path)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" || res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("redeeming it: %d %v", res.StatusCode, res.Header)
	}
	if res, body := browser.get("/"); res.StatusCode != http.StatusOK || body != "the secret page" {
		t.Errorf("after redeeming it: %d %q", res.StatusCode, body)
	}
	list, err := c.ShareLinks(ctx, "shop", "production")
	if err != nil || len(list) != 1 || list[0].Uses != 1 || list[0].URL != "" {
		t.Errorf("the list keeps no address: %+v %v", list, err)
	}

	// Only a hash of it is kept.
	db := testAuth(t, cfg).DB
	if _, err := db.ShareLinkByToken(ctx, strings.TrimPrefix(u.Path, "/.moorage/share/")); err == nil {
		t.Errorf("the token is kept as it is")
	}

	// It does not work on another app.
	if _, err := deployApp(t, c, "other", appPlan(map[string]string{"TESTAPP_BODY": "another secret"})); err != nil {
		t.Fatal(err)
	}
	setGate(t, c, "other", "production", apitypes.AccessChange{Mode: "share"})
	if res, _ := newVisitor(t, addrs, "other.localhost").get(u.Path); res.StatusCode != http.StatusGone {
		t.Errorf("a link to another app: %d", res.StatusCode)
	}

	// A limited one waits for a click, so that what previews an address does not use it up.
	once, err := c.CreateShareLink(ctx, "shop", "production", apitypes.NewShareLink{MaxUses: 1})
	if err != nil {
		t.Fatal(err)
	}
	oncePath := mustPath(t, once.URL)
	first := newVisitor(t, addrs, host)
	res, page := first.get(oncePath)
	if res.StatusCode != http.StatusOK || cookieNamed(res, "moorage_gate") != nil {
		t.Errorf("a limited link, fetched: %d %v", res.StatusCode, res.Header)
	}
	contains(t, "the page that asks", page, "Open it", `method="post"`)
	if res, _ := first.do(http.MethodPost, oncePath, url.Values{}, nil); res.StatusCode != http.StatusSeeOther {
		t.Errorf("a limited link, used: %d", res.StatusCode)
	}
	if res, _ := newVisitor(t, addrs, host).do(http.MethodPost, oncePath, url.Values{}, nil); res.StatusCode != http.StatusGone {
		t.Errorf("a limited link, used up: %d", res.StatusCode)
	}

	// It gets through other gates too, and an expired one nowhere.
	setGate(t, c, "shop", "production", apitypes.AccessChange{Mode: "password", Password: "correct horse"})
	third := newVisitor(t, addrs, host)
	if res, _ := third.get(u.Path); res.StatusCode != http.StatusSeeOther {
		t.Errorf("a link through the password gate: %d", res.StatusCode)
	}
	if res, body := third.get("/"); res.StatusCode != http.StatusOK || body != "the secret page" {
		t.Errorf("after it: %d", res.StatusCode)
	}
	if _, err := c.CreateShareLink(ctx, "shop", "production", apitypes.NewShareLink{Expires: "500d"}); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("a link too long: %v", err)
	}

	// The console makes them, and shows them once; revoking ends them, and whom they let in.
	token := b.token()
	res, page = b.post("/apps/shop/share-links", url.Values{"csrf": {token}, "expires": {"1d"}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("making a link in the console: %d\n%s", res.StatusCode, page)
	}
	contains(t, "the new link", page, "/.moorage/share/", "shown only now")
	_, page = b.get("/apps/shop/access")
	if strings.Contains(page, "/.moorage/share/") {
		t.Errorf("the link is shown again")
	}
	contains(t, "the access page", page, "A password", "Revoke", "Make a link")
	if err := c.RevokeShareLink(ctx, "shop", "production", list[0].ID); err != nil {
		t.Fatal(err)
	}
	if res, _ := newVisitor(t, addrs, host).get(u.Path); res.StatusCode != http.StatusGone {
		t.Errorf("a revoked link: %d", res.StatusCode)
	}

	// The console changes the gate, as maintainers alone may.
	if res, body := b.post("/apps/shop/access", url.Values{"csrf": {token}, "mode": {"public"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("changing the gate: %d\n%s", res.StatusCode, body)
	}
	if res, body := get(t, addrs.HTTP, host, "/"); res.StatusCode != http.StatusOK || body != "the secret page" {
		t.Errorf("public again: %d", res.StatusCode)
	}
}

var madeToken = regexp.MustCompile(`moor_[A-Za-z0-9_-]+`)

// tokenOf makes an API token in the console, as someone would.
func tokenOf(t *testing.T, b *consoleUser, scope string) string {
	t.Helper()
	res, page := b.post("/account/tokens/create", url.Values{"csrf": {b.token()}, "name": {"for " + scope}, "scope": {scope}})
	token := madeToken.FindString(page)
	if res.StatusCode != http.StatusCreated || token == "" {
		t.Fatalf("making a token: %d\n%s", res.StatusCode, page)
	}
	return token
}

func mustPath(t *testing.T, address string) string {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	return u.Path
}

func cookieNamed(res *http.Response, name string) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == name && c.Value != "" {
			return c
		}
	}
	return nil
}

// TestGateDefaults takes gates from the server and the manifest, and from an allow-list.
func TestGateDefaults(t *testing.T) {
	cfg := testConfig(t)
	cfg.Access.Preview = "login"
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	site := map[string]string{"index.html": "<p>hello</p>\n", "moorage.toml": "[app]\nruntime = \"static\"\n\n[environments.staging]\nbranch = \"develop\"\naccess = \"share\"\n"}
	repo, resolve := pushRepo(t, cfg, addrs, "docs", site)
	repo.must(append(append([]string{}, resolve...), "push", "moorage", "main")...)

	// Production is public by default, even as a static site, and previews are not.
	if res, _ := get(t, addrs.HTTP, "docs.localhost", "/"); res.StatusCode != http.StatusOK {
		t.Errorf("production: %d", res.StatusCode)
	}
	repo.must("switch", "-q", "-c", "a-branch")
	repo.commitAndPush(resolve, "On a branch", "a-branch")
	preview := newVisitor(t, addrs, "docs--a-branch.localhost")
	if res, page := preview.get("/"); res.StatusCode != http.StatusFound || strings.Contains(page, "hello") {
		t.Errorf("a preview: %d\n%s", res.StatusCode, page)
	}
	if a, err := c.Access(ctx, "docs", "a-branch"); err != nil || a.Mode != "login" || a.Source != "server" {
		t.Errorf("a preview's gate: %+v %v", a, err)
	}

	// A manifest is a deployer's: it can ask for a gate, and not open what the server has closed.
	repo.write("moorage.toml", "[app]\nruntime = \"static\"\n\n[previews]\naccess = \"public\"\n\n[environments.staging]\nbranch = \"develop\"\naccess = \"share\"\n")
	repo.commitAndPush(resolve, "Open it", "a-branch")
	if a, err := c.Access(ctx, "docs", "a-branch"); err != nil || a.Mode != "login" || !a.AskedInVain {
		t.Errorf("a manifest that opens a gate: %+v %v", a, err)
	}
	if res, page := newVisitor(t, addrs, "docs--a-branch.localhost").get("/"); res.StatusCode != http.StatusFound || strings.Contains(page, "hello") {
		t.Errorf("a manifest that opens a gate, served: %d", res.StatusCode)
	}

	// The manifest asks for one, and a maintainer can say otherwise.
	repo.must("switch", "-q", "-c", "develop", "main")
	repo.commitAndPush(resolve, "On develop", "develop")
	if a, err := c.Access(ctx, "docs", "staging"); err != nil || a.Mode != "share" || a.Source != "manifest" {
		t.Errorf("what the manifest asks: %+v %v", a, err)
	}
	if res, _ := newVisitor(t, addrs, "docs--staging.localhost").get("/"); res.StatusCode != http.StatusForbidden {
		t.Errorf("what the manifest asks, served: %d", res.StatusCode)
	}
	if a := setGate(t, c, "docs", "staging", apitypes.AccessChange{Mode: "public"}); a.Source != "setting" {
		t.Errorf("a maintainer's say: %+v", a)
	}
	if res, _ := newVisitor(t, addrs, "docs--staging.localhost").get("/"); res.StatusCode != http.StatusOK {
		t.Errorf("after a maintainer said: %d", res.StatusCode)
	}

	// An allow-list goes with any gate, and turns away those outside it, as it stands now.
	if _, err := c.SetAccess(ctx, "docs", "staging", apitypes.AccessChange{Mode: "public", Allow: new([]string{"nonsense"})}); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("a list that makes no sense: %v", err)
	}
	setGate(t, c, "docs", "staging", apitypes.AccessChange{Mode: "public", Allow: new([]string{"192.0.2.0/24"})})
	if res, page := newVisitor(t, addrs, "docs--staging.localhost").get("/"); res.StatusCode != http.StatusForbidden || strings.Contains(page, "hello") {
		t.Errorf("from outside the list: %d", res.StatusCode)
	}
	// Changing the gate leaves it as it was, unless told otherwise, so as not to open anything.
	if a := setGate(t, c, "docs", "staging", apitypes.AccessChange{Mode: "share"}); len(a.Allow) != 1 {
		t.Errorf("the list after changing the gate: %+v", a)
	}
	if a := setGate(t, c, "docs", "staging", apitypes.AccessChange{Mode: "public", Allow: new([]string{"127.0.0.1", "::1"})}); len(a.Allow) != 2 {
		t.Errorf("the list: %+v", a)
	}
	if res, _ := newVisitor(t, addrs, "docs--staging.localhost").get("/"); res.StatusCode != http.StatusOK {
		t.Errorf("from inside the list: %d", res.StatusCode)
	}

	// With a password gate and none set, nobody gets in.
	files := map[string]string{"index.html": "<p>hello</p>\n", "moorage.toml": "[app]\nruntime = \"static\"\n\n[environments.production]\naccess = \"password\"\n"}
	d, _ := deploySource(t, c, "locked", files)
	if d.Status != apitypes.StatusLive {
		t.Fatalf("%+v", d)
	}
	if a, err := c.Access(ctx, "locked", "production"); err != nil || !a.PasswordMissing {
		t.Errorf("no password: %+v %v", a, err)
	}
	if res, page := newVisitor(t, addrs, "locked.localhost").get("/"); res.StatusCode != http.StatusServiceUnavailable || strings.Contains(page, "hello") {
		t.Errorf("a password gate with no password: %d\n%s", res.StatusCode, page)
	}
}

// TestGateKeepsIdleAsleep shows that those a gate turns away neither wake an environment, nor
// keep it from sleeping.
func TestGateKeepsIdleAsleep(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	files := sleepyManifest("0s")
	files["moorage.toml"] = strings.Replace(files["moorage.toml"], `sleep_after = "1s"`, `sleep_after = "1s"`+"\naccess = \"share\"", 1)
	deployedSource(t, c, "shop", files)
	knock := newVisitor(t, addrs, "shop.localhost")
	waitUntil(t, "it to go to sleep while someone knocks", func() bool {
		knock.get("/")
		return asleep(t, c, "shop")
	})
	if res, _ := knock.get("/"); res.StatusCode != http.StatusForbidden {
		t.Errorf("knocking: %d", res.StatusCode)
	}
	if !asleep(t, c, "shop") {
		t.Errorf("knocking woke it")
	}
	waitUntil(t, "its web process to stop", func() bool { return processes(instances(t, c, "shop")) == "worker" })
	knock.get("/")
	if processes(instances(t, c, "shop")) != "worker" {
		t.Errorf("its instances: %+v", instances(t, c, "shop"))
	}
}
