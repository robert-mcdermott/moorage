package console

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/doctor"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Keyring seals the passwords of container registries.
type Keyring interface {
	SealRegistry(host, password string) []byte
}

// settingsView is the administrator's pages.
type settingsView struct {
	// The server's settings, as groups of what server.toml says.
	Groups     []settingGroup
	ConfigPath string
	// The users.
	Users []userRow
	// Invitation is a link just made, shown once.
	Invitation     string
	InvitationUser string
	// The registries.
	Registries []store.Registry
	// What the form said, when it was refused.
	FormEmail, FormName, FormRole, FormHost, FormUser string
	// Who did what.
	Audit                             []store.AuditEntry
	AuditExport                       string
	FormActor, FormAction, FormTarget string
	// The system.
	Checks            []doctor.Check
	Fails, Warns, Oks int
	Facts             []setting
}

type settingGroup struct {
	Title    string
	Settings []setting
}

type setting struct{ Name, Value string }

type userRow struct {
	store.User
	Self bool
}

// adminData is what the administrator's pages begin with: they are for admins alone.
func (c *Console) adminData(w http.ResponseWriter, r *http.Request, tab, title string) (auth.Principal, data, bool) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return p, data{}, false
	}
	if !p.Admin {
		c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "This is for administrators", Message: "Ask one of them, if it needs changing.", Link: "/", LinkText: "Back to the start"})
		return p, data{}, false
	}
	d, _, err := c.shellData(r, p, "settings", title)
	if err != nil {
		c.internal(w, err)
		return p, data{}, false
	}
	d.Tab = tab
	d.Notice = settingsNotices[r.URL.Query().Get("done")]
	return p, d, true
}

var settingsNotices = map[string]string{
	"role":             "The role is changed.",
	"disabled":         "The account is disabled, and its sessions have ended.",
	"enabled":          "The account is enabled.",
	"registry":         "The credentials are kept, sealed. Deployments use them to pull from this registry.",
	"registry-removed": "The credentials are removed.",
	"tested":           "The test went. Look for it where the channel goes.",
	"channel-removed":  "The channel is removed, and the rules that used it.",
	"rule":             "The rule is added.",
	"rule-removed":     "The rule is removed.",
}

// adminAction guards a form for administrators.
func (c *Console) adminAction(next func(http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
	return c.post(func(w http.ResponseWriter, r *http.Request) {
		p, ok := c.signedIn(r)
		if !ok {
			http.Redirect(w, r, "/login?next=/settings", http.StatusSeeOther)
			return
		}
		if !auth.CheckCSRF(p.CookieValue, r.PostFormValue("csrf")) {
			c.refused(w)
			return
		}
		if !p.Admin || !p.Can(auth.ScopeAdmin) {
			c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "This is for administrators", Message: "Ask one of them, if it needs changing.", Link: "/", LinkText: "Back to the start"})
			return
		}
		c.audited(w, r, p, func(w http.ResponseWriter) { next(w, r, p) })
	})
}

