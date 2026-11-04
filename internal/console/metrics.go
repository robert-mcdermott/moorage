package console

import (
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
)

// metricsView is an environment's Metrics page.
type metricsView struct {
	Ranges []rangeLink
	Charts []chartView
	Live   bool
	Empty  bool
}

type rangeLink struct {
	Label, Href string
	Current     bool
}

// glanceTile is a number and its shape, for a glance at how something is doing.
type glanceTile struct {
	Label, Value string
	Spark        template.HTML
}

func (c *Console) metricsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	c.showMetrics(w, r, p, app)
}

func (c *Console) showMetrics(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	d, err := c.appData(r, p, app, "apps", "Metrics of "+app)
	if err != nil {
		c.internal(w, err)
		return
	}
	env := envOf(r)
	d.Crumbs = []crumb{{"Apps", "/"}, {app, appAddress(app, env, "", nil)}}
	d.Tab = "metrics"
	d.App = appView{Name: app, URL: c.cfg.EnvURL(app, env)}
	span, _ := metrics.RangeOf(r.URL.Query().Get("range"))
	view := &metricsView{Live: span.Key == "live", Empty: true}
	for _, rng := range metrics.Ranges {
		view.Ranges = append(view.Ranges, rangeLink{Label: rng.Label, Href: appAddress(app, env, "/metrics", url.Values{"range": {rng.Key}}), Current: rng.Key == span.Key})
	}
	if c.metrics != nil {
		envID, err := c.db.EnvID(r.Context(), app, env)
		if err != nil {
			c.internal(w, err)
			return
		}
		to := time.Now()
		from := to.Add(-span.Span)
		charts, step, err := c.metrics.View(r.Context(), envID, from, to, span.Points)
		if err != nil {
			c.internal(w, err)
			return
		}
		marks, err := c.db.ReleaseMarks(r.Context(), app, env, from, to)
		if err != nil {
			c.internal(w, err)
			return
		}
		for _, chart := range charts {
			built := buildChart(chart, from, to, step, marks)
			view.Empty = view.Empty && built.Empty
			view.Charts = append(view.Charts, built)
		}
	}
	d.Metrics = view
	d.Poll = view.Live
	c.render(w, http.StatusOK, "app-metrics", d)
}

// glance says how an environment has been doing lately, in a few numbers.
func (c *Console) glance(envID string, static bool) []glanceTile {
	if c.metrics == nil {
		return nil
	}
	now := c.metrics.Glance(envID, time.Now())
	tiles := []glanceTile{
		{Label: "Requests", Value: formatValue("rate", now.RequestsPerMinute), Spark: sparkline(now.Requests, "Requests a minute over the last hour")},
	}
	latency := "—"
	if now.HasP95 {
		latency = formatValue("ms", now.P95)
	}
	tiles = append(tiles, glanceTile{Label: "95% answered within", Value: latency, Spark: sparkline(now.Latency, "95th percentile response time over the last hour")})
	if !static {
		cpu, mem := "—", "—"
		if now.HasUsage {
			cpu, mem = formatValue("percent", now.CPU), formatValue("bytes", now.Memory)
		}
		tiles = append(tiles,
			glanceTile{Label: "CPU", Value: cpu, Spark: sparkline(now.CPULine, "CPU over the last hour")},
			glanceTile{Label: "Memory", Value: mem, Spark: sparkline(now.MemLine, "Memory over the last hour")})
	}
	return tiles
}
