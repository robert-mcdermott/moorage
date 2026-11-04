package console

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/ingress"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// gateLabels say what a gate is, in few words.
var gateLabels = map[string]string{
	ingress.GatePublic:   "Anyone",
	ingress.GatePassword: "A password",
	ingress.GateLogin:    "Those signed in",
	ingress.GateShare:    "Only share links",
}

// accessView is an environment's Access page.
type accessView struct {
	Mode, Label, Source string
	// Setting is what has been set; empty leaves it to the manifest and the server.
	Setting         string
	PasswordSet     bool
	PasswordMissing bool
	AllowText       string
	AnyUser         bool
	Links           []shareRow
	// NewLink is a link just made, which is shown only now.
	NewLink string
	// AskedInVain says that its manifest asks for no gate, which only a maintainer can allow.
	AskedInVain bool
	// Domains are hostnames of the user's own, and NoCertificate says the server's CA cannot
	// vouch for them, so that HTTPS needs a certificate from elsewhere.
	Domains       []domainRow
	NoCertificate bool
}

type shareRow struct {
	ID, By, State string
	Uses          string
	Created       time.Time
	Expires       time.Time
	Used          time.Time
	Active        bool
}

func (c *Console) accessView(r *http.Request, app, env string, canSeeLinks bool) (*accessView, error) {
	if c.deployer == nil {
		return &accessView{Mode: ingress.GatePublic, Label: gateLabels[ingress.GatePublic]}, nil
	}
	state, err := c.deployer.Access(r.Context(), app, env)
	if err != nil {
		return nil, err
	}
	view := &accessView{
		Mode: state.Mode, Label: gateLabels[state.Mode], Setting: state.Set.Mode, PasswordSet: state.Set.PasswordHash != "", PasswordMissing: state.PasswordMissing(),
		AllowText: strings.Join(state.Set.Allow, "\n"), AnyUser: state.Set.AnyUser, AskedInVain: state.AskedInVain(), NoCertificate: c.cfg.TLS.Mode == "internal",
		Source: map[string]string{"setting": "Set here", "manifest": "From its moorage.toml", "server": "The server's default for this kind of environment"}[state.Source],
	}
	if !canSeeLinks {
		return view, nil
	}
	links, err := c.db.ShareLinks(r.Context(), app, env)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, l := range links {
		row := shareRow{ID: l.ID, By: l.CreatedBy, Created: l.CreatedAt, Expires: l.ExpiresAt, Used: l.LastUsedAt, Uses: strconv.Itoa(l.Uses), State: "In use", Active: l.Active(now)}
		if l.MaxUses > 0 {
			row.Uses += " of " + strconv.Itoa(l.MaxUses)
			if l.Uses >= l.MaxUses {
				row.State, row.Active = "Used up", false
			}
		}
		switch {
		case !l.RevokedAt.IsZero():
			row.State = "Revoked"
		case !now.Before(l.ExpiresAt):
			row.State = "Expired"
		}
		view.Links = append(view.Links, row)
	}
	return view, nil
}

func (c *Console) accessPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	c.showAccess(w, r, p, app, http.StatusOK, nil, "")
}

func (c *Console) showAccess(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, code int, failed *failure, newLink string) {
	d, err := c.appData(r, p, app, "apps", "Domains and access of "+app)
	if err != nil {
		c.internal(w, err)
		return
	}
	d.Crumbs = []crumb{{"Apps", "/"}, {app, appAddress(app, envOf(r), "", nil)}}
	d.Tab = "access"
	d.App = appView{Name: app, URL: c.cfg.EnvURL(app, envOf(r))}
	if d.Gate, err = c.accessView(r, app, envOf(r), d.Access.Deploy); err != nil {
		c.internal(w, err)
		return
	}
	d.Gate.NewLink = newLink
	if d.Gate.Domains, err = c.domainRows(r, app, envOf(r)); err != nil {
		c.internal(w, err)
		return
	}
	d.Notice = map[string]string{"changed": "That takes effect now.", "revoked": "The link is revoked. Whoever it let in loses their way within a minute."}[r.URL.Query().Get("done")]
	d.Failure = failed
	c.render(w, code, "app-access", d)
}

func (c *Console) accessFailed(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, err error) {
	var invalid *deploy.InvalidAccessError
	switch {
	case errors.As(err, &invalid):
		c.showAccess(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: sentence(invalid.Error())}, "")
	case errors.Is(err, store.ErrNotFound):
		c.showAccess(w, r, p, app, http.StatusConflict, &failure{Message: "That is no longer there."}, "")
	default:
		c.internal(w, err)
	}
}

func (c *Console) setAccess(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	allow := strings.FieldsFunc(r.PostFormValue("allow"), func(r rune) bool { return r == '\n' || r == '\r' || r == ',' || r == ' ' })
	change := deploy.AccessChange{Mode: r.PostFormValue("mode"), Password: r.PostFormValue("password"), AnyUser: r.PostFormValue("any_user") != "", Allow: &allow}
	if _, err := c.deployer.SetAccess(lasting(r), app, envOf(r), p.Name, change); err != nil {
		c.accessFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, appAddress(app, envOf(r), "/access", url.Values{"done": {"changed"}}), http.StatusSeeOther)
}

