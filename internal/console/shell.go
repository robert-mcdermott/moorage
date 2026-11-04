package console

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/doctor"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// The commands that install moor: the same as the README's and docs/install.md's.
const (
	installUnix    = "curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/robert-mcdermott/moorage/main/install.sh | sh"
	installWindows = "irm https://raw.githubusercontent.com/robert-mcdermott/moorage/main/install.ps1 | iex"
)

// activityPage is how many events a page of the timeline shows.
const activityPage = 50

// maxListedApps bounds the apps the pages hold; there is a limit to what a
// person reads as a list, and to what a server runs.
const maxListedApps = 500

// appRow is a line of the apps list.
type appRow struct {
	Name, URL string
	Status    status
	Version   int
	ChangedAt time.Time
	ChangedBy string
	// Instances says how many are healthy, of how many.
	Instances string
	// Last says what its newest deployment deployed, LastFailed that it failed
	// while a release keeps serving, and Deploying that it is under way.
	Last                  string
	LastFailed, Deploying bool
}

// crumb is a step of the way back from a page.
type crumb struct{ Label, Href string }

// eventRow is an entry of the timeline.
type eventRow struct {
	ID, App, Summary, Level string
	At                      time.Time
}

// paletteItem is an entry of the command palette.
type paletteItem struct {
	Label    string `json:"label"`
	Hint     string `json:"hint,omitempty"`
	Href     string `json:"href,omitempty"`
	Action   string `json:"action,omitempty"`
	External bool   `json:"external,omitempty"`
}

// ago says how long ago a time was. Scripts keep it true while a page stays open.
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "min") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "h") + " ago"
	case d < 48*time.Hour:
		return "yesterday"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "days") + " ago"
	}
	return t.Format("2 Jan 2006")
}

func plural(n int, unit string) string { return strconv.Itoa(n) + " " + unit }

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// requireUser returns who is signed in, or sends them to sign in.
func (c *Console) requireUser(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := c.signedIn(r)
	if !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	}
	return p, ok
}

// shellData is what every page of the console's frame needs.
func (c *Console) shellData(r *http.Request, p auth.Principal, page, title string) (data, []store.AppSummary, error) {
	apps, err := c.db.AppsAfter(r.Context(), "", maxListedApps)
	if err != nil {
		return data{}, nil, err
	}
	d := data{
		Title: title, Heading: title, Page: page,
		User: p.Name, Admin: p.Admin, CSRF: auth.CSRFToken(p.CookieValue),
		Palette: c.palette(apps, p.Admin),
	}
	for _, a := range apps {
		d.AppNames = append(d.AppNames, a.Name)
	}
	return d, apps, nil
}

func (c *Console) appURL(name string) string {
	return c.cfg.PublicURL(name + "." + c.cfg.Domain)
}

// palette is what the command palette offers.
func (c *Console) palette(apps []store.AppSummary, admin bool) []paletteItem {
	items := []paletteItem{
		{Label: "Apps", Hint: "Page", Href: "/"},
		{Label: "Activity", Hint: "Page", Href: "/activity"},
		{Label: "Getting started", Hint: "Page", Href: "/welcome"},
		{Label: "New app", Hint: "Page", Href: "/new"},
		{Label: "Your account", Hint: "Account", Href: "/account"},
		{Label: "SSH keys", Hint: "Account", Href: "/account/keys"},
		{Label: "API tokens", Hint: "Account", Href: "/account/tokens"},
		{Label: "Sessions", Hint: "Account", Href: "/account/sessions"},
	}
	if admin {
		items = append(items,
			paletteItem{Label: "Settings", Hint: "Server", Href: "/settings"},
			paletteItem{Label: "Users", Hint: "Server", Href: "/settings/users"},
			paletteItem{Label: "Container registries", Hint: "Server", Href: "/settings/registries"},
			paletteItem{Label: "System", Hint: "Server", Href: "/settings/system"},
		)
	}
	for _, a := range apps {
		items = append(items,
			paletteItem{Label: a.Name, Hint: "App", Href: "/apps/" + url.PathEscape(a.Name)},
			paletteItem{Label: "Deployments of " + a.Name, Hint: "App", Href: "/apps/" + url.PathEscape(a.Name) + "/deployments"},
			paletteItem{Label: "Logs of " + a.Name, Hint: "App", Href: "/apps/" + url.PathEscape(a.Name) + "/logs"},
			paletteItem{Label: "Variables of " + a.Name, Hint: "App", Href: "/apps/" + url.PathEscape(a.Name) + "/variables"},
			paletteItem{Label: "Deploy " + a.Name, Hint: "App", Href: "/apps/" + url.PathEscape(a.Name) + "/deploy"},
			paletteItem{Label: "Open " + a.Name, Hint: "Its address", Href: c.appURL(a.Name), External: true},
		)
	}
	return append(items,
		paletteItem{Label: "Use the system's theme", Hint: "Theme", Action: "theme:system"},
		paletteItem{Label: "Use the light theme", Hint: "Theme", Action: "theme:light"},
		paletteItem{Label: "Use the dark theme", Hint: "Theme", Action: "theme:dark"},
		paletteItem{Label: "Sign out", Hint: "Account", Action: "signout"},
	)
}

func (c *Console) activity(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	d, _, err := c.shellData(r, p, "activity", "Activity")
	if err != nil {
		c.internal(w, err)
		return
	}
	q := r.URL.Query()
	d.FilterApp = q.Get("app")
	before := q.Get("before")
	list, err := c.db.Events(r.Context(), store.EventFilter{App: d.FilterApp, Before: before, Limit: activityPage + 1})
	if err != nil {
		c.internal(w, err)
		return
	}
	if len(list) > activityPage {
		list = list[:activityPage]
		older := url.Values{"before": {list[activityPage-1].ID}}
		if d.FilterApp != "" {
			older.Set("app", d.FilterApp)
		}
		d.Older = "/activity?" + older.Encode()
	}
	for _, e := range list {
		d.Events = append(d.Events, eventRow{ID: e.ID, App: e.App, Summary: events.Summary(e), Level: events.Level(e.Type), At: e.At})
	}
	// Only the newest page grows by itself, from where it was drawn.
	d.Live = before == ""
	if d.Live && len(list) > 0 {
		d.Stream = "/api/v1/events/stream?after=" + url.QueryEscape(list[0].ID)
	}
	c.render(w, http.StatusOK, "activity", d)
}

func (c *Console) welcome(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	d, apps, err := c.shellData(r, p, "welcome", "Getting started")
	if err != nil {
		c.internal(w, err)
		return
	}
	for _, a := range apps {
		if a.Production != nil {
			d.Apps = append(d.Apps, appRow{Name: a.Name})
		}
	}
	console := c.cfg.PublicURL(c.cfg.ConsoleHost)
	d.InstallUnix, d.InstallWindows = installUnix, installWindows
	d.LoginCommand = "moor login " + console
	d.AppPattern = c.appURL("<app>")
	if p.Admin {
		d.Checks = doctor.Setup(r.Context(), doctor.Options{Config: c.cfg})
		d.ChecksPassed = doctor.Worst(d.Checks) == doctor.OK
	}
	c.render(w, http.StatusOK, "welcome", d)
}
