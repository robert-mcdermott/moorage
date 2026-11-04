package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/store"
)

// A time-based one-time password (RFC 6238), as authenticator apps make: six digits, every 30 seconds.
const (
	totpPeriod = 30
	totpDigits = 6
	// totpSkew is how many steps a code may be off by, for clocks and for typing.
	totpSkew = 1
	// secondFactorLifetime is how long someone has to give a code, once their password was right.
	secondFactorLifetime = 5 * time.Minute
	recoveryCodes        = 10
)

// ErrSecondFactor says that a second factor is needed, or the one given is wrong.
var ErrSecondFactor = errors.New("that code is not right")

// ErrSecondFactorNeeded says that a password was right, and that it takes a code as well.
var ErrSecondFactorNeeded = errors.New("this account also takes a code from its authenticator app")

// Sealer seals what someone's second factor needs, with the server's master key.
type Sealer interface {
	SealTOTP(userID string, value []byte) []byte
	OpenTOTP(userID string, sealed []byte) ([]byte, error)
}

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// totpAt is the code for a step of time.
func totpAt(secret []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	code := strconv.FormatUint(uint64(value%1000000), 10)
	return strings.Repeat("0", totpDigits-len(code)) + code
}

// CodeFor is the code an authenticator app shows for a key at a time.
func CodeFor(key string, at time.Time) (string, error) {
	secret, err := totpEncoding.DecodeString(key)
	if err != nil {
		return "", err
	}
	return totpAt(secret, at.Unix()/totpPeriod), nil
}

// checkTOTP says which step of time a code belongs to, if it belongs to one near enough to now.
func checkTOTP(secret []byte, code string, now time.Time) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	current := now.Unix() / totpPeriod
	found, step := false, int64(0)
	// All of them, whichever matches, so that how long this takes says nothing.
	for s := current - totpSkew; s <= current+totpSkew; s++ {
		if subtle.ConstantTimeCompare([]byte(totpAt(secret, s)), []byte(code)) == 1 {
			found, step = true, s
		}
	}
	return step, found
}

// SecondFactorSetup is what someone needs to add this account to an authenticator app.
type SecondFactorSetup struct {
	// URI is what a QR code holds, and Key the same to type by hand.
	URI, Key string
}

// BeginSecondFactor makes a new secret for someone, which counts once they have confirmed it.
func (s *Service) BeginSecondFactor(ctx context.Context, u store.User, issuer string) (SecondFactorSetup, error) {
	if s.Sealer == nil {
		return SecondFactorSetup{}, errors.New("this server cannot seal a second factor")
	}
	secret := make([]byte, 20)
	if _, err := rand.Read(secret); err != nil {
		return SecondFactorSetup{}, err
	}
	if err := s.DB.SetUserTOTP(ctx, store.UserTOTP{UserID: u.ID, Sealed: s.Sealer.SealTOTP(u.ID, secret), CreatedAt: s.now()}); err != nil {
		return SecondFactorSetup{}, err
	}
	return secondFactorSetup(secret, u.Email, issuer), nil
}

// CurrentSecondFactorSetup is what BeginSecondFactor said, for a page shown again.
func (s *Service) CurrentSecondFactorSetup(ctx context.Context, u store.User, issuer string) (SecondFactorSetup, bool, error) {
	t, err := s.DB.GetUserTOTP(ctx, u.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && t.Confirmed()) {
		return SecondFactorSetup{}, false, nil
	}
	if err != nil {
		return SecondFactorSetup{}, false, err
	}
	secret, err := s.Sealer.OpenTOTP(u.ID, t.Sealed)
	if err != nil {
		return SecondFactorSetup{}, false, err
	}
	return secondFactorSetup(secret, u.Email, issuer), true, nil
}

func secondFactorSetup(secret []byte, account, issuer string) SecondFactorSetup {
	key := totpEncoding.EncodeToString(secret)
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	query := url.Values{"secret": {key}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {strconv.Itoa(totpDigits)}, "period": {strconv.Itoa(totpPeriod)}}
	return SecondFactorSetup{URI: "otpauth://totp/" + label + "?" + query.Encode(), Key: key}
}

// ConfirmSecondFactor turns it on, if a code shows that they can use it, and answers with the codes
// that get them in if they lose it, which are shown only now.
func (s *Service) ConfirmSecondFactor(ctx context.Context, userID, code, ip string) ([]string, error) {
	keys := []string{"totp-account:" + userID, "totp-ip:" + ip}
	if err := s.limit(keys...); err != nil {
		return nil, err
	}
	t, err := s.DB.GetUserTOTP(ctx, userID)
	if err != nil {
		return nil, err
	}
	if t.Confirmed() {
		return nil, errors.New("two-factor sign-in is already on")
	}
	secret, err := s.Sealer.OpenTOTP(userID, t.Sealed)
	if err != nil {
		return nil, err
	}
	step, ok := checkTOTP(secret, code, s.now())
	if !ok {
		s.failed(keys...)
		return nil, ErrSecondFactor
	}
	codes := make([]string, recoveryCodes)
	hashes := make([]string, recoveryCodes)
	for i := range codes {
		codes[i] = newRecoveryCode()
		hashes[i] = Hex(normalizeRecovery(codes[i]))
	}
	if err := s.DB.ConfirmUserTOTP(ctx, userID, step, s.now(), hashes); err != nil {
		return nil, err
	}
	s.Audit(ctx, store.AuditEntry{Actor: userID, IP: ip, Action: "2fa.on", Target: userID, Result: "ok"})
	return codes, nil
}

