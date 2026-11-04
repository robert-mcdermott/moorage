package console

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/notify"
	"github.com/robert-mcdermott/moorage/internal/store"
	"github.com/robert-mcdermott/moorage/internal/telemetry"
)

// telemetryView is the settings page of where things are sent.
type telemetryView struct {
	Sinks []store.TelemetrySink
	Apps  []string
	Form  sinkForm
	// Tested says how a test just went.
	Tested string
}

type sinkForm struct {
	Name, Kind, Address, Headers, App string
	Logs, Metrics                     bool
}

func (c *Console) telemetrySettings(w http.ResponseWriter, r *http.Request) {
	c.showTelemetry(w, r, http.StatusOK, telemetryView{Form: sinkForm{Logs: true}}, nil)
}

func (c *Console) showTelemetry(w http.ResponseWriter, r *http.Request, code int, view telemetryView, failed *failure) {
	_, d, ok := c.adminData(w, r, "telemetry", "Telemetry")
	if !ok {
		return
	}
	sinks, err := c.db.TelemetrySinks(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	view.Sinks = sinks
	envs, err := c.db.AllEnvironments(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	seen := map[string]bool{}
	for _, e := range envs {
		if !seen[e.App] {
			seen[e.App] = true
			view.Apps = append(view.Apps, e.App)
		}
	}
	d.Telemetry = &view
	d.Failure = failed
	d.Notice = map[string]string{"removed": "It is no longer being sent there."}[r.URL.Query().Get("done")]
	c.render(w, code, "settings-telemetry", d)
}

func headersOf(text string) (map[string]string, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			return nil, errors.New("A header goes Name: value, on a line of its own.")
		}
		out[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return out, nil
}

func (c *Console) addSink(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	form := sinkForm{Name: strings.TrimSpace(r.PostFormValue("name")), Kind: r.PostFormValue("kind"), Address: strings.TrimSpace(r.PostFormValue("address")), App: r.PostFormValue("app"),
		Logs: r.PostFormValue("logs") != "", Metrics: r.PostFormValue("metrics") != ""}
	refuse := func(message string) {
		c.showTelemetry(w, r, http.StatusUnprocessableEntity, telemetryView{Form: form}, &failure{Message: message})
	}
	headers, err := headersOf(r.PostFormValue("headers"))
	if err != nil {
		refuse(err.Error())
		return
	}
	appID := ""
	if form.App != "" {
		if appID, err = c.db.AppID(r.Context(), form.App); err != nil {
			refuse("There is no app " + form.App + ".")
			return
		}
	}
	if _, err := c.telemetry.Set(r.Context(), telemetry.Form{Name: form.Name, Kind: form.Kind, Address: form.Address, Headers: headers, Logs: form.Logs, Metrics: form.Metrics, AppID: appID}, p.Name); err != nil {
		var invalid *telemetry.InvalidError
		if errors.As(err, &invalid) {
			refuse(invalid.Msg)
			return
		}
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/settings/telemetry", http.StatusSeeOther)
}

func (c *Console) testSink(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	name := r.PostFormValue("name")
	if err := c.telemetry.Test(r.Context(), name, p.Name); err != nil {
		c.showTelemetry(w, r, http.StatusBadGateway, telemetryView{Form: sinkForm{Logs: true}}, &failure{Message: name + " could not be reached: " + err.Error()})
		return
	}
	c.showTelemetry(w, r, http.StatusOK, telemetryView{Form: sinkForm{Logs: true}, Tested: name}, nil)
}

func (c *Console) removeSink(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if err := c.telemetry.Remove(r.Context(), r.PostFormValue("name")); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/settings/telemetry?done=removed", http.StatusSeeOther)
}

// thresholdRow is a threshold, on an app's metrics page.
type thresholdRow struct {
	store.AlertThreshold
	Label, Unit, Now string
	Firing           bool
}

// thresholdsView is the part of an app's metrics page that says when someone is told.
type thresholdsView struct {
	Rows  []thresholdRow
	Kinds []thresholdKind
}

type thresholdKind struct{ Metric, Label, Unit string }

func (c *Console) thresholdData(r *http.Request, app, env string) (*thresholdsView, error) {
	e, err := c.db.Environment(r.Context(), app, env)
	if err != nil {
		return nil, err
	}
	list, err := c.db.AlertThresholds(r.Context(), e.ID)
	if err != nil {
		return nil, err
	}
	view := &thresholdsView{}
	units := map[string]string{}
	for _, k := range notify.ThresholdKinds {
		view.Kinds = append(view.Kinds, thresholdKind(k))
		units[k.Metric] = k.Unit
	}
	for _, t := range list {
		row := thresholdRow{AlertThreshold: t, Label: notify.ThresholdLabel(t.Metric), Unit: units[t.Metric], Now: "Nothing to compare with"}
		if c.notifier != nil {
			if value, known, firing := c.notifier.ThresholdStatus(t.ID); known {
				row.Now, row.Firing = strconv.FormatFloat(value, 'f', 1, 64)+" "+units[t.Metric], firing
			}
		}
		view.Rows = append(view.Rows, row)
	}
	return view, nil
}

func (c *Console) addThreshold(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	env := envOf(r)
	above, err := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("above")), 64)
	if err != nil {
		above = 0
	}
	minutes, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("minutes")))
	if err != nil {
		minutes = 0
	}
	metric := r.PostFormValue("metric")
	back := func(failure string) {
		http.Redirect(w, r, appAddress(app, env, "/metrics", url.Values{"threshold": {failure}})+"#thresholds", http.StatusSeeOther)
	}
	if why := notify.ThresholdCheck(metric, above, minutes); why != "" {
		back(why)
		return
	}
	e, err := c.db.Environment(r.Context(), app, env)
	if err != nil {
		c.internal(w, err)
		return
	}
	t := store.AlertThreshold{ID: ids.New(ids.Threshold), EnvID: e.ID, Metric: metric, Above: above, Minutes: minutes, CreatedBy: p.Name, CreatedAt: time.Now().UTC()}
	if err := c.db.AddAlertThreshold(r.Context(), t); err != nil {
		if errors.Is(err, store.ErrThresholdExists) {
			back("That threshold is there already.")
			return
		}
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, appAddress(app, env, "/metrics", nil)+"#thresholds", http.StatusSeeOther)
}

func (c *Console) removeThreshold(w http.ResponseWriter, r *http.Request, _ auth.Principal, app string) {
	env := envOf(r)
	e, err := c.db.Environment(r.Context(), app, env)
	if err != nil {
		c.internal(w, err)
		return
	}
	if err := c.db.DeleteAlertThreshold(r.Context(), e.ID, r.PostFormValue("id")); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, appAddress(app, env, "/metrics", nil)+"#thresholds", http.StatusSeeOther)
}
