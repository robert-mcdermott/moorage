package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Token scopes: read ⊂ deploy ⊂ admin. A token can do what its scope allows
// and its user may do. Gate is for access gates (0.4.0), and grants nothing else.
const (
	ScopeRead   = "read"
	ScopeDeploy = "deploy"
	ScopeAdmin  = "admin"
	ScopeGate   = "gate"
)

// ValidScope reports whether a scope exists.
func ValidScope(s string) bool {
	return s == ScopeRead || s == ScopeDeploy || s == ScopeAdmin || s == ScopeGate
}

// LocalAdmin is the name of the server's local administrator, who holds the
// admin token on the server's machine and has no account.
const LocalAdmin = "admin"

const (
	sessionIdle     = 7 * 24 * time.Hour
	sessionLifetime = 30 * 24 * time.Hour
	touchEvery      = time.Minute

	// DeviceCodeLife is how long a CLI's sign-in waits for approval, and
	// DevicePollInterval how often its CLI may ask.
	DeviceCodeLife     = 10 * time.Minute
	DevicePollInterval = 2 * time.Second

	// InvitationLife is how long an invitation link works.
	InvitationLife = 7 * 24 * time.Hour
)

// Errors of authentication.
var (
	ErrBadCredentials  = errors.New("the email address or password is wrong")
	ErrUnauthenticated = errors.New("not signed in")
	// ErrLocalOnly is returned for the admin token from another machine, once
	// the server has accounts.
	ErrLocalOnly    = errors.New("the server's admin token works only on the server's own machine now that it has accounts: sign in with moor login, or use an API token")
	ErrBadClaimCode = errors.New("that setup link is not valid")

	ErrDevicePending  = errors.New("waiting for approval in the browser")
	ErrDeviceSlowDown = errors.New("polling too fast")
	ErrDeviceDenied   = errors.New("the sign-in was denied")
	ErrDeviceExpired  = errors.New("the sign-in expired")
)

// RateLimitedError is returned while a key must wait.
type RateLimitedError struct{ Wait time.Duration }

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("too many attempts: try again in %s", e.Wait.Round(time.Second))
}

// Principal is who a request is from.
type Principal struct {
	// UserID is empty for the server's local administrator.
	UserID string
	// Name identifies them in records: an email address, or LocalAdmin.
	Name string
	// Admin reports that their server role lets them manage users.
	Admin bool
	// Scopes limit a token; nil means no limit (a session, the local admin).
	Scopes []string
	// App, if set, is the only app a token may reach.
	App string
	// TokenID and SessionID say how they authenticated.
	TokenID   string
	SessionID string
	// CookieValue is the raw session cookie, from which CSRF tokens derive.
	CookieValue string
}

// Local reports whether this is the server's local administrator.
func (p Principal) Local() bool { return p.UserID == "" }

// Can reports whether their scopes allow a scope.
func (p Principal) Can(scope string) bool {
	if p.Scopes == nil {
		return true
	}
	switch scope {
	case ScopeRead:
		return slices.Contains(p.Scopes, ScopeRead) || slices.Contains(p.Scopes, ScopeDeploy) || slices.Contains(p.Scopes, ScopeAdmin)
	case ScopeDeploy:
		return slices.Contains(p.Scopes, ScopeDeploy) || slices.Contains(p.Scopes, ScopeAdmin)
	}
	return slices.Contains(p.Scopes, scope)
}

// CanApp reports whether a token's restriction allows an app.
func (p Principal) CanApp(app string) bool { return p.App == "" || p.App == app }

