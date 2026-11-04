// Package console serves the pages on the console's hostname. For now they are
// the few an account needs: the first-run claim, signing in and out, choosing
// a password and approving a CLI's sign-in. The web console grows from here.
package console

import (
	"bytes"
	_ "embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/brand"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/store"
)

//go:embed pages.html
var pagesHTML string

// compiled holds each page in the layout, as "body" is a different template in
// each.
var compiled = func() map[string]*template.Template {
	base := template.Must(template.New("pages").Parse(pagesHTML))
	out := map[string]*template.Template{}
	for _, page := range []string{"home", "setup", "login", "invite", "device-enter", "device-confirm", "message"} {
		t := template.Must(base.Clone())
		template.Must(t.New("body").Parse(`{{template "` + page + `" .}}`))
		out[page] = t
	}
	return out
}()

// Console serves the pages.
type Console struct {
	auth *auth.Service
	db   *store.DB
	log  *slog.Logger
}

// New returns the pages' handler.
func New(authn *auth.Service, db *store.DB, log *slog.Logger) http.Handler {
	c := &Console{auth: authn, db: db, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", c.home)
	mux.HandleFunc("GET /setup", c.setupPage)
	mux.HandleFunc("POST /setup", c.post(c.setup))
	mux.HandleFunc("GET /login", c.loginPage)
	mux.HandleFunc("POST /login", c.post(c.login))
	mux.HandleFunc("POST /logout", c.post(c.logout))
	mux.HandleFunc("GET /invite", c.invitePage)
	mux.HandleFunc("POST /invite", c.post(c.invite))
	mux.HandleFunc("GET /device", c.devicePage)
	mux.HandleFunc("POST /device", c.post(c.device))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c.message(w, http.StatusNotFound, data{Title: "Not found", Heading: "Not found", Message: "There is nothing at this address.", Link: "/", LinkText: "Back to the start"})
	})
	return mux
}

// data is what a page shows.
type data struct {
	Title, Heading, Message string
	Link, LinkText          string
	Error                   string
	// User is who is signed in.
	User string
	// CSRF is the session's token, for forms that change anything.
	CSRF      string
	Unclaimed bool
	// A form's values, kept when it is refused. Passwords are not kept.
	Email, Name, Next string
	Code              string
	Client, ClientIP  string

	Version string
	Icon    template.HTML
	Favicon template.URL
}

// render sends a page.
func (c *Console) render(w http.ResponseWriter, status int, page string, d data) {
	d.Version, d.Icon, d.Favicon = buildinfo.Version, brand.Icon(), brand.FaviconURL()
	var buf bytes.Buffer
	if err := compiled[page].ExecuteTemplate(&buf, "layout", d); err != nil {
		c.log.Error("render a page", "page", page, "err", err)
		http.Error(w, "The page could not be shown.", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	h.Set("X-Content-Type-Options", "nosniff")
	// The one-time codes in these pages' addresses must not leave in a Referer.
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (c *Console) message(w http.ResponseWriter, status int, d data) {
	c.render(w, status, "message", d)
}

func (c *Console) internal(w http.ResponseWriter, err error) {
	c.log.Error("a page failed", "err", err)
	c.message(w, http.StatusInternalServerError, data{Title: "Something went wrong", Heading: "Something went wrong", Message: "The server could not complete the request; its log has the details."})
}

// post guards a form's handler: the request must come from the console's own
// pages, and its form is read with a limit.
func (c *Console) post(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.SameOrigin(r) {
			c.auth.Security("a form was posted from another site", "ip", c.auth.ClientIP(r), "path", r.URL.Path)
			c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "That request was refused", Message: "It did not come from this site's own pages.", Link: "/", LinkText: "Back to the start"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			c.message(w, http.StatusBadRequest, data{Title: "Refused", Heading: "That request was refused", Message: "The form could not be read."})
			return
		}
		next(w, r)
	}
}

// signedIn returns who the request's session is, if any.
func (c *Console) signedIn(r *http.Request) (auth.Principal, bool) {
	cookie, err := r.Cookie(c.auth.CookieName())
	if err != nil {
		return auth.Principal{}, false
	}
	p, err := c.auth.Session(r.Context(), cookie.Value)
	return p, err == nil
}

func (c *Console) startSession(w http.ResponseWriter, r *http.Request, u store.User) bool {
	value, err := c.auth.StartSession(r.Context(), u, c.auth.ClientIP(r), r.UserAgent())
	if err != nil {
		c.internal(w, err)
		return false
	}
	http.SetCookie(w, c.auth.SessionCookie(value))
	return true
}

// failed shows why a form was refused, on its own page.
func (c *Console) failed(w http.ResponseWriter, page string, d data, err error) {
	var limited *auth.RateLimitedError
	var field *auth.FieldError
	switch {
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.Wait.Seconds())+1))
		d.Error = "Too many attempts. Try again in a moment."
		c.render(w, http.StatusTooManyRequests, page, d)
	case errors.As(err, &field):
		d.Error = sentence(field.Detail)
		c.render(w, http.StatusUnprocessableEntity, page, d)
	case errors.Is(err, auth.ErrBadCredentials):
		d.Error = "The email address or password is wrong."
		c.render(w, http.StatusUnauthorized, page, d)
	default:
		c.internal(w, err)
	}
}

