package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/store"
)

// GateCodeLife is how long a one-time code from the console to an app's hostname lasts.
const GateCodeLife = 60 * time.Second

type gateCode struct {
	userID, sessionID, target string
	expires                   time.Time
}

// gateCodes are one-time codes that say, to an app's hostname, who the console knows them as.
type gateCodes struct {
	mu    sync.Mutex
	codes map[string]gateCode // by the hash of the code
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// IssueGateCode makes a code that says who someone is, signed in by which session, to one
// place: a hostname, and any path on it. It says so once, and soon.
func (s *Service) IssueGateCode(userID, sessionID, target string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	s.gate.mu.Lock()
	defer s.gate.mu.Unlock()
	if s.gate.codes == nil {
		s.gate.codes = map[string]gateCode{}
	}
	now := time.Now()
	for k, c := range s.gate.codes {
		if now.After(c.expires) {
			delete(s.gate.codes, k)
		}
	}
	s.gate.codes[hashCode(code)] = gateCode{userID, sessionID, target, now.Add(GateCodeLife)}
	return code, nil
}

// RedeemGateCode says who a code was made for, if it was made for this place, and has not
// been used. Asking uses it up, whatever the answer.
func (s *Service) RedeemGateCode(code, target string) (userID, sessionID string, ok bool) {
	s.gate.mu.Lock()
	defer s.gate.mu.Unlock()
	key := hashCode(code)
	c, found := s.gate.codes[key]
	delete(s.gate.codes, key)
	if !found || c.target != target || time.Now().After(c.expires) {
		return "", "", false
	}
	return c.userID, c.sessionID, true
}

// SessionAlive says whether a console session has not ended: signing out, ending it, or changing
// a password ends the way through a gate that it opened.
func (s *Service) SessionAlive(ctx context.Context, userID, sessionID string) (bool, error) {
	list, err := s.DB.UserSessions(ctx, userID)
	if err != nil {
		return false, err
	}
	now := s.now()
	for _, sess := range list {
		if sess.ID == sessionID {
			return now.Before(sess.ExpiresAt) && now.Sub(sess.LastSeenAt) <= sessionIdle, nil
		}
	}
	return false, nil
}

// GateUser says whether a user may pass the sign-in gate of an app: any account, if anyUser,
// or else one with a role that lets them see it. It returns the account for the app to hear of.
func (s *Service) GateUser(ctx context.Context, userID, app string, anyUser bool) (store.User, bool, error) {
	u, err := s.DB.UserByID(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return u, false, nil
	}
	if err != nil {
		return u, false, err
	}
	if u.Disabled() {
		return u, false, nil
	}
	if anyUser {
		return u, true, nil
	}
	access, err := s.AppAccess(ctx, Principal{UserID: u.ID, Name: u.Email, Admin: u.Role == store.RoleAdmin}, app)
	if err != nil {
		return u, false, err
	}
	return u, access.See, nil
}
