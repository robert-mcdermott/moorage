package console

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// accountView is the pages of an account.
type accountView struct {
	UserRecord store.User
	Keys       []store.SSHKey
	Tokens     []store.APIToken
	Sessions   []sessionRow
	// NewToken is the token just made, with its secret, shown once.
	NewToken       *store.APIToken
	NewTokenSecret string
	// Form keeps what was typed in a form that was refused.
	FormName, FormValue string
	Scopes              []string
	AppChoice           string
	Expires             string
	Envs                string
	// Security is the page of how they sign in.
	Security securityView
}

type sessionRow struct {
	store.Session
	Current bool
	Browser string
}

// shortAgent makes a browser's description something a person can read.
func shortAgent(ua string) string {
	if ua == "" {
		return "An unknown browser"
	}
	browser := "A browser"
	for _, known := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"}, {"curl/", "curl"},
	} {
		if strings.Contains(ua, known.token) {
			browser = known.name
			break
		}
	}
	for _, known := range []struct{ token, name string }{
		{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"}, {"Mac OS X", "macOS"}, {"Windows", "Windows"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, known.token) {
			return browser + " on " + known.name
		}
	}
	return browser
}

func (c *Console) accountData(r *http.Request, p auth.Principal, tab, title string) (data, error) {
	d, _, err := c.shellData(r, p, "account", title)
	if err != nil {
		return d, err
	}
	d.Tab = tab
	d.Heading = title
	return d, nil
}

// accountPage is one page of an account, by the tab it is.
func (c *Console) accountPage(tab, title, page string, load func(context.Context, auth.Principal, *accountView) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := c.requireUser(w, r)
		if !ok {
			return
		}
		c.showAccount(w, r, p, tab, title, page, load, http.StatusOK, accountView{}, nil)
	}
}

func (c *Console) showAccount(w http.ResponseWriter, r *http.Request, p auth.Principal, tab, title, page string, load func(context.Context, auth.Principal, *accountView) error, code int, view accountView, failed *failure) {
	d, err := c.accountData(r, p, tab, title)
	if err != nil {
		c.internal(w, err)
		return
	}
	if err := load(r.Context(), p, &view); err != nil {
		c.internal(w, err)
		return
	}
	d.Account = view
	d.Failure = failed
	d.Notice = accountNotices[r.URL.Query().Get("done")]
	c.render(w, code, page, d)
}

var accountNotices = map[string]string{
	"password":       "Your password is changed, and your other sessions have ended.",
	"key-added":      "The key is added. You can push with it now.",
	"key-removed":    "The key is removed.",
	"token-gone":     "The token is revoked: nothing can use it now.",
	"session-gone":   "That session has ended.",
	"others-gone":    "Your other sessions have ended.",
	"two-factor-off": "Two-factor sign-in is off.",
}

func (c *Console) loadProfile(ctx context.Context, p auth.Principal, view *accountView) error {
	var err error
	view.UserRecord, err = c.db.UserByID(ctx, p.UserID)
	return err
}

func (c *Console) loadKeys(ctx context.Context, p auth.Principal, view *accountView) error {
	var err error
	view.Keys, err = c.db.SSHKeys(ctx, p.UserID)
	return err
}

func (c *Console) loadTokens(ctx context.Context, p auth.Principal, view *accountView) error {
	var err error
	view.Tokens, err = c.db.Tokens(ctx, p.UserID)
	return err
}

func (c *Console) loadSessions(ctx context.Context, p auth.Principal, view *accountView) error {
	list, err := c.db.UserSessions(ctx, p.UserID)
	if err != nil {
		return err
	}
	for _, s := range list {
		view.Sessions = append(view.Sessions, sessionRow{Session: s, Current: s.ID == p.SessionID, Browser: shortAgent(s.UserAgent)})
	}
	return nil
}