// sentence capitalises an error's text and ends it with a full stop.
func sentence(s string) string {
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

func (c *Console) home(w http.ResponseWriter, r *http.Request) {
	d := data{Title: "Moorage"}
	if p, ok := c.signedIn(r); ok {
		d.User, d.CSRF = p.Name, auth.CSRFToken(p.CookieValue)
	} else {
		claimed, err := c.auth.Claimed(r.Context())
		if err != nil {
			c.internal(w, err)
			return
		}
		d.Unclaimed = !claimed
	}
	c.render(w, http.StatusOK, "home", d)
}

func badClaim() data {
	return data{Title: "Claim this server", Heading: "That setup link is not valid", Message: "Open the link that moor server status shows on the server."}
}

// claimRefused answers a claim whose code is wrong, or that is rate limited.
func (c *Console) claimRefused(w http.ResponseWriter, err error) {
	var limited *auth.RateLimitedError
	if errors.As(err, &limited) {
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.Wait.Seconds())+1))
		c.message(w, http.StatusTooManyRequests, data{Title: "Claim this server", Heading: "Too many attempts", Message: "Try again in a moment."})
		return
	}
	c.message(w, http.StatusForbidden, badClaim())
}

func (c *Console) setupPage(w http.ResponseWriter, r *http.Request) {
	claimed, err := c.auth.Claimed(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	if claimed {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	code := r.URL.Query().Get("code")
	if err := c.auth.CheckClaimCode(code, c.auth.ClientIP(r)); err != nil {
		c.claimRefused(w, err)
		return
	}
	c.render(w, http.StatusOK, "setup", data{Title: "Claim this server", Code: code})
}

func (c *Console) setup(w http.ResponseWriter, r *http.Request) {
	code, email, name, password := r.PostFormValue("code"), r.PostFormValue("email"), r.PostFormValue("name"), r.PostFormValue("password")
	d := data{Title: "Claim this server", Code: code, Email: email, Name: name}
	ip := c.auth.ClientIP(r)
	if err := c.auth.CheckClaimCode(code, ip); err != nil {
		c.claimRefused(w, err)
		return
	}
	if password != r.PostFormValue("confirm") {
		d.Error = "The two passwords differ."
		c.render(w, http.StatusUnprocessableEntity, "setup", d)
		return
	}
	u, err := c.auth.Claim(r.Context(), code, ip, email, name, password)
	switch {
	case errors.Is(err, auth.ErrBadClaimCode):
		c.claimRefused(w, err)
		return
	case errors.Is(err, store.ErrAlreadyClaimed):
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case err != nil:
		c.failed(w, "setup", d, err)
		return
	}
	if c.startSession(w, r, u) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// safeNext keeps a redirect after signing in on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) || strings.ContainsAny(next, "\r\n") {
		return "/"
	}
	return next
}

func (c *Console) loginPage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if _, ok := c.signedIn(r); ok {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	claimed, err := c.auth.Claimed(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	if !claimed { // there is no account to sign in with: the start page says why
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	c.render(w, http.StatusOK, "login", data{Title: "Sign in", Next: next})
}

func (c *Console) login(w http.ResponseWriter, r *http.Request) {
	email, next := r.PostFormValue("email"), safeNext(r.PostFormValue("next"))
	u, err := c.auth.SignIn(r.Context(), email, r.PostFormValue("password"), c.auth.ClientIP(r))
	if err != nil {
		c.failed(w, "login", data{Title: "Sign in", Email: email, Next: next}, err)
		return
	}
	if c.startSession(w, r, u) {
		http.Redirect(w, r, next, http.StatusSeeOther)
	}
}

func (c *Console) logout(w http.ResponseWriter, r *http.Request) {
	if p, ok := c.signedIn(r); ok {
		if !auth.CheckCSRF(p.CookieValue, r.PostFormValue("csrf")) {
			c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "That request was refused", Message: "It did not come from this site's own pages.", Link: "/", LinkText: "Back to the start"})
			return
		}
		if err := c.auth.EndSession(r.Context(), p.CookieValue); err != nil {
			c.internal(w, err)
			return
		}
	}
	http.SetCookie(w, c.auth.SessionCookie(""))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func badInvitation() data {
	return data{Title: "Choose a password", Heading: "That link is not valid", Message: "It has been used, or it has expired. Ask an administrator for a new one."}
}

func (c *Console) invitePage(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	u, err := c.db.InvitedUser(r.Context(), auth.Hex(code))
	if errors.Is(err, store.ErrNotFound) {
		c.message(w, http.StatusNotFound, badInvitation())
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	c.render(w, http.StatusOK, "invite", data{Title: "Choose a password", Code: code, Email: u.Email})
}

func (c *Console) invite(w http.ResponseWriter, r *http.Request) {
	code, password := r.PostFormValue("code"), r.PostFormValue("password")
	invited, err := c.db.InvitedUser(r.Context(), auth.Hex(code))
	if errors.Is(err, store.ErrNotFound) {
		c.message(w, http.StatusNotFound, badInvitation())
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	d := data{Title: "Choose a password", Code: code, Email: invited.Email}
	if password != r.PostFormValue("confirm") {
		d.Error = "The two passwords differ."
		c.render(w, http.StatusUnprocessableEntity, "invite", d)
		return
	}
	u, err := c.auth.AcceptInvitation(r.Context(), code, password)
	if errors.Is(err, store.ErrNotFound) {
		c.message(w, http.StatusNotFound, badInvitation())
		return
	}
	if err != nil {
		c.failed(w, "invite", d, err)
		return
	}
	if u.Disabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if c.startSession(w, r, u) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// normalizeUserCode tolerates a code typed in lower case, or without its dash.
func normalizeUserCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) == 8 && !strings.Contains(code, "-") {
		code = code[:4] + "-" + code[4:]
	}
	return code
}

func (c *Console) devicePage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.signedIn(r)
	if !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	code := normalizeUserCode(r.URL.Query().Get("code"))
	if code == "" {
		c.render(w, http.StatusOK, "device-enter", data{Title: "Approve a sign-in"})
		return
	}
	dev, err := c.auth.FindDevice(r.Context(), code, p.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.render(w, http.StatusNotFound, "device-enter", data{Title: "Approve a sign-in", Error: "That code is not valid, or it has expired. Check your terminal."})
			return
		}
		c.failed(w, "device-enter", data{Title: "Approve a sign-in"}, err)
		return
	}
	c.render(w, http.StatusOK, "device-confirm", data{
		Title: "Approve a sign-in", User: p.Name, CSRF: auth.CSRFToken(p.CookieValue), Code: dev.UserCode, Client: dev.Client, ClientIP: dev.IP,
	})
}

func (c *Console) device(w http.ResponseWriter, r *http.Request) {
	p, ok := c.signedIn(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !auth.CheckCSRF(p.CookieValue, r.PostFormValue("csrf")) {
		c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "That request was refused", Message: "It did not come from this site's own pages.", Link: "/device", LinkText: "Start again"})
		return
	}
	approve := r.PostFormValue("decision") == "approve"
	userID := ""
	if approve {
		userID = p.UserID
	}
	err := c.db.DecideDevice(r.Context(), normalizeUserCode(r.PostFormValue("code")), userID)
	if errors.Is(err, store.ErrNotFound) {
		c.render(w, http.StatusNotFound, "device-enter", data{Title: "Approve a sign-in", Error: "That code is not valid, or it has expired. Check your terminal."})
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	if approve {
		c.auth.Security("a CLI's sign-in was approved", "user", p.Name, "ip", c.auth.ClientIP(r))
		c.message(w, http.StatusOK, data{Title: "Approved", Heading: "Signed in", Message: "You can close this page and go back to your terminal."})
		return
	}
	c.message(w, http.StatusOK, data{Title: "Denied", Heading: "Denied", Message: "The sign-in was refused. You can close this page."})
}