func newRecoveryCode() string {
	raw := make([]byte, 10)
	_, _ = rand.Read(raw)
	code := strings.ToLower(totpEncoding.EncodeToString(raw))
	return code[:4] + "-" + code[4:8] + "-" + code[8:12] + "-" + code[12:16]
}

func normalizeRecovery(code string) string {
	return strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(code))
}

// NeedsSecondFactor says whether someone has a second factor that counts.
func (s *Service) NeedsSecondFactor(ctx context.Context, userID string) (bool, error) {
	t, err := s.DB.GetUserTOTP(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return t.Confirmed(), nil
}

// CheckSecondFactor takes a code from an authenticator app, or a recovery code. Each works once.
func (s *Service) CheckSecondFactor(ctx context.Context, u store.User, code, ip string) error {
	keys := []string{"totp-account:" + u.ID, "totp-ip:" + ip}
	if err := s.limit(keys...); err != nil {
		s.Security("a second factor refused by the rate limit", "ip", ip, "email", u.Email)
		return err
	}
	refuse := func() error {
		s.failed(keys...)
		s.Security("a second factor failed", "ip", ip, "email", u.Email)
		s.Audit(ctx, store.AuditEntry{Actor: u.Email, IP: ip, Action: "signin.second_factor", Result: "failed"})
		return ErrSecondFactor
	}
	t, err := s.DB.GetUserTOTP(ctx, u.ID)
	if err != nil {
		return err
	}
	secret, err := s.Sealer.OpenTOTP(u.ID, t.Sealed)
	if err != nil {
		return err
	}
	if step, ok := checkTOTP(secret, code, s.now()); ok {
		fresh, err := s.DB.UseTOTPStep(ctx, u.ID, step)
		if err != nil {
			return err
		}
		if !fresh {
			return refuse()
		}
	} else {
		used, err := s.DB.UseRecoveryCode(ctx, u.ID, Hex(normalizeRecovery(code)), s.now())
		if err != nil {
			return err
		}
		if !used {
			return refuse()
		}
		s.Security("a recovery code was used", "ip", ip, "email", u.Email)
		s.Audit(ctx, store.AuditEntry{Actor: u.Email, IP: ip, Action: "signin.recovery_code", Result: "ok"})
	}
	if s.Limits != nil {
		s.Limits.Clear("totp-account:" + u.ID)
	}
	return nil
}

// TurnOffSecondFactor removes someone's second factor, by them or an administrator.
func (s *Service) TurnOffSecondFactor(ctx context.Context, userID, by, ip string) error {
	if err := s.DB.DeleteUserTOTP(ctx, userID); err != nil {
		return err
	}
	s.Audit(ctx, store.AuditEntry{Actor: by, IP: ip, Action: "2fa.off", Target: userID, Result: "ok"})
	return nil
}

// PendingSecondFactor is what a browser holds between the password and the code: it says who, until
// when, and only while their password stays the same.
func (s *Service) PendingSecondFactor(u store.User) string {
	expires := strconv.FormatInt(s.now().Add(secondFactorLifetime).Unix(), 10)
	return u.ID + "." + expires + "." + s.pendingMAC(u, expires)
}

func (s *Service) pendingMAC(u store.User, expires string) string {
	mac := hmac.New(sha256.New, s.SecondFactorKey)
	fmt.Fprintf(mac, "%s\x00%s\x00%s", u.ID, expires, u.PasswordHash)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// PendingUser reads what PendingSecondFactor made.
func (s *Service) PendingUser(ctx context.Context, pending string) (store.User, error) {
	id, rest, ok := strings.Cut(pending, ".")
	expires, mac, ok2 := strings.Cut(rest, ".")
	if !ok || !ok2 {
		return store.User{}, ErrSecondFactor
	}
	seconds, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || s.now().Unix() > seconds || len(s.SecondFactorKey) == 0 {
		return store.User{}, ErrSecondFactor
	}
	u, err := s.DB.UserByID(ctx, id)
	if err != nil || u.Disabled() {
		return store.User{}, ErrSecondFactor
	}
	if !hmac.Equal([]byte(mac), []byte(s.pendingMAC(u, expires))) {
		return store.User{}, ErrSecondFactor
	}
	return u, nil
}
