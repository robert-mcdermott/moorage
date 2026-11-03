package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/store"
)

const goodPassword = "a long and unusual passphrase"

func service(t *testing.T) *Service {
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
	return &Service{DB: db, LocalToken: Hash("moor_local"), ClaimFile: filepath.Join(dir, "keys", "claim-code"), Limits: NewLimiter()}
}

func addUser(t *testing.T, s *Service, email, role string) store.User {
	t.Helper()
	u, err := s.NewUser(context.Background(), email, "", role, goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPasswords(t *testing.T) {
	ctx := context.Background()
	hash, err := HashPassword(ctx, goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("hash: %s", hash)
	}
	if ok, err := VerifyPassword(ctx, hash, goodPassword); err != nil || !ok {
		t.Errorf("the right password: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword(ctx, hash, goodPassword+"x"); ok {
		t.Error("a wrong password matched")
	}
	if other, _ := HashPassword(ctx, goodPassword); other == hash {
		t.Error("two hashes share a salt")
	}
	if _, err := VerifyPassword(ctx, "not a hash", "x"); err == nil {
		t.Error("nonsense verified")
	}
	for password, ok := range map[string]bool{
		goodPassword:                true,
		"short":                     false,
		"elevenchars":               false,
		"exactlytwelv":              true,
		"Password123456":            false,
		"CORRECTHORSEBATTERYSTAPLE": false,
		"aaaaaaaaaaaaaaaa":          false,
		"abcdabcdabcdabcd":          false,
		strings.Repeat("xy1", 500):  false,
	} {
		if err := CheckPassword(password); (err == nil) != ok {
			t.Errorf("CheckPassword(%.20q) = %v, want ok=%v", password, err, ok)
		}
	}
	for line := range strings.SplitSeq(commonPasswordList, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") && len(line) < MinPasswordLength {
			t.Errorf("%q is too short to need listing", line)
		}
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter()
	l.now = func() time.Time { return now }
	for range 5 {
		if w := l.Wait("k"); w != 0 {
			t.Fatalf("a free attempt waits %s", w)
		}
		l.Fail("k")
	}
	if w := l.Wait("k"); w != 0 {
		t.Errorf("after five failures: %s", w)
	}
	l.Fail("k")
	if w := l.Wait("k"); w != time.Second {
		t.Errorf("the sixth failure: %s", w)
	}
	now = now.Add(2 * time.Second)
	if w := l.Wait("k"); w != 0 {
		t.Errorf("after the wait: %s", w)
	}
	l.Fail("k")
	if w := l.Wait("k"); w != 2*time.Second {
		t.Errorf("the seventh failure: %s", w)
	}
	for range 40 {
		l.Fail("k")
	}
	if w := l.Wait("k"); w != 15*time.Minute {
		t.Errorf("at most: %s", w)
	}
	if w := l.Wait("other"); w != 0 {
		t.Errorf("another key: %s", w)
	}
	l.Clear("k")
	if w := l.Wait("k"); w != 0 {
		t.Errorf("after a success: %s", w)
	}
	l.Fail("old")
	for range 6 {
		l.Fail("old")
	}
	now = now.Add(3 * time.Hour)
	if w := l.Wait("old"); w != 0 || len(l.entries) != 0 {
		t.Errorf("a key not seen for hours: %s, %d entries", w, len(l.entries))
	}
}

func TestLocalToken(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	// Until the server has accounts, the admin token works from anywhere.
	p, err := s.Token(ctx, " moor_local\n", "192.0.2.9")
	if err != nil || !p.Local() || !p.Admin || p.Name != LocalAdmin || !p.Can(ScopeAdmin) {
		t.Fatalf("before the claim: %+v %v", p, err)
	}
	addUser(t, s, "a@example.com", store.RoleAdmin)
	if _, err := s.Token(ctx, "moor_local", "192.0.2.9"); !errors.Is(err, ErrLocalOnly) {
		t.Errorf("from another machine, after the claim: %v", err)
	}
	for _, ip := range []string{"127.0.0.1", "::1"} {
		if _, err := s.Token(ctx, "moor_local", ip); err != nil {
			t.Errorf("from %s: %v", ip, err)
		}
	}
	if _, err := s.Token(ctx, "moor_wrong", "127.0.0.1"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a wrong token: %v", err)
	}
}

func addToken(t *testing.T, s *Service, userID, name, app string, expires time.Time, scopes ...string) string {
	t.Helper()
	token := NewToken()
	err := s.DB.CreateToken(context.Background(), store.APIToken{
		ID: ids.New(ids.Token), UserID: userID, Name: name, TokenHash: Hex(token), Scopes: scopes, App: app, ExpiresAt: expires, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAPITokens(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	s.DB.EnsureApp(ctx, "shop")
	admin := addUser(t, s, "admin@example.com", store.RoleAdmin)
	member := addUser(t, s, "member@example.com", store.RoleMember)

	read := addToken(t, s, member.ID, "read", "", time.Time{}, ScopeRead)
	p, err := s.Token(ctx, read, "192.0.2.1")
	if err != nil || p.UserID != member.ID || p.Name != "member@example.com" || p.Admin || p.Local() {
		t.Fatalf("a member's token: %+v %v", p, err)
	}
	if !p.Can(ScopeRead) || p.Can(ScopeDeploy) || p.Can(ScopeAdmin) {
		t.Errorf("a read token's scopes: %+v", p)
	}
	tokens, _ := s.DB.Tokens(ctx, member.ID)
	if tokens[0].LastUsedAt.IsZero() {
		t.Error("the use was not recorded")
	}

	deploy := addToken(t, s, member.ID, "deploy", "shop", time.Time{}, ScopeDeploy)
	p, _ = s.Token(ctx, deploy, "192.0.2.1")
	if !p.Can(ScopeRead) || !p.Can(ScopeDeploy) || p.Can(ScopeAdmin) || !p.CanApp("shop") || p.CanApp("other") {
		t.Errorf("a deploy token for one app: %+v", p)
	}
	gate := addToken(t, s, member.ID, "gate", "", time.Time{}, ScopeGate)
	if p, _ := s.Token(ctx, gate, "192.0.2.1"); p.Can(ScopeRead) || !p.Can(ScopeGate) {
		t.Errorf("a gate token: %+v", p)
	}
	full := addToken(t, s, admin.ID, "all", "", time.Time{}, ScopeAdmin)
	if p, _ := s.Token(ctx, full, "192.0.2.1"); !p.Admin || !p.Can(ScopeRead) || !p.Can(ScopeDeploy) || p.Can(ScopeGate) {
		t.Errorf("an admin's admin token: %+v", p)
	}

	// A token of the local administrator works from anywhere: it is not the
	// admin token, and it can be revoked.
	localOwned := addToken(t, s, "", "ci", "", time.Time{}, ScopeDeploy)
	if p, err := s.Token(ctx, localOwned, "192.0.2.1"); err != nil || !p.Local() || !p.Can(ScopeDeploy) || p.Can(ScopeAdmin) {
		t.Errorf("a token of the local administrator: %+v %v", p, err)
	}

	expired := addToken(t, s, member.ID, "old", "", time.Now().Add(-time.Minute), ScopeRead)
	if _, err := s.Token(ctx, expired, "192.0.2.1"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("an expired token: %v", err)
	}
	if err := s.DB.SetUserDisabled(ctx, member.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(ctx, read, "192.0.2.1"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a disabled user's token: %v", err)
	}
}

func TestSSHKeyPrincipals(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	addUser(t, s, "admin@example.com", store.RoleAdmin)
	member := addUser(t, s, "member@example.com", store.RoleMember)
	s.DB.AddSSHKey(ctx, store.SSHKey{ID: ids.New(ids.SSHKey), UserID: member.ID, Name: "m", Fingerprint: "fpm", PublicKey: "k", CreatedAt: time.Now()})
	s.DB.AddSSHKey(ctx, store.SSHKey{ID: ids.New(ids.SSHKey), Name: "l", Fingerprint: "fpl", PublicKey: "k", CreatedAt: time.Now()})
	if p, k, err := s.Key(ctx, "fpm"); err != nil || p.Name != "member@example.com" || p.Admin || k.Name != "m" {
		t.Errorf("a member's key: %+v %+v %v", p, k, err)
	}
	if p, _, err := s.Key(ctx, "fpl"); err != nil || !p.Local() || !p.Admin {
		t.Errorf("the local administrator's key: %+v %v", p, err)
	}
	if _, _, err := s.Key(ctx, "nope"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("an unknown key: %v", err)
	}
	s.DB.SetUserDisabled(ctx, member.ID, true)
	if _, _, err := s.Key(ctx, "fpm"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a disabled user's key: %v", err)
	}
}

func TestSessions(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	u := addUser(t, s, "a@example.com", store.RoleMember)
	now := time.Now()
	s.Now = func() time.Time { return now }
	value, err := s.StartSession(ctx, u, "192.0.2.1", "agent")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := s.DB.UserSessions(ctx, u.ID)
	if len(stored) != 1 || stored[0].IDHash == value || strings.Contains(stored[0].IDHash, value) {
		t.Fatalf("stored: %+v", stored)
	}
	p, err := s.Session(ctx, value)
	if err != nil || p.UserID != u.ID || p.SessionID != stored[0].ID || p.Scopes != nil {
		t.Fatalf("session: %+v %v", p, err)
	}
	if _, err := s.Session(ctx, "wrong"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a wrong cookie: %v", err)
	}

	// Idle for too long: it ends. A session in use lasts, up to its limit.
	now = now.Add(sessionIdle + time.Hour)
	if _, err := s.Session(ctx, value); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("an idle session: %v", err)
	}
	now = time.Now()
	value, _ = s.StartSession(ctx, u, "192.0.2.1", "agent")
	for range 5 {
		now = now.Add(sessionIdle - time.Hour)
		// The session's last use is recorded with the real clock, so bring it
		// forward as a request at that moment would.
		sess, err := s.DB.SessionByHash(ctx, Hex(value))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Writer.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, now.UnixMilli(), sess.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Session(ctx, value); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("past the session's lifetime: %v", err)
	}

	now = time.Now()
	value, _ = s.StartSession(ctx, u, "192.0.2.1", strings.Repeat("a", 1000))
	if err := s.EndSession(ctx, value); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, value); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("an ended session: %v", err)
	}
}

func TestCookieAndCSRF(t *testing.T) {
	s := &Service{Secure: true}
	c := s.SessionCookie("value")
	if c.Name != "__Host-moorage_session" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Errorf("over HTTPS: %+v", c)
	}
	if c := s.SessionCookie(""); c.MaxAge >= 0 {
		t.Errorf("the removal: %+v", c)
	}
	s.Secure = false
	if c := s.SessionCookie("value"); c.Name != "moorage_session" || c.Secure || !c.HttpOnly {
		t.Errorf("over plain HTTP: %+v", c)
	}
	token := CSRFToken("cookie")
	if !CheckCSRF("cookie", token) || CheckCSRF("other", token) || CheckCSRF("cookie", "") || strings.Contains(token, "cookie") {
		t.Errorf("CSRF token %q", token)
	}
	for _, tc := range []struct {
		site, origin string
		want         bool
	}{
		{"same-origin", "", true},
		{"none", "", true},
		{"same-site", "", false}, // an app on a sibling hostname
		{"cross-site", "", false},
		{"", "https://moorage.example.com", true},
		{"", "https://shop.example.com", false},
		{"", "null", false},
		{"", "", true},
	} {
		r := httptest.NewRequest(http.MethodPost, "https://moorage.example.com/x", nil)
		if tc.site != "" {
			r.Header.Set("Sec-Fetch-Site", tc.site)
		}
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := SameOrigin(r); got != tc.want {
			t.Errorf("site %q origin %q: %v", tc.site, tc.origin, got)
		}
	}
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("10.0.0.0/8")}
	for _, tc := range []struct {
		peer, xff, want string
	}{
		{"192.0.2.1:5000", "", "192.0.2.1"},
		{"192.0.2.1:5000", "6.6.6.6", "192.0.2.1"}, // a forgery: the peer is no proxy
		{"127.0.0.1:5000", "", "127.0.0.1"},
		{"127.0.0.1:5000", "203.0.113.5", "203.0.113.5"},
		{"127.0.0.1:5000", "6.6.6.6, 203.0.113.5", "203.0.113.5"}, // the client's own claim is not believed
		{"127.0.0.1:5000", "203.0.113.5, 10.1.1.1", "203.0.113.5"},
		{"[::1]:5000", "203.0.113.5", "::1"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.peer
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := ClientIP(r, trusted); got != tc.want {
			t.Errorf("peer %s, forwarded %q: %s, want %s", tc.peer, tc.xff, got, tc.want)
		}
	}
}

func TestSignIn(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	u := addUser(t, s, "a@example.com", store.RoleMember)
	if got, err := s.SignIn(ctx, " A@Example.com ", goodPassword, "192.0.2.1"); err != nil || got.ID != u.ID {
		t.Fatalf("the right password: %+v %v", got, err)
	}
	if _, err := s.SignIn(ctx, "a@example.com", "wrong", "192.0.2.1"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("a wrong password: %v", err)
	}
	if _, err := s.SignIn(ctx, "nobody@example.com", goodPassword, "192.0.2.2"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("an unknown address: %v", err)
	}
	invited := store.User{ID: ids.New(ids.User), Email: "new@example.com", Name: "New", Role: store.RoleMember, CreatedAt: time.Now()}
	s.DB.CreateUser(ctx, invited)
	if _, err := s.SignIn(ctx, "new@example.com", "", "192.0.2.3"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("an account without a password: %v", err)
	}
	addUser(t, s, "admin@example.com", store.RoleAdmin)
	s.DB.SetUserDisabled(ctx, u.ID, true)
	if _, err := s.SignIn(ctx, "a@example.com", goodPassword, "192.0.2.4"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("a disabled account: %v", err)
	}
}

func TestSignInLimits(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	addUser(t, s, "a@example.com", store.RoleMember)
	var limited *RateLimitedError
	// Failures from many addresses count against the account...
	for i := range 6 {
		if _, err := s.SignIn(ctx, "a@example.com", "wrong", "192.0.2."+string(rune('1'+i))); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := s.SignIn(ctx, "a@example.com", goodPassword, "198.51.100.1"); !errors.As(err, &limited) {
		t.Errorf("the account, even with the right password: %v", err)
	}
	// ...and failures from one address count against it, whoever they name.
	for i := range 6 {
		s.SignIn(ctx, "user"+string(rune('a'+i))+"@example.com", "wrong", "198.51.100.2")
	}
	if _, err := s.SignIn(ctx, "other@example.com", "wrong", "198.51.100.2"); !errors.As(err, &limited) {
		t.Errorf("the address: %v", err)
	}
}

func TestClaim(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	code, err := s.PrepareClaim(ctx)
	if err != nil || code == "" {
		t.Fatalf("prepare: %q %v", code, err)
	}
	if again, _ := s.PrepareClaim(ctx); again != code {
		t.Errorf("the code changed across starts: %q, %q", code, again)
	}
	if fi, err := os.Stat(s.ClaimFile); err != nil || (fi.Mode().Perm()&0o077 != 0 && !isWindows()) {
		t.Errorf("the file: %v %v", fi, err)
	}
	if _, err := s.Claim(ctx, "wrong", "192.0.2.1", "a@example.com", "A", goodPassword); !errors.Is(err, ErrBadClaimCode) {
		t.Errorf("a wrong code: %v", err)
	}
	if _, err := s.Claim(ctx, "", "192.0.2.2", "a@example.com", "A", goodPassword); !errors.Is(err, ErrBadClaimCode) {
		t.Errorf("no code: %v", err)
	}
	var field *FieldError
	if _, err := s.Claim(ctx, code, "192.0.2.3", "a@example.com", "A", "short"); !errors.As(err, &field) || field.Field != "password" {
		t.Errorf("a short password: %v", err)
	}
	if _, err := s.Claim(ctx, code, "192.0.2.3", "not an address", "A", goodPassword); !errors.As(err, &field) || field.Field != "email" {
		t.Errorf("a bad address: %v", err)
	}
	u, err := s.Claim(ctx, code, "192.0.2.3", "A@Example.com", "", goodPassword)
	if err != nil || u.Role != store.RoleAdmin || u.Email != "a@example.com" || u.Name != "a" {
		t.Fatalf("the claim: %+v %v", u, err)
	}
	if _, err := os.Stat(s.ClaimFile); !os.IsNotExist(err) {
		t.Errorf("the code is still there: %v", err)
	}
	if _, err := s.Claim(ctx, code, "192.0.2.3", "b@example.com", "B", goodPassword); !errors.Is(err, ErrBadClaimCode) {
		t.Errorf("using the code again: %v", err)
	}
	if code, err := s.PrepareClaim(ctx); err != nil || code != "" {
		t.Errorf("a claimed server's code: %q %v", code, err)
	}
	// A code that a claimed server finds is removed at start.
	os.WriteFile(s.ClaimFile, []byte("stale\n"), 0o600)
	s.PrepareClaim(ctx)
	if _, err := os.Stat(s.ClaimFile); !os.IsNotExist(err) {
		t.Errorf("a stale code stayed: %v", err)
	}
}

func TestClaimLimit(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	code, _ := s.PrepareClaim(ctx)
	for range 6 {
		s.CheckClaimCode("wrong", "192.0.2.1")
	}
	var limited *RateLimitedError
	if err := s.CheckClaimCode(code, "192.0.2.1"); !errors.As(err, &limited) {
		t.Errorf("guessing, then the right code: %v", err)
	}
	if err := s.CheckClaimCode(code, "192.0.2.2"); err != nil {
		t.Errorf("another address: %v", err)
	}
}

func TestInvitations(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	u, err := s.NewUser(ctx, "new@example.com", "New", store.RoleMember, "")
	if err != nil || u.PasswordHash != "" {
		t.Fatalf("a new user: %+v %v", u, err)
	}
	s.DB.CreateUser(ctx, u)
	code, err := s.Invite(ctx, u.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var field *FieldError
	if _, err := s.AcceptInvitation(ctx, code, "short"); !errors.As(err, &field) {
		t.Errorf("a short password: %v", err)
	}
	if _, err := s.AcceptInvitation(ctx, "wrong", goodPassword); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a wrong code: %v", err)
	}
	if got, err := s.AcceptInvitation(ctx, code, goodPassword); err != nil || got.ID != u.ID {
		t.Fatalf("accepted: %+v %v", got, err)
	}
	if _, err := s.SignIn(ctx, "new@example.com", goodPassword, "192.0.2.1"); err != nil {
		t.Errorf("signing in afterwards: %v", err)
	}
	if _, err := s.AcceptInvitation(ctx, code, "another good passphrase"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("accepting twice: %v", err)
	}
}

func TestDeviceLogin(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	u := addUser(t, s, "a@example.com", store.RoleMember)
	now := time.Now()
	s.Now = func() time.Time { return now }
	code, userCode, err := s.StartDevice(ctx, "moor on laptop", "192.0.2.1")
	if err != nil || len(userCode) != 9 || userCode[4] != '-' {
		t.Fatalf("start: %q %q %v", code, userCode, err)
	}
	if _, _, err := s.PollDevice(ctx, code, "moor login"); !errors.Is(err, ErrDevicePending) {
		t.Errorf("before approval: %v", err)
	}
	if _, _, err := s.PollDevice(ctx, "wrong", "moor login"); !errors.Is(err, ErrDeviceExpired) {
		t.Errorf("a wrong code: %v", err)
	}
	if err := s.DB.DecideDevice(ctx, userCode, u.ID); err != nil {
		t.Fatal(err)
	}
	token, got, err := s.PollDevice(ctx, code, "moor login")
	if err != nil || got.ID != u.ID || !strings.HasPrefix(token, TokenPrefix) {
		t.Fatalf("after approval: %q %+v %v", token, got, err)
	}
	if p, err := s.Token(ctx, token, "192.0.2.1"); err != nil || p.UserID != u.ID || !p.Can(ScopeAdmin) {
		t.Errorf("the new token: %+v %v", p, err)
	}
	if _, _, err := s.PollDevice(ctx, code, "moor login"); !errors.Is(err, ErrDeviceExpired) {
		t.Errorf("the code a second time: %v", err)
	}

	// Polling too fast is slowed down.
	code, _, _ = s.StartDevice(ctx, "moor on laptop", "192.0.2.1")
	s.PollDevice(ctx, code, "x")
	if _, _, err := s.PollDevice(ctx, code, "x"); !errors.Is(err, ErrDeviceSlowDown) {
		t.Errorf("two quick polls: %v", err)
	}

	// Denied.
	code, userCode, _ = s.StartDevice(ctx, "moor on laptop", "192.0.2.1")
	s.DB.DecideDevice(ctx, userCode, "")
	if _, _, err := s.PollDevice(ctx, code, "x"); !errors.Is(err, ErrDeviceDenied) {
		t.Errorf("denied: %v", err)
	}

	// Expired.
	code, _, _ = s.StartDevice(ctx, "moor on laptop", "192.0.2.1")
	now = now.Add(DeviceCodeLife + time.Minute)
	if _, _, err := s.PollDevice(ctx, code, "x"); !errors.Is(err, ErrDeviceExpired) {
		t.Errorf("expired: %v", err)
	}
}

func isWindows() bool { return os.PathSeparator == '\\' }
