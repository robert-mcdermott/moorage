package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func (s *Server) environmentMetrics(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	span, known := metrics.RangeOf(r.URL.Query().Get("range"))
	if !known {
		problem(w, http.StatusUnprocessableEntity, "invalid_range", "A range is live, 1h, 24h, 7d or 30d.")
		return
	}
	envID, err := s.db.EnvID(r.Context(), app, env)
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusNotFound, "not_found", sentenceOf(err))
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	to := time.Now()
	from := to.Add(-span.Span)
	charts, step, err := s.metrics.View(r.Context(), envID, from, to, span.Points)
	if err != nil {
		s.internal(w, err)
		return
	}
	doc := apitypes.Metrics{From: from.UTC(), To: to.UTC(), Step: int(step.Seconds()), Charts: []apitypes.MetricChart{}, Deployments: []apitypes.MetricMark{}}
	for _, c := range charts {
		chart := apitypes.MetricChart{Key: c.Key, Title: c.Title, Unit: c.Unit, Lines: []apitypes.MetricLine{}}
		for _, l := range c.Lines {
			line := apitypes.MetricLine{Name: l.Name, Points: make([]apitypes.MetricPoint, 0, len(l.Points))}
			for _, p := range l.Points {
				line.Points = append(line.Points, apitypes.MetricPoint{At: p.At.UTC(), Value: p.Avg, Min: p.Min, Max: p.Max})
			}
			chart.Lines = append(chart.Lines, line)
		}
		doc.Charts = append(doc.Charts, chart)
	}
	marks, err := s.db.ReleaseMarks(r.Context(), app, env, from, to)
	if err != nil {
		s.internal(w, err)
		return
	}
	for _, m := range marks {
		doc.Deployments = append(doc.Deployments, apitypes.MetricMark{At: m.At, Version: m.Version, By: m.By})
	}
	writeJSON(w, http.StatusOK, doc)
}

// Prometheus answers on the console's hostname at /metrics, as Prometheus expects to find it. It takes
// a token that can read, and says only what its holder could see anyway. The server's own numbers
// are for administrators.
func Prometheus(db *store.DB, authn *auth.Service, m *metrics.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET")
			http.Error(w, "Method not allowed\n", http.StatusMethodNotAllowed)
			return
		}
		p, err := authn.Request(r)
		if err != nil || !p.Can(auth.ScopeRead) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="moorage"`)
			http.Error(w, "This takes an API token that can read\n", http.StatusUnauthorized)
			return
		}
		visible, err := authn.Visible(r.Context(), p)
		if err != nil {
			http.Error(w, "The server could not complete the request\n", http.StatusInternalServerError)
			return
		}
		list, err := db.AllEnvironments(r.Context())
		if err != nil {
			http.Error(w, "The server could not complete the request\n", http.StatusInternalServerError)
			return
		}
		envs := make([]metrics.PromEnv, 0, len(list))
		for _, e := range list {
			if visible(e.App) && (p.App == "" || p.App == e.App) {
				envs = append(envs, metrics.PromEnv{App: e.App, Env: e.Name, EnvID: e.ID})
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		m.WriteProm(w, envs, p.Admin && p.App == "")
	})
}