// Service authenticates requests and manages accounts' credentials.
type Service struct {
	DB *store.DB
	// LocalToken is the hash of the server's local admin token.
	LocalToken [32]byte
	// Trusted lists the proxies whose forwarded headers are believed.
	Trusted []netip.Prefix
	// Secure sets the session cookie's Secure attribute: the console is
	// reached over HTTPS, directly or through a proxy.
	Secure bool
	// ClaimFile holds the first-run claim code until the server is claimed.
	ClaimFile string
	Limits    *Limiter
	Log       *slog.Logger
	// Now, if set, replaces the clock (tests).
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logger() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// Security logs a security event, such as a failed sign-in.
func (s *Service) Security(event string, args ...any) {
	s.logger().Warn("security: "+event, args...)
}

// Hex returns the hex SHA-256 of a secret, which is how it is stored.
func Hex(secret string) string {
	h := Hash(secret)
	return hex.EncodeToString(h[:])
}

func randomCode() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// ClientIP returns the address a request came from: the peer, or, when the
// peer is a trusted proxy, the nearest forwarded address that is not.
func (s *Service) ClientIP(r *http.Request) string {
	return ClientIP(r, s.Trusted)
}

// ClientIP is the address a request came from, believing trusted proxies only.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if !isTrusted(peer, trusted) {
		return peer
	}
	forwarded := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(forwarded) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(forwarded[i])
		if ip != "" && !isTrusted(ip, trusted) {
			return ip
		}
	}
	return peer
}

func isTrusted(ip string, trusted []netip.Prefix) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return slices.ContainsFunc(trusted, func(p netip.Prefix) bool { return p.Contains(addr) })
}

func isLoopback(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	return err == nil && addr.Unmap().IsLoopback()
}

// Claimed reports whether the server has accounts.
func (s *Service) Claimed(ctx context.Context) (bool, error) {
	n, err := s.DB.CountUsers(ctx)
	return n > 0, err
}

// Token authenticates an API token, from the address ip.
func (s *Service) Token(ctx context.Context, token, ip string) (Principal, error) {
	token = strings.TrimSpace(token)
	if Matches(token, s.LocalToken) {
		claimed, err := s.Claimed(ctx)
		if err != nil {
			return Principal{}, err
		}
		if claimed && !isLoopback(ip) {
			s.Security("the admin token was used from another machine", "ip", ip)
			return Principal{}, ErrLocalOnly
		}
		return Principal{Name: LocalAdmin, Admin: true}, nil
	}
	t, err := s.DB.TokenByHash(ctx, Hex(token))
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err
	}
	if !t.ExpiresAt.IsZero() && s.now().After(t.ExpiresAt) {
		return Principal{}, ErrUnauthenticated
	}
	p := Principal{Name: LocalAdmin, Admin: true, Scopes: t.Scopes, App: t.App, TokenID: t.ID}
	if p.Scopes == nil {
		p.Scopes = []string{}
	}
	if t.UserID != "" {
		u, err := s.activeUser(ctx, t.UserID)
		if err != nil {
			return Principal{}, err
		}
		p.UserID, p.Name, p.Admin = u.ID, u.Email, u.Role == store.RoleAdmin
	}
	if s.now().Sub(t.LastUsedAt) > touchEvery {
		if err := s.DB.TouchToken(ctx, t.ID); err != nil {
			s.logger().Warn("record a token's use", "err", err)
		}
	}
	return p, nil
}

func (s *Service) activeUser(ctx context.Context, id string) (store.User, error) {
	u, err := s.DB.UserByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return u, ErrUnauthenticated
	}
	if err != nil {
		return u, err
	}
	if u.Disabled() {
		return u, ErrUnauthenticated
	}
	return u, nil
}

// Key authenticates a public key by its fingerprint.
func (s *Service) Key(ctx context.Context, fingerprint string) (Principal, store.SSHKey, error) {
	k, err := s.DB.SSHKeyByFingerprint(ctx, fingerprint)
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, k, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, k, err
	}
	if k.UserID == "" {
		return Principal{Name: LocalAdmin, Admin: true}, k, nil
	}
	u, err := s.activeUser(ctx, k.UserID)
	if err != nil {
		return Principal{}, k, err
	}
	return Principal{UserID: u.ID, Name: u.Email, Admin: u.Role == store.RoleAdmin}, k, nil
}