func orNone(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func (c *Console) settingsPage(w http.ResponseWriter, r *http.Request) {
	_, d, ok := c.adminData(w, r, "server", "Settings")
	if !ok {
		return
	}
	cfg := c.cfg
	tls := cfg.TLS.Mode
	switch cfg.TLS.Mode {
	case "internal":
		tls = "Certificates from Moorage's own CA"
		if cfg.TLS.CACert != "" {
			tls = "Certificates from an intermediate CA: " + cfg.TLS.CACert
		}
	case "files":
		tls = fmt.Sprintf("Certificates from files (%d)", len(cfg.TLS.Certificates))
	case "off":
		tls = "Off: plain HTTP, or a proxy in front"
	}
	mirrors := func(url string) string {
		if url == "" {
			return "The official location"
		}
		return url
	}
	d.Settings = settingsView{
		ConfigPath: cfg.Path,
		Groups: []settingGroup{
			{"Addresses", []setting{
				{"Apps", c.appURL("<app>")},
				{"The console", cfg.PublicURL(cfg.ConsoleHost)},
				{"Routing", cfg.Routing},
			}},
			{"Listening", []setting{
				{"HTTP", orNone(cfg.Listen.HTTP)}, {"HTTPS", orNone(cfg.Listen.HTTPS)},
				{"SSH (git)", orNone(cfg.Listen.SSH)}, {"DNS", orNone(cfg.Listen.DNS)},
			}},
			{"TLS and proxies", []setting{
				{"TLS", tls},
				{"Browsers see", cfg.Proxy.ExternalScheme},
				{"Trusted proxies", orNone(strings.Join(cfg.Proxy.Trusted, ", "))},
			}},
			{"Keeping apps apart", []setting{
				{"Asked for", orDefault(cfg.Isolation.Level, "auto")},
			}},
			{"Notifications", []setting{
				{"May also reach", orNone(strings.Join(cfg.Notifications.Allow, ", "))},
			}},
			{"Who may reach what, unless the manifest or a maintainer says otherwise", []setting{
				{"Production", gateLabels[cfg.Access.Production]},
				{"Named environments", gateLabels[cfg.Access.Named]},
				{"Previews", gateLabels[cfg.Access.Preview]},
			}},
			{"Running apps", []setting{
				{"Container engine", cfg.Containers.Engine},
				{"Data directory", cfg.DataDir},
			}},
			{"Toolchains", []setting{
				{"Go", mirrors(cfg.Toolchains.GoURL)}, {"Node.js", mirrors(cfg.Toolchains.NodeURL)},
				{"uv", mirrors(cfg.Toolchains.UVURL)}, {"Python", mirrors(cfg.Toolchains.PythonURL)},
			}},
			{"The server's log", []setting{{"Level", cfg.Log.Level}, {"Format", cfg.Log.Format}}},
		},
	}
	c.render(w, http.StatusOK, "settings", d)
}

func (c *Console) showUsers(w http.ResponseWriter, r *http.Request, code int, view settingsView, failed *failure) {
	p, d, ok := c.adminData(w, r, "users", "Users")
	if !ok {
		return
	}
	list, err := c.db.Users(r.Context(), "", 1000)
	if err != nil {
		c.internal(w, err)
		return
	}
	for _, u := range list {
		view.Users = append(view.Users, userRow{User: u, Self: u.ID == p.UserID})
	}
	d.Settings = view
	d.Failure = failed
	c.render(w, code, "settings-users", d)
}

func (c *Console) usersPage(w http.ResponseWriter, r *http.Request) {
	c.showUsers(w, r, http.StatusOK, settingsView{}, nil)
}

// invitationURL makes a link for someone to choose a password.
func (c *Console) invitationURL(r *http.Request, p auth.Principal, u store.User) (string, error) {
	code, err := c.auth.Invite(r.Context(), u.ID, p.Name)
	if err != nil {
		return "", err
	}
	return c.cfg.PublicURL(c.cfg.ConsoleHost) + "/invite?code=" + url.QueryEscape(code), nil
}

func (c *Console) createUser(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	email, name, role := r.PostFormValue("email"), r.PostFormValue("name"), r.PostFormValue("role")
	view := settingsView{FormEmail: email, FormName: name, FormRole: role}
	if role != store.RoleAdmin && role != store.RoleMember {
		c.showUsers(w, r, http.StatusUnprocessableEntity, view, &failure{Message: "A role is admin or member."})
		return
	}
	u, err := c.auth.NewUser(r.Context(), email, name, role, "")
	if err == nil {
		err = c.db.CreateUser(r.Context(), u)
	}
	var field *auth.FieldError
	switch {
	case errors.As(err, &field):
		c.showUsers(w, r, http.StatusUnprocessableEntity, view, &failure{Message: sentence(field.Detail)})
		return
	case errors.Is(err, store.ErrEmailTaken):
		c.showUsers(w, r, http.StatusConflict, view, &failure{Message: "There is already an account with that email address."})
		return
	case err != nil:
		c.internal(w, err)
		return
	}
	link, err := c.invitationURL(r, p, u)
	if err != nil {
		c.internal(w, err)
		return
	}
	c.showUsers(w, r, http.StatusCreated, settingsView{Invitation: link, InvitationUser: u.Email}, nil)
}

func (c *Console) lookupFormUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	u, err := c.db.UserByID(r.Context(), r.PostFormValue("user"))
	if errors.Is(err, store.ErrNotFound) {
		c.showUsers(w, r, http.StatusNotFound, settingsView{}, &failure{Message: "There is no such account."})
		return u, false
	}
	if err != nil {
		c.internal(w, err)
		return u, false
	}
	return u, true
}

func (c *Console) inviteUser(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	u, ok := c.lookupFormUser(w, r)
	if !ok {
		return
	}
	link, err := c.invitationURL(r, p, u)
	if err != nil {
		c.internal(w, err)
		return
	}
	c.showUsers(w, r, http.StatusCreated, settingsView{Invitation: link, InvitationUser: u.Email}, nil)
}

