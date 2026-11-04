package console

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/serverconfig"
	"github.com/robert-mcdermott/moorage/internal/sso"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// signInView is the page of how people sign in.
type signInView struct {
	On                                            bool
	Redirect                                      string
	Issuer, ClientID, Label, Scopes, EmailDomains string
	GroupClaim, GroupRules                        string
	HasSecret, Provision, TrustEmails             bool
	PasswordsOff                                  bool
}

func (c *Console) ssoCookieName() string {
	if c.auth.Secure {
		// So that an app on a name below this server's cannot set it for the whole domain, and have someone sign
		// in as whoever that app chooses.
		return "__Host-" + sso.CookieName
	}
	return sso.CookieName
}

func (c *Console) ssoCookie(value string, maxAge int) *http.Cookie {
	// Lax, since a provider sends a browser back from elsewhere with a link, and that has to bring it.
	return &http.Cookie{Name: c.ssoCookieName(), Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: c.auth.Secure}
}

func (c *Console) ssoFailed(w http.ResponseWriter, err error) {
	var refused *sso.RefusedError
	if errors.As(err, &refused) {
		c.message(w, http.StatusForbidden, data{Title: "Not signed in", Heading: "You are not signed in", Message: refused.Msg, Link: "/login", LinkText: "Back to signing in"})
		return
	}
	c.log.Warn("sign in through a provider", "err", err)
	c.message(w, http.StatusBadGateway, data{Title: "Not signed in", Heading: "You are not signed in", Message: "Your provider could not be reached. Try again in a moment.", Link: "/login", LinkText: "Back to signing in"})
}

func (c *Console) ssoBegin(w http.ResponseWriter, r *http.Request) {
	address, held, err := c.sso.Begin(r.Context(), safeNext(r.URL.Query().Get("next")))
	if err != nil {
		c.ssoFailed(w, err)
		return
	}
	http.SetCookie(w, c.ssoCookie(held, 600))
	http.Redirect(w, r, address, http.StatusSeeOther)
}

func (c *Console) ssoCallback(w http.ResponseWriter, r *http.Request) {
	held := ""
	if cookie, err := r.Cookie(c.ssoCookieName()); err == nil {
		held = cookie.Value
	}
	// It is good for one try, whichever way that goes.
	http.SetCookie(w, c.ssoCookie("", -1))
	u, next, err := c.sso.Finish(r.Context(), r.URL.Query(), held, c.auth.ClientIP(r))
	if err != nil {
		c.ssoFailed(w, err)
		return
	}
	if c.startSession(w, r, u) {
		http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
	}
}