// CookieName is the session cookie's name: with HTTPS, the __Host- prefix makes
// browsers keep it to the console's own hostname, away from the apps on its
// sibling hostnames (spec/06-ingress.md, "Cookie rules").
func (s *Service) CookieName() string {
	if s.Secure {
		return "__Host-moorage_session"
	}
	return "moorage_session"
}

// SessionCookie is the cookie for a new session, or, with an empty value, the
// cookie that removes it.
func (s *Service) SessionCookie(value string) *http.Cookie {
	c := &http.Cookie{Name: s.CookieName(), Value: value, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode}
	if value == "" {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(sessionLifetime.Seconds())
	}
	return c
}

// Session authenticates a session cookie's value.
func (s *Service) Session(ctx context.Context, value string) (Principal, error) {
	sess, err := s.DB.SessionByHash(ctx, Hex(value))
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err
	}
	now := s.now()
	if now.After(sess.ExpiresAt) || now.Sub(sess.LastSeenAt) > sessionIdle {
		_ = s.DB.DeleteSessionByHash(ctx, sess.IDHash)
		return Principal{}, ErrUnauthenticated
	}
	u, err := s.activeUser(ctx, sess.UserID)
	if err != nil {
		return Principal{}, err
	}
	if now.Sub(sess.LastSeenAt) > touchEvery {
		if err := s.DB.TouchSession(ctx, sess.ID); err != nil {
			s.logger().Warn("record a session's use", "err", err)
		}
	}
	return Principal{UserID: u.ID, Name: u.Email, Admin: u.Role == store.RoleAdmin, SessionID: sess.ID, CookieValue: value}, nil
}

// Request authenticates a request by its bearer token or session cookie.
func (s *Service) Request(r *http.Request) (Principal, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		token, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			return Principal{}, ErrUnauthenticated
		}
		return s.Token(r.Context(), token, s.ClientIP(r))
	}
	c, err := r.Cookie(s.CookieName())
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	return s.Session(r.Context(), c.Value)
}

// CSRFToken is the token that forms and cookie-authenticated API requests must
// carry: it is bound to the session, and useless without it.
func CSRFToken(cookieValue string) string {
	m := hmac.New(sha256.New, []byte(cookieValue))
	m.Write([]byte("moorage csrf"))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// CheckCSRF reports whether token is the session's CSRF token.
func CheckCSRF(cookieValue, token string) bool {
	return subtle.ConstantTimeCompare([]byte(CSRFToken(cookieValue)), []byte(token)) == 1
}

// SameOrigin reports whether a browser says a request came from the page's own
// origin: apps on sibling hostnames are same-site, but not same-origin. A
// request with neither header did not come from a browser that sends them.
func SameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	_, host, ok := strings.Cut(origin, "://")
	return ok && strings.EqualFold(host, r.Host)
}

// NormalizeEmail lowercases and checks an email address.
func NormalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || !strings.Contains(email, "@") {
		return "", errors.New("that is not an email address")
	}
	return email, nil
}

// StartSession begins a session and returns the cookie's value.
func (s *Service) StartSession(ctx context.Context, u store.User, ip, userAgent string) (string, error) {
	value := randomCode()
	now := s.now()
	if len(userAgent) > 256 {
		userAgent = userAgent[:256]
	}
	err := s.DB.CreateSession(ctx, store.Session{
		ID: ids.New(ids.Session), IDHash: Hex(value), UserID: u.ID, CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(sessionLifetime), IP: ip, UserAgent: userAgent,
	})
	return value, err
}

// EndSession ends the session with this cookie value.
func (s *Service) EndSession(ctx context.Context, value string) error {
	return s.DB.DeleteSessionByHash(ctx, Hex(value))
}

func (s *Service) limit(keys ...string) error {
	if s.Limits == nil {
		return nil
	}
	for _, k := range keys {
		if wait := s.Limits.Wait(k); wait > 0 {
			return &RateLimitedError{Wait: wait}
		}
	}
	return nil
}

func (s *Service) failed(keys ...string) {
	if s.Limits != nil {
		for _, k := range keys {
			s.Limits.Fail(k)
		}
	}
}

