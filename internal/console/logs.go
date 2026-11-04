package console

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/applogs"
)

// logsPageLines is how many lines the logs page starts with.
const logsPageLines = 500

// logsFollowLines is how many a follower that comes back may catch up on.
const logsFollowLines = 2000

// logsView is the logs page.
type logsView struct {
	// What the form says.
	Of, Grep, Range, From, To string
	Problems                  bool
	Sources                   []logSource
	Instances                 []string
	Processes                 []string
	// Lines are the lines, oldest first. Older continues further back, Newest returns.
	Lines         []logLineView
	Older, Newest string
	// Stream is where a page follows the lines from, unless it ends at a moment.
	Stream string
	// After is where the lines end, for following them from there.
	After string
	// Download is the address of them all, as a file.
	Download string
	// FormError says what is wrong with the form.
	FormError string
}

type logSource struct {
	Name, Label string
	On          bool
}

type logLineView struct {
	At      time.Time
	Source  string
	Who     string
	Level   string
	Text    string
	Raw     string
	Problem bool
}

var sourceLabels = map[string]string{
	applogs.Stdout: "Output", applogs.Stderr: "Errors", applogs.Moorage: "Moorage",
	applogs.Build: "Builds", applogs.Access: "Requests", applogs.Events: "Events",
}

// timeRanges are what the time range offers.
var timeRanges = []struct{ Value, Label string }{
	{"", "All of it"}, {"15m", "Last 15 minutes"}, {"1h", "Last hour"}, {"6h", "Last 6 hours"},
	{"24h", "Last 24 hours"}, {"7d", "Last 7 days"}, {"custom", "Between…"},
}

func lineView(e applogs.Entry) logLineView {
	v := logLineView{At: e.Time, Source: e.Source, Who: e.Who(), Level: e.Level, Text: ansi.ReplaceAllString(e.Display(), ""), Problem: e.Problem()}
	if e.Message != "" {
		v.Raw = e.Text
	}
	return v
}

func (c *Console) logsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	d, err := c.appData(r, p, app, "apps", "Logs of "+app)
	if err != nil {
		c.internal(w, err)
		return
	}
	if !d.Access.Logs {
		c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "You may not read its logs", Message: "Viewers of the project " + d.Access.Project.Name + " may not read logs.", Link: "/apps/" + url.PathEscape(app), LinkText: "Back to the app"})
		return
	}
	d.Crumbs = []crumb{{"Apps", "/"}, {app, "/apps/" + url.PathEscape(app)}}
	d.Tab = "logs"
	d.App = appView{Name: app, URL: c.cfg.EnvURL(app, envOf(r))}
	view, err := c.logsView(r.Context(), r.URL.Query(), app, envOf(r))
	if err != nil {
		c.internal(w, err)
		return
	}
	d.Logs = view
	d.TimeRanges = timeRanges
	code := http.StatusOK
	if view.FormError != "" {
		code = http.StatusUnprocessableEntity
	}
	c.render(w, code, "logs", d)
}

// logsView reads what the page's address asks for: the page works as a plain form.
func (c *Console) logsView(ctx context.Context, form url.Values, app, env string) (logsView, error) {
	reader := applogs.Reader{DB: c.db, DataDir: c.cfg.DataDir}
	view := logsView{Of: form.Get("of"), Grep: strings.TrimSpace(form.Get("q")), Range: form.Get("range"), From: form.Get("from"), To: form.Get("to"), Problems: form.Get("problems") == "1"}
	seen := map[string]bool{}
	view.Instances = reader.Instances(app, env)
	for _, name := range view.Instances {
		process, _, _ := strings.Cut(name, ".")
		if !seen[process] {
			seen[process] = true
			view.Processes = append(view.Processes, process)
		}
	}
	q := applogs.Query{Grep: view.Grep, Problems: view.Problems, Tail: logsPageLines}
	q.Process, q.Instance, _ = strings.Cut(view.Of, ".")
	for _, name := range form["source"] {
		if slices.Contains(applogs.AllSources, name) {
			q.Sources = append(q.Sources, name)
		}
	}
	on := q.Sources
	if len(on) == 0 {
		on = applogs.DefaultSources
	}
	for _, name := range applogs.AllSources {
		view.Sources = append(view.Sources, logSource{name, sourceLabels[name], slices.Contains(on, name)})
	}

	// The browser's clock says what a moment typed into the form means.
	zone := time.Local
	if minutes, err := strconv.Atoi(form.Get("tz")); err == nil && minutes > -24*60 && minutes < 24*60 {
		zone = time.FixedZone("browser", -minutes*60)
	}
	now := time.Now()
	var err error
	switch view.Range {
	case "":
	case "custom":
		if q.Since, err = applogs.ParseTime(view.From, now, zone); err == nil {
			q.Until, err = applogs.ParseTime(view.To, now, zone)
		}
	default:
		q.Since, err = applogs.ParseTime(view.Range, now, zone)
	}
	if err != nil {
		view.FormError = sentence(err.Error())
		return view, nil
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && q.Until.Before(q.Since) {
		view.FormError = "The end of that range is before its start."
		return view, nil
	}
	// Going further back ends the lines at the moment the page began at.
	if before := form.Get("before"); before != "" {
		if t, err := time.Parse(time.RFC3339Nano, before); err == nil {
			q.Until = t
		}
	}
	live := q.Until.IsZero()

	entries, more, err := reader.Read(ctx, app, env, q)
	if err != nil {
		return view, err
	}
	for _, e := range entries {
		view.Lines = append(view.Lines, lineView(e))
	}
	page := url.Values{}
	for name, values := range form {
		if name != "before" {
			page[name] = values
		}
	}
	base := "/apps/" + url.PathEscape(app) + "/logs"
	if more && len(entries) > 0 {
		older := url.Values{}
		for name, values := range page {
			older[name] = values
		}
		older.Set("before", entries[0].Time.Add(-time.Microsecond).UTC().Format(time.RFC3339Nano))
		view.Older = base + "?" + older.Encode()
	}
	if form.Get("before") != "" {
		view.Newest = base
		if len(page) > 0 {
			view.Newest += "?" + page.Encode()
		}
	}

	api := url.Values{}
	if q.Process != "" {
		api.Set("process", q.Process)
	}
	if q.Instance != "" {
		api.Set("instance", q.Instance)
	}
	if len(q.Sources) > 0 {
		api.Set("source", strings.Join(q.Sources, ","))
	}
	if q.Grep != "" {
		api.Set("grep", q.Grep)
	}
	if q.Problems {
		api.Set("problems", "1")
	}
	if !q.Since.IsZero() {
		api.Set("since", q.Since.UTC().Format(time.RFC3339Nano))
	}
	path := "/api/v1/apps/" + url.PathEscape(app) + "/envs/" + url.PathEscape(env) + "/logs"
	if live {
		stream := url.Values{"follow": {"1"}, "tail": {strconv.Itoa(logsFollowLines)}}
		for name, values := range api {
			stream[name] = values
		}
		view.Stream = path + "?" + stream.Encode()
		view.After = now.UTC().Format(time.RFC3339Nano)
		if n := len(entries); n > 0 {
			view.After = entries[n-1].Time.UTC().Format(time.RFC3339Nano)
		}
	} else {
		api.Set("until", q.Until.UTC().Format(time.RFC3339Nano))
	}
	api.Set("download", "1")
	api.Set("format", "text")
	view.Download = path + "?" + api.Encode()
	return view, nil
}

func clock(t time.Time) string { return t.UTC().Format("Jan 02 15:04:05") }

// countText says how many of something there are.
func countText(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
