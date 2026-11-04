package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/config"
)

var (
	totpKeyInPage   = regexp.MustCompile(`[A-Z2-7]{32}`)
	recoveryInPage  = regexp.MustCompile(`[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}`)
	secondFactorKey = func(t *testing.T, page string) string {
		t.Helper()
		key := totpKeyInPage.FindString(page)
		if key == "" {
			t.Fatalf("no key in:\n%s", page)
		}
		return key
	}
)

func codeFor(t *testing.T, key string, at time.Time) string {
	t.Helper()
	code, err := auth.CodeFor(key, at)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// freshBrowser signs in with a password alone, as far as that goes.
func freshBrowser(t *testing.T, cfg config.Config, addrs Addrs, email, password string) (*consoleUser, *http.Response) {
	t.Helper()
	b := &consoleUser{t: t, addr: addrs.HTTP, host: cfg.ConsoleHost}
	res, body := b.post("/login", url.Values{"email": {email}, "password": {password}})
	if res.StatusCode == http.StatusUnauthorized {
		t.Fatalf("signing in: %s", body)
	}
	return b, res
}

func apiLogin(t *testing.T, addrs Addrs, cfg config.Config, body map[string]string) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addrs.HTTP.String()+"/api/v1/auth/login", strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = cfg.ConsoleHost
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out strings.Builder
	buf := make([]byte, 4096)
	n, _ := res.Body.Read(buf)
	out.Write(buf[:n])
	return res.StatusCode, out.String()
}