// SignIn checks an email address and password, with limits per address and
// per account, and returns the user. Unknown addresses take as long as known
// ones, and answer the same.
func (s *Service) SignIn(ctx context.Context, email, password, ip string) (store.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	keys := []string{"signin-ip:" + ip, "signin-account:" + email}
	if err := s.limit(keys...); err != nil {
		s.Security("sign-in refused by the rate limit", "ip", ip, "email", email)
		return store.User{}, err
	}
	u, err := s.DB.UserByEmail(ctx, email)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.User{}, err
	}
	hash := u.PasswordHash
	if hash == "" {
		hash = decoyHash()
	}
	ok, err := VerifyPassword(ctx, hash, password)
	if err != nil {
		return store.User{}, err
	}
	if !ok || u.ID == "" || u.PasswordHash == "" || u.Disabled() {
		s.failed(keys...)
		s.Security("a sign-in failed", "ip", ip, "email", email)
		return store.User{}, ErrBadCredentials
	}
	if s.Limits != nil {
		s.Limits.Clear("signin-account:" + email)
	}
	return u, nil
}

// EnsureClaimCode returns the first-run claim code in file, creating it if it
// does not exist.
func EnsureClaimCode(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err == nil {
		if code := strings.TrimSpace(string(data)); code != "" {
			return code, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	code := randomCode()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", err
	}
	if err := writeSecret(file, code+"\n"); err != nil {
		return "", fmt.Errorf("write the claim code: %w", err)
	}
	return code, nil
}

// PrepareClaim makes sure an unclaimed server has a claim code, and removes
// the code of a claimed one. It returns the code, "" once claimed.
func (s *Service) PrepareClaim(ctx context.Context) (string, error) {
	claimed, err := s.Claimed(ctx)
	if err != nil {
		return "", err
	}
	if claimed {
		if err := os.Remove(s.ClaimFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "", nil
	}
	return EnsureClaimCode(s.ClaimFile)
}

// CheckClaimCode reports whether code claims this server, with a limit per
// address.
func (s *Service) CheckClaimCode(code, ip string) error {
	key := "claim-ip:" + ip
	if err := s.limit(key); err != nil {
		return err
	}
	data, err := os.ReadFile(s.ClaimFile)
	if err != nil || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(data))), []byte(code)) != 1 || code == "" {
		s.failed(key)
		s.Security("a wrong claim code", "ip", ip)
		return ErrBadClaimCode
	}
	return nil
}

// Claim makes the first account: the server's first admin. The code stops
// working once it succeeds.
func (s *Service) Claim(ctx context.Context, code, ip, email, name, password string) (store.User, error) {
	if err := s.CheckClaimCode(code, ip); err != nil {
		return store.User{}, err
	}
	u, err := s.NewUser(ctx, email, name, store.RoleAdmin, password)
	if err != nil {
		return store.User{}, err
	}
	if err := s.DB.ClaimServer(ctx, u); err != nil {
		return store.User{}, err
	}
	if err := os.Remove(s.ClaimFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger().Warn("remove the claim code", "err", err)
	}
	s.logger().Info("the server was claimed", "email", u.Email, "ip", ip)
	return u, nil
}

// NewUser validates a new account's details and returns it, not yet stored. A
// password may be empty: the user chooses one with an invitation.
func (s *Service) NewUser(ctx context.Context, email, name, role, password string) (store.User, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return store.User{}, &FieldError{"email", err.Error()}
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	u := store.User{ID: ids.New(ids.User), Email: email, Name: name, Role: role, CreatedAt: s.now().UTC()}
	if password != "" {
		if err := CheckPassword(password); err != nil {
			return store.User{}, &FieldError{"password", err.Error()}
		}
		if u.PasswordHash, err = HashPassword(ctx, password); err != nil {
			return store.User{}, err
		}
	}
	return u, nil
}

// FieldError says what is wrong with one field of a request.
type FieldError struct{ Field, Detail string }

func (e *FieldError) Error() string { return e.Detail }