// accountAction guards a form that changes one's own account.
func (c *Console) accountAction(next func(http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
	return c.post(func(w http.ResponseWriter, r *http.Request) {
		p, ok := c.signedIn(r)
		if !ok {
			http.Redirect(w, r, "/login?next=/account", http.StatusSeeOther)
			return
		}
		if !auth.CheckCSRF(p.CookieValue, r.PostFormValue("csrf")) {
			c.refused(w)
			return
		}
		c.audited(w, r, p, func(w http.ResponseWriter) { next(w, r, p) })
	})
}

func (c *Console) changePassword(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	refuse := func(code int, message string) {
		c.showAccount(w, r, p, "profile", "Your account", "account", c.loadProfile, code, accountView{}, &failure{Message: message})
	}
	next := r.PostFormValue("new")
	if next != r.PostFormValue("confirm") {
		refuse(http.StatusUnprocessableEntity, "The two new passwords differ.")
		return
	}
	err := c.auth.ChangePassword(r.Context(), p.UserID, r.PostFormValue("current"), next, p.SessionID, c.auth.ClientIP(r))
	var field *auth.FieldError
	var limited *auth.RateLimitedError
	switch {
	case errors.As(err, &limited):
		refuse(http.StatusTooManyRequests, "Too many attempts. Try again in a moment.")
	case errors.As(err, &field):
		refuse(http.StatusUnprocessableEntity, sentence(field.Detail))
	case err != nil:
		c.internal(w, err)
	default:
		http.Redirect(w, r, "/account?done=password", http.StatusSeeOther)
	}
}

func (c *Console) addKey(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	name, key := r.PostFormValue("name"), r.PostFormValue("key")
	_, err := c.auth.AddSSHKey(r.Context(), p.UserID, name, key)
	var invalid *auth.InvalidKeyError
	var exists *store.KeyExistsError
	view := accountView{FormName: name, FormValue: key}
	switch {
	case errors.As(err, &invalid):
		c.showAccount(w, r, p, "keys", "SSH keys", "account-keys", c.loadKeys, http.StatusUnprocessableEntity, view, &failure{Message: invalid.Error()})
	case errors.As(err, &exists):
		c.showAccount(w, r, p, "keys", "SSH keys", "account-keys", c.loadKeys, http.StatusConflict, view, &failure{Message: sentence(exists.Error())})
	case err != nil:
		c.internal(w, err)
	default:
		http.Redirect(w, r, "/account/keys?done=key-added", http.StatusSeeOther)
	}
}

func (c *Console) removeKey(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if _, err := c.db.RemoveSSHKey(r.Context(), p.UserID, r.PostFormValue("key")); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/account/keys?done=key-removed", http.StatusSeeOther)
}

// tokenLifetimes are what a token's expiry offers.
var tokenLifetimes = map[string]time.Duration{"30d": 30 * 24 * time.Hour, "90d": 90 * 24 * time.Hour, "1y": 365 * 24 * time.Hour}

func (c *Console) createToken(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	name := r.PostFormValue("name")
	scopes := r.PostForm["scope"]
	app := strings.TrimSpace(r.PostFormValue("app"))
	expiry := r.PostFormValue("expires")
	var expires time.Time
	if d, ok := tokenLifetimes[expiry]; ok {
		expires = time.Now().Add(d)
	}
	envs := r.PostFormValue("envs")
	view := accountView{FormName: name, Scopes: scopes, AppChoice: app, Expires: expiry, Envs: envs}
	t, secret, err := c.auth.CreateLimitedToken(r.Context(), p.UserID, name, scopes, app, envs, expires)
	var field *auth.FieldError
	switch {
	case errors.Is(err, store.ErrTokenNameTaken):
		c.showAccount(w, r, p, "tokens", "API tokens", "account-tokens", c.loadTokens, http.StatusConflict, view, &failure{Message: "You already have a token of that name."})
	case errors.As(err, &field):
		c.showAccount(w, r, p, "tokens", "API tokens", "account-tokens", c.loadTokens, http.StatusUnprocessableEntity, view, &failure{Message: sentence(field.Detail)})
	case err != nil:
		c.internal(w, err)
	default:
		// The one time it can be shown: this answer is not a redirect, and is not kept.
		c.showAccount(w, r, p, "tokens", "API tokens", "account-tokens", c.loadTokens, http.StatusCreated, accountView{NewToken: &t, NewTokenSecret: secret}, nil)
	}
}

func (c *Console) revokeToken(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if _, err := c.db.DeleteToken(r.Context(), p.UserID, r.PostFormValue("token")); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/account/tokens?done=token-gone", http.StatusSeeOther)
}

func (c *Console) endSession(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id := r.PostFormValue("session")
	if err := c.db.DeleteSession(r.Context(), p.UserID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.internal(w, err)
		return
	}
	if id == p.SessionID {
		http.SetCookie(w, c.auth.SessionCookie(""))
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/account/sessions?done=session-gone", http.StatusSeeOther)
}

func (c *Console) endOtherSessions(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if err := c.db.DeleteOtherSessions(r.Context(), p.UserID, p.SessionID); err != nil {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/account/sessions?done=others-gone", http.StatusSeeOther)
}