func (c *Console) changeUser(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	u, ok := c.lookupFormUser(w, r)
	if !ok {
		return
	}
	var err error
	done := ""
	switch r.PostFormValue("change") {
	case "role":
		role := r.PostFormValue("role")
		if role != store.RoleAdmin && role != store.RoleMember {
			c.showUsers(w, r, http.StatusUnprocessableEntity, settingsView{}, &failure{Message: "A role is admin or member."})
			return
		}
		err, done = c.db.SetUserRole(r.Context(), u.ID, role), "role"
	case "disable":
		err, done = c.db.SetUserDisabled(r.Context(), u.ID, true), "disabled"
	case "enable":
		err, done = c.db.SetUserDisabled(r.Context(), u.ID, false), "enabled"
	}
	if errors.Is(err, store.ErrLastAdmin) {
		c.showUsers(w, r, http.StatusConflict, settingsView{}, &failure{Message: "That would leave this server with no administrator."})
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	c.auth.Security("an account was changed", "user", u.Email, "change", done, "by", p.Name)
	http.Redirect(w, r, "/settings/users?done="+done, http.StatusSeeOther)
}

func (c *Console) showRegistries(w http.ResponseWriter, r *http.Request, code int, view settingsView, failed *failure) {
	_, d, ok := c.adminData(w, r, "registries", "Container registries")
	if !ok {
		return
	}
	var err error
	if view.Registries, err = c.db.Registries(r.Context()); err != nil {
		c.internal(w, err)
		return
	}
	d.Settings = view
	d.Failure = failed
	c.render(w, code, "settings-registries", d)
}

func (c *Console) registriesPage(w http.ResponseWriter, r *http.Request) {
	c.showRegistries(w, r, http.StatusOK, settingsView{}, nil)
}

func (c *Console) setRegistry(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	host := strings.ToLower(strings.TrimSpace(r.PostFormValue("host")))
	username, password := strings.TrimSpace(r.PostFormValue("username")), r.PostFormValue("password")
	view := settingsView{FormHost: host, FormUser: username}
	switch {
	case !names.ValidRegistryHost(host):
		c.showRegistries(w, r, http.StatusUnprocessableEntity, view, &failure{Message: "A registry is named by its host, such as ghcr.io."})
		return
	case username == "" || password == "":
		c.showRegistries(w, r, http.StatusUnprocessableEntity, view, &failure{Message: "A registry's credentials are a username and a password or token."})
		return
	case c.keyring == nil:
		c.showRegistries(w, r, http.StatusConflict, view, &failure{Message: "This server cannot seal credentials."})
		return
	}
	reg := store.Registry{Host: host, Username: username, Sealed: c.keyring.SealRegistry(host, password), CreatedAt: time.Now().UTC()}
	if err := c.db.SetRegistry(r.Context(), reg); err != nil {
		c.internal(w, err)
		return
	}
	c.log.Info("stored the credentials of a container registry", "host", host, "by", p.Name)
	http.Redirect(w, r, "/settings/registries?done=registry", http.StatusSeeOther)
}

func (c *Console) removeRegistry(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	host := r.PostFormValue("host")
	if err := c.db.DeleteRegistry(r.Context(), host); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.internal(w, err)
		return
	}
	c.log.Info("removed the credentials of a container registry", "host", host, "by", p.Name)
	http.Redirect(w, r, "/settings/registries?done=registry-removed", http.StatusSeeOther)
}

// systemPage says how the server stands, with what `moor server doctor` finds.
func (c *Console) systemPage(w http.ResponseWriter, r *http.Request) {
	_, d, ok := c.adminData(w, r, "system", "System")
	if !ok {
		return
	}
	view := settingsView{Checks: doctor.Run(r.Context(), doctor.Options{Config: c.cfg})}
	for _, check := range view.Checks {
		switch check.Status {
		case doctor.Fail:
			view.Fails++
		case doctor.Warn:
			view.Warns++
		case doctor.OK:
			view.Oks++
		}
	}
	info := buildinfo.Get()
	apps, err := c.db.Apps(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	running := 0
	for _, a := range apps {
		for _, in := range c.instances(a.Name) {
			if in.Healthy {
				running++
			}
		}
	}
	view.Facts = []setting{
		{"Version", info.String()},
		{"Platform", runtime.GOOS + "/" + runtime.GOARCH},
		{"Running since", ago(c.started)},
		{"Apps", strconv.Itoa(len(apps))},
		{"Healthy instances", strconv.Itoa(running)},
	}
	if stat, err := os.Stat(filepath.Join(c.cfg.DataDir, "moorage.db")); err == nil {
		view.Facts = append(view.Facts, setting{"Database", humanSizeOf(stat.Size())})
	}
	st := c.isolation()
	limits := "An instance over its memory limit is restarted"
	if st.Enforced() {
		limits = "Enforced by the kernel"
	}
	view.Facts = append(view.Facts, setting{"Isolation", sentence(st.Describe())}, setting{"Limits", limits})
	for _, note := range st.Notes {
		view.Facts = append(view.Facts, setting{"About isolation", sentence(note)})
	}
	d.Settings = view
	c.render(w, http.StatusOK, "settings-system", d)
}

func humanSizeOf(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