// Invite makes a one-time link's code, for a user to choose a password. It
// replaces any earlier invitation of theirs.
func (s *Service) Invite(ctx context.Context, userID, by string) (string, error) {
	code := randomCode()
	return code, s.DB.CreateInvitation(ctx, Hex(code), userID, by, s.now().Add(InvitationLife))
}

// AcceptInvitation uses an invitation: it sets the user's password, ending
// their sessions, and returns them.
func (s *Service) AcceptInvitation(ctx context.Context, code, password string) (store.User, error) {
	if err := CheckPassword(password); err != nil {
		return store.User{}, &FieldError{"password", err.Error()}
	}
	hash, err := HashPassword(ctx, password)
	if err != nil {
		return store.User{}, err
	}
	return s.DB.AcceptInvitation(ctx, Hex(code), hash)
}

const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ" // no vowels, so no words

// newUserCode returns a code for people to compare: eight letters.
func newUserCode() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, userCodeAlphabet[int(c)%len(userCodeAlphabet)])
	}
	return string(out)
}

// FindDevice returns the sign-in waiting with this user code, with a limit on
// how many wrong codes one user may try.
func (s *Service) FindDevice(ctx context.Context, userCode, userID string) (store.DeviceCode, error) {
	key := "device-user:" + userID
	if err := s.limit(key); err != nil {
		return store.DeviceCode{}, err
	}
	d, err := s.DB.DeviceByUserCode(ctx, userCode)
	if errors.Is(err, store.ErrNotFound) {
		s.failed(key)
	}
	return d, err
}

// StartDevice begins a CLI's sign-in: the CLI keeps the secret code and shows
// its user the user code, to approve in a browser.
func (s *Service) StartDevice(ctx context.Context, client, ip string) (code, userCode string, err error) {
	key := "device-ip:" + ip
	if err := s.limit(key); err != nil {
		return "", "", err
	}
	s.failed(key) // every start counts: nothing makes it succeed sooner
	if len(client) > 100 {
		client = client[:100]
	}
	code, userCode = randomCode(), newUserCode()
	err = s.DB.CreateDeviceCode(ctx, store.DeviceCode{CodeHash: Hex(code), UserCode: userCode, Client: client, IP: ip, ExpiresAt: s.now().Add(DeviceCodeLife)})
	return code, userCode, err
}

// PollDevice answers a CLI's question about its sign-in: once approved, it
// returns the new token and its owner, once.
func (s *Service) PollDevice(ctx context.Context, code, tokenName string) (string, store.User, error) {
	hash := Hex(code)
	d, err := s.DB.PollDevice(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return "", store.User{}, ErrDeviceExpired
	}
	if err != nil {
		return "", store.User{}, err
	}
	switch {
	case s.now().After(d.ExpiresAt):
		_ = s.DB.DeleteDevice(ctx, hash)
		return "", store.User{}, ErrDeviceExpired
	case d.Denied:
		_ = s.DB.DeleteDevice(ctx, hash)
		return "", store.User{}, ErrDeviceDenied
	case d.ApprovedBy == "":
		if !d.LastPollAt.IsZero() && s.now().Sub(d.LastPollAt) < DevicePollInterval/2 {
			return "", store.User{}, ErrDeviceSlowDown
		}
		return "", store.User{}, ErrDevicePending
	}
	u, err := s.activeUser(ctx, d.ApprovedBy)
	if err != nil {
		return "", store.User{}, ErrDeviceDenied
	}
	token := NewToken()
	id := ids.New(ids.Token)
	err = s.DB.RedeemDevice(ctx, hash, store.APIToken{
		ID: id, UserID: u.ID, Name: fmt.Sprintf("%s (%s)", tokenName, id[len(id)-6:]), TokenHash: Hex(token),
		Scopes: []string{ScopeAdmin}, CreatedAt: s.now().UTC(),
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", store.User{}, ErrDeviceExpired
	}
	if err != nil {
		return "", store.User{}, err
	}
	return token, u, nil
}