func (c *Console) createShareLink(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	var life time.Duration
	if said := strings.TrimSpace(r.PostFormValue("expires")); said != "" {
		var d manifest.Duration
		if err := d.UnmarshalText([]byte(said)); err != nil {
			c.showAccess(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: sentence(err.Error())}, "")
			return
		}
		life = d.Std()
	}
	maxUses := 0
	if said := strings.TrimSpace(r.PostFormValue("max_uses")); said != "" {
		n, err := strconv.Atoi(said)
		if err != nil {
			c.showAccess(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: "How many uses? That is not a number."}, "")
			return
		}
		maxUses = n
	}
	_, address, err := c.deployer.CreateShareLink(lasting(r), app, envOf(r), p.Name, life, maxUses)
	if err != nil {
		c.accessFailed(w, r, p, app, err)
		return
	}
	// Shown here and never again, so not by way of an address that a browser would remember.
	c.showAccess(w, r, p, app, http.StatusOK, nil, address)
}

func (c *Console) revokeShareLink(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	if err := c.deployer.RevokeShareLink(lasting(r), app, envOf(r), r.PathValue("id"), p.Name); err != nil {
		c.accessFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, appAddress(app, envOf(r), "/access", url.Values{"done": {"revoked"}}), http.StatusSeeOther)
}

// gateSignIn is where the sign-in gate of an app sends someone. Signed in here, and let in
// by the app's gate, they go back with a one-time code, for that place alone, that says who they
// are. Hostnames are looked up, not believed: nothing goes anywhere but to what this server serves.
func (c *Console) gateSignIn(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	host := q.Get("host")
	var rt ingress.RouteInfo
	found := false
	if c.gateRoute != nil {
		rt, found = c.gateRoute(host, pathOf(q.Get("return")))
	}
	if !found {
		c.message(w, http.StatusNotFound, data{Title: "Not found", Heading: "There is no such app", Message: "Its address may have changed.", Link: "/", LinkText: "Back to the apps"})
		return
	}
	if p.UserID != "" {
		_, allowed, err := c.auth.GateUser(r.Context(), p.UserID, rt.App, rt.AnyUser)
		if err != nil {
			c.internal(w, err)
			return
		}
		if allowed {
			code, err := c.auth.IssueGateCode(p.UserID, p.SessionID, rt.Target)
			if err != nil {
				c.internal(w, err)
				return
			}
			back := url.Values{"code": {code}, "state": {q.Get("state")}, "return": {q.Get("return")}}
			w.Header().Set("Referrer-Policy", "no-referrer")
			http.Redirect(w, r, rt.Base+"/.moorage/auth/callback?"+back.Encode(), http.StatusSeeOther)
			return
		}
	}
	c.message(w, http.StatusForbidden, data{Title: "Access denied", Heading: "You have no way into " + rt.App, Message: "Signing in lets you through its gate if you have a role in its project. A maintainer can give you one, or anyone working on it can make you a share link.", Link: "/", LinkText: "Back to the apps"})
}

// pathOf is the path of an address, without what follows it.
func pathOf(address string) string {
	path, _, _ := strings.Cut(address, "?")
	return path
}

// domainRow is a domain on an environment's page.
type domainRow struct {
	Address, Problem, Record string
	Verified, StripPrefix    bool
	Prefix                   string
}

func (c *Console) domainRows(r *http.Request, app, env string) ([]domainRow, error) {
	if c.deployer == nil {
		return nil, nil
	}
	list, err := c.db.Domains(r.Context(), app, env)
	if err != nil {
		return nil, err
	}
	rows := make([]domainRow, 0, len(list))
	for _, d := range list {
		rows = append(rows, domainRow{Address: d.Address(), Problem: d.CheckError, Record: c.deployer.DomainRecord(app, env, d.Hostname), Verified: d.Verified(), StripPrefix: d.StripPrefix, Prefix: d.Prefix})
	}
	return rows, nil
}

func (c *Console) domainFailed(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, err error) {
	var invalid *deploy.InvalidDomainError
	if errors.As(err, &invalid) {
		c.showAccess(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: sentence(invalid.Error())}, "")
		return
	}
	c.accessFailed(w, r, p, app, err)
}

func (c *Console) addDomain(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	if _, err := c.deployer.AddDomain(lasting(r), app, envOf(r), p.Name, r.PostFormValue("address"), r.PostFormValue("strip") != ""); err != nil {
		c.domainFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, appAddress(app, envOf(r), "/access", nil), http.StatusSeeOther)
}

func (c *Console) verifyDomain(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	if _, err := c.deployer.VerifyDomain(lasting(r), app, envOf(r), r.PostFormValue("address")); err != nil {
		c.domainFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, appAddress(app, envOf(r), "/access", nil), http.StatusSeeOther)
}

func (c *Console) removeDomain(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	if err := c.deployer.RemoveDomain(lasting(r), app, envOf(r), p.Name, r.PostFormValue("address")); err != nil {
		c.domainFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, appAddress(app, envOf(r), "/access", nil), http.StatusSeeOther)
}