func (c *Console) showSignIn(w http.ResponseWriter, r *http.Request, code int, form *sso.Form, failed *failure) {
	_, d, ok := c.adminData(w, r, "sign-in", "Sign-in")
	if !ok {
		return
	}
	cfg, err := c.db.SSOConfig(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	view := signInView{On: cfg.Configured(), Redirect: c.sso.RedirectURL(), Issuer: cfg.Issuer, ClientID: cfg.ClientID, Label: cfg.Label, Scopes: cfg.Scopes, EmailDomains: cfg.EmailDomains,
		GroupClaim: cfg.GroupClaim, GroupRules: cfg.GroupRules, HasSecret: len(cfg.SealedSecret) > 0, Provision: cfg.Provision, TrustEmails: cfg.TrustEmails, PasswordsOff: cfg.PasswordsOff}
	if form != nil {
		view.Issuer, view.ClientID, view.Label, view.Scopes, view.EmailDomains = form.Issuer, form.ClientID, form.Label, form.Scopes, form.EmailDomains
		view.GroupClaim, view.GroupRules, view.Provision, view.TrustEmails, view.PasswordsOff = form.GroupClaim, form.GroupRules, form.Provision, form.TrustEmails, form.PasswordsOff
	}
	d.SignIn, d.Failure = view, failed
	c.render(w, code, "settings-sign-in", d)
}

func (c *Console) signInSettings(w http.ResponseWriter, r *http.Request) {
	c.showSignIn(w, r, http.StatusOK, nil, nil)
}

func (c *Console) configureSignIn(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	form := sso.Form{Issuer: r.PostFormValue("issuer"), ClientID: r.PostFormValue("client_id"), ClientSecret: r.PostFormValue("client_secret"), Label: r.PostFormValue("label"), Scopes: r.PostFormValue("scopes"),
		EmailDomains: r.PostFormValue("email_domains"), Provision: r.PostFormValue("provision") != "", TrustEmails: r.PostFormValue("trust_emails") != "", GroupClaim: r.PostFormValue("group_claim"),
		GroupRules: strings.ReplaceAll(r.PostFormValue("group_rules"), "\r\n", "\n"), PasswordsOff: r.PostFormValue("passwords_off") != ""}
	err := c.sso.Configure(r.Context(), form, p.Name)
	var invalid *sso.InvalidError
	switch {
	case errors.As(err, &invalid):
		c.showSignIn(w, r, http.StatusUnprocessableEntity, &form, &failure{Message: invalid.Msg})
	case err != nil:
		c.internal(w, err)
	default:
		http.Redirect(w, r, "/settings/sign-in?done=sign-in-set", http.StatusSeeOther)
	}
}

func (c *Console) turnOffSignIn(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if err := c.sso.TurnOff(r.Context()); err != nil {
		c.internal(w, err)
		return
	}
	c.log.Info("turned off single sign-on", "by", p.Name)
	http.Redirect(w, r, "/settings/sign-in?done=sign-in-off", http.StatusSeeOther)
}

// trustedSignIn signs in whoever a proxy that signs people in says a request is from, before anything
// asks. It holds them to a session of their own like anyone else, for what depends on one.
func (c *Console) trustedSignIn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email := c.auth.TrustedIdentity(r)
		// Nothing a page needs to sign someone in for, and a request for one of these with no cookie, such as a
		// check of the proxy's, should not leave a session behind each time.
		if email == "" || strings.HasPrefix(r.URL.Path, "/assets/") {
			next.ServeHTTP(w, r)
			return
		}
		// A server nobody has claimed is waiting for its setup link. Making an account here would claim it, for
		// whoever came first, with no administrator.
		if claimed, err := c.auth.Claimed(r.Context()); err != nil || !claimed {
			next.ServeHTTP(w, r)
			return
		}
		if p, ok := c.signedIn(r); ok && strings.EqualFold(p.Name, email) {
			next.ServeHTTP(w, r)
			return
		}
		ip := c.auth.ClientIP(r)
		name := ""
		if c.auth.TrustedNameHeader != "" {
			name = r.Header.Get(c.auth.TrustedNameHeader)
		}
		u, err := c.auth.TrustedUser(r.Context(), email, name, ip)
		if errors.Is(err, auth.ErrNoAccount) {
			c.auth.Security("a proxy vouched for someone with no account", "ip", ip, "email", email)
			c.message(w, http.StatusForbidden, data{Title: "Not signed in", Heading: "You are not signed in", Message: sentence(err.Error())})
			return
		}
		if err != nil {
			c.internal(w, err)
			return
		}
		value, err := c.auth.StartSession(r.Context(), u, ip, r.UserAgent())
		if err != nil {
			c.internal(w, err)
			return
		}
		cookie := c.auth.SessionCookie(value)
		http.SetCookie(w, cookie)
		// What follows sees the new session, and none it had before, of whoever that was.
		kept := r.Cookies()
		r.Header.Del("Cookie")
		for _, old := range kept {
			if old.Name != cookie.Name {
				r.AddCookie(old)
			}
		}
		r.AddCookie(&http.Cookie{Name: cookie.Name, Value: value})
		next.ServeHTTP(w, r)
	})
}

// downloadConfiguration hands over the server's settings as a file. Only an admin sees it, and it has no
// secrets in it.
func (c *Console) downloadConfiguration(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	if !p.Admin || !p.Can(auth.ScopeAdmin) {
		c.message(w, http.StatusForbidden, data{Title: "Not allowed", Heading: "Not allowed", Message: "Only an administrator can read the server's configuration."})
		return
	}
	settings := &serverconfig.Service{DB: c.db, Auth: c.auth, Notifier: c.notifier, Backups: c.backups, SSO: c.sso}
	f, err := settings.Export(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	body, err := serverconfig.Encode(f, " from "+c.cfg.ConsoleHost+", on "+time.Now().UTC().Format("2 January 2006")+", by "+buildinfo.Version)
	if err != nil {
		c.internal(w, err)
		return
	}
	c.auth.Audit(r.Context(), store.AuditEntry{Actor: p.Name, IP: c.auth.ClientIP(r), Action: "server.config.export", Result: "ok"})
	w.Header().Set("Content-Type", "application/toml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="moorage-config.toml"`)
	_, _ = w.Write(body)
}