// TestTwoFactorSignIn goes through setting it up, signing in with it and every way around it.
func TestTwoFactorSignIn(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	const email, password = "owner@example.com", "a long and unusual passphrase"

	// Nothing to do until someone wants it.
	if res, page := owner.get("/account/security"); res.StatusCode != http.StatusOK || !strings.Contains(page, "Set it up") {
		t.Fatalf("%d\n%s", res.StatusCode, page)
	}
	if _, res := freshBrowser(t, cfg, addrs, email, password); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("before: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// Setting it up shows a code to scan and a key to type, and a wrong code does not turn it on.
	if res, page := owner.post("/account/security/start", url.Values{"csrf": {owner.token()}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d\n%s", res.StatusCode, page)
	}
	_, page := owner.get("/account/security")
	if !strings.Contains(page, "<svg") || !strings.Contains(page, "<path") {
		t.Errorf("no QR code:\n%s", page)
	}
	key := secondFactorKey(t, page)
	if res, page := owner.post("/account/security/confirm", url.Values{"csrf": {owner.token()}, "code": {"000000"}}); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, "not right") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	if _, res := freshBrowser(t, cfg, addrs, email, password); res.Header.Get("Location") != "/" {
		t.Errorf("a wrong code turned it on: %s", res.Header.Get("Location"))
	}

	// The secret is sealed.
	var sealed []byte
	if err := testAuth(t, cfg).DB.Reader.QueryRowContext(ctx, `SELECT sealed FROM user_totp`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), key) || len(sealed) < 40 {
		t.Errorf("the secret is kept in the clear: %q", sealed)
	}

	// The right code turns it on, and recovery codes are shown once.
	first := codeFor(t, key, time.Now())
	res, page := owner.post("/account/security/confirm", url.Values{"csrf": {owner.token()}, "code": {first}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d\n%s", res.StatusCode, page)
	}
	recovery := recoveryInPage.FindAllString(page, -1)
	if len(recovery) != 10 {
		t.Fatalf("%d recovery codes:\n%s", len(recovery), page)
	}
	if _, again := owner.get("/account/security"); recoveryInPage.MatchString(again) || totpKeyInPage.MatchString(again) || !strings.Contains(again, "10 recovery codes") {
		t.Errorf("shown again:\n%s", again)
	}
	// Whoever was signed in stays so.
	if res, _ := owner.get("/account"); res.StatusCode != http.StatusOK {
		t.Errorf("%d", res.StatusCode)
	}

	// A password is now not enough: that gets a browser to the code, and nowhere else.
	b, res := freshBrowser(t, cfg, addrs, email, password)
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/login/code") {
		t.Fatalf("%d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := b.get("/account"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("signed in with a password alone: %d", res.StatusCode)
	}
	if res, page := b.get("/login/code"); res.StatusCode != http.StatusOK || !strings.Contains(page, "authenticator") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	for _, wrong := range []string{"000000", first, "abcd-efgh-ijkl-mnop"} { // the code that turned it on has been used
		if res, _ := patiently(t, func() (*http.Response, string) { return b.post("/login/code", url.Values{"code": {wrong}}) }); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%q: %d", wrong, res.StatusCode)
		}
	}
	if res, _ := b.get("/account"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("signed in by a wrong code: %d", res.StatusCode)
	}

	// A recovery code gets in, once. So does the next code.
	if res, page := patiently(t, func() (*http.Response, string) {
		return b.post("/login/code", url.Values{"code": {strings.ToUpper(recovery[0])}})
	}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("a recovery code: %d\n%s", res.StatusCode, page)
	}
	if res, _ := b.get("/account"); res.StatusCode != http.StatusOK {
		t.Errorf("%d", res.StatusCode)
	}
	used, _ := freshBrowser(t, cfg, addrs, email, password)
	if res, _ := patiently(t, func() (*http.Response, string) { return used.post("/login/code", url.Values{"code": {recovery[0]}}) }); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a recovery code twice: %d", res.StatusCode)
	}
	if res, page := patiently(t, func() (*http.Response, string) {
		return used.post("/login/code", url.Values{"code": {codeFor(t, key, time.Now().Add(30*time.Second))}})
	}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("the next code: %d\n%s", res.StatusCode, page)
	}

	// Without the password's step, a code goes nowhere.
	stranger := &consoleUser{t: t, addr: addrs.HTTP, host: cfg.ConsoleHost}
	if res, _ := stranger.post("/login/code", url.Values{"code": {recovery[1]}}); res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/login") {
		t.Errorf("%d %s", res.StatusCode, res.Header.Get("Location"))
	}
	stranger.cookies = []*http.Cookie{{Name: "moorage_pending", Value: "usr_x.99999999999.forged"}}
	if res, _ := stranger.post("/login/code", url.Values{"code": {recovery[1]}}); res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/login") {
		t.Errorf("a forged cookie: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// The API asks the same.
	login := func(body map[string]string) (int, string) {
		t.Helper()
		res, text := patiently(t, func() (*http.Response, string) {
			status, text := apiLogin(t, addrs, cfg, body)
			return &http.Response{StatusCode: status}, text
		})
		return res.StatusCode, text
	}
	if status, body := login(map[string]string{"email": email, "password": password}); status != http.StatusUnauthorized || !strings.Contains(body, "second_factor_needed") {
		t.Errorf("%d %s", status, body)
	}
	if status, body := login(map[string]string{"email": email, "password": password, "code": "000000"}); status != http.StatusUnauthorized || !strings.Contains(body, "bad_second_factor") {
		t.Errorf("%d %s", status, body)
	}
	if status, body := login(map[string]string{"email": email, "password": password, "code": recovery[1]}); status != http.StatusOK {
		t.Errorf("%d %s", status, body)
	}

	// A link for a new password does not get round it.
	res, page = owner.post("/settings/users/invite", url.Values{"csrf": {owner.token()}, "user": {userIDOf(t, cfg, email)}})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("%d\n%s", res.StatusCode, page)
	}
	link := regexp.MustCompile(`/invite\?code=([^<"\s]+)`).FindStringSubmatch(page)
	if link == nil {
		t.Fatalf("no link:\n%s", page)
	}
	next := &consoleUser{t: t, addr: addrs.HTTP, host: cfg.ConsoleHost}
	const newPassword = "another long and unusual passphrase"
	if res, body := next.post("/invite", url.Values{"code": {link[1]}, "password": {newPassword}, "confirm": {newPassword}}); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
		t.Errorf("%d %s\n%s", res.StatusCode, res.Header.Get("Location"), body)
	}
	if res, _ := next.get("/account"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("signed in by a link: %d", res.StatusCode)
	}

	// Someone else's turn: an administrator turns it off for one who has lost everything.
	sam := addTeammate(t, cfg, addrs, adminAfresh(t, cfg, addrs, email, newPassword, recovery[2]), "sam@example.com")
	if res, _ := sam.post("/settings/users/second-factor-off", url.Values{"csrf": {sam.token()}, "user": {userIDOf(t, cfg, email)}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a member turning it off for an administrator: %d", res.StatusCode)
	}
	admin := adminAfresh(t, cfg, addrs, email, newPassword, recovery[3])
	if _, page := admin.get("/settings/users"); !strings.Contains(page, "Two-factor") {
		t.Errorf("%s", page)
	}

	// Turning it off takes the password, even signed in.
	if res, page := admin.post("/account/security/off", url.Values{"csrf": {admin.token()}, "password": {"not the password at all"}}); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, "not your password") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	if res, page := admin.post("/account/security/off", url.Values{"csrf": {admin.token()}, "password": {newPassword}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	if _, res := freshBrowser(t, cfg, addrs, email, newPassword); res.Header.Get("Location") != "/" {
		t.Errorf("after turning it off: %s", res.Header.Get("Location"))
	}
}

// patiently asks again for as long as the limit on wrong codes says to wait, which is what it is for.
func patiently[T any](t *testing.T, ask func() (*http.Response, T)) (*http.Response, T) {
	t.Helper()
	for range 40 {
		res, got := ask()
		if res.StatusCode != http.StatusTooManyRequests {
			return res, got
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("still too many attempts")
	panic("unreachable")
}

func adminAfresh(t *testing.T, cfg config.Config, addrs Addrs, email, password, recovery string) *consoleUser {
	t.Helper()
	b, res := freshBrowser(t, cfg, addrs, email, password)
	if !strings.HasPrefix(res.Header.Get("Location"), "/login/code") {
		t.Fatalf("%s", res.Header.Get("Location"))
	}
	if res, page := patiently(t, func() (*http.Response, string) { return b.post("/login/code", url.Values{"code": {recovery}}) }); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d\n%s", res.StatusCode, page)
	}
	return b
}

func userIDOf(t *testing.T, cfg config.Config, email string) string {
	t.Helper()
	u, err := testAuth(t, cfg).DB.UserByEmail(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}
