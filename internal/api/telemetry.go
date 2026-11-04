package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/notify"
	"github.com/robert-mcdermott/moorage/internal/store"
	"github.com/robert-mcdermott/moorage/internal/telemetry"
)

func sinkDoc(k store.TelemetrySink) apitypes.TelemetrySink {
	return apitypes.TelemetrySink{Name: k.Name, Kind: k.Kind, Address: k.Address, HasHeaders: len(k.SealedHeaders) > 0, Logs: k.Logs, Metrics: k.Metrics, App: k.App,
		LastSentAt: k.LastSentAt, LastError: k.LastError, Dropped: k.Dropped, CreatedBy: k.CreatedBy, CreatedAt: k.CreatedAt}
}

func (s *Server) telemetrySinks(w http.ResponseWriter, r *http.Request) {
	list, err := s.db.TelemetrySinks(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]apitypes.TelemetrySink, 0, len(list))
	for _, k := range list {
		out = append(out, sinkDoc(k))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) sinkFailed(w http.ResponseWriter, err error) {
	var invalid *telemetry.InvalidError
	switch {
	case errors.As(err, &invalid):
		problem(w, http.StatusUnprocessableEntity, "invalid", invalid.Msg)
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", "There is no such sink.")
	default:
		s.internal(w, err)
	}
}

func (s *Server) setTelemetrySink(w http.ResponseWriter, r *http.Request) {
	var in apitypes.NewTelemetrySink
	if !decode(w, r, &in) {
		return
	}
	appID := ""
	if in.App != "" {
		id, err := s.db.AppID(r.Context(), in.App)
		if errors.Is(err, store.ErrNotFound) {
			problem(w, http.StatusUnprocessableEntity, "invalid", "There is no app "+in.App+".")
			return
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		appID = id
	}
	k, err := s.telemetry.Set(r.Context(), telemetry.Form{Name: in.Name, Kind: in.Kind, Address: in.Address, Headers: in.Headers, Logs: in.Logs, Metrics: in.Metrics, AppID: appID}, who(r).Name)
	if err != nil {
		s.sinkFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sinkDoc(k))
}

func (s *Server) removeTelemetrySink(w http.ResponseWriter, r *http.Request) {
	if err := s.telemetry.Remove(r.Context(), r.PathValue("name")); err != nil {
		s.sinkFailed(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testTelemetrySink(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.db.TelemetrySink(r.Context(), name); err != nil {
		s.sinkFailed(w, err)
		return
	}
	if err := s.telemetry.Test(r.Context(), name, who(r).Name); err != nil {
		problem(w, http.StatusBadGateway, "not_sent", "It could not be sent: "+sentenceOf(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) thresholdDoc(t store.AlertThreshold) apitypes.Threshold {
	doc := apitypes.Threshold{ID: t.ID, Metric: t.Metric, Label: notify.ThresholdLabel(t.Metric), Above: t.Above, Minutes: t.Minutes, CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt}
	doc.Firing = t.Firing
	if value, known, firing := s.notifier.ThresholdStatus(t.ID); known {
		doc.Value, doc.Firing = &value, firing
	}
	return doc
}

func (s *Server) thresholds(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	e, err := s.db.Environment(r.Context(), app, env)
	if s.runFailed(w, err) {
		return
	}
	list, err := s.db.AlertThresholds(r.Context(), e.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]apitypes.Threshold, 0, len(list))
	for _, t := range list {
		out = append(out, s.thresholdDoc(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) addThreshold(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	var in apitypes.NewThreshold
	if !decode(w, r, &in) {
		return
	}
	if in.Minutes == 0 {
		in.Minutes = 5
	}
	if why := notify.ThresholdCheck(in.Metric, in.Above, in.Minutes); why != "" {
		problem(w, http.StatusUnprocessableEntity, "invalid", why)
		return
	}
	e, err := s.db.Environment(r.Context(), app, env)
	if s.runFailed(w, err) {
		return
	}
	t := store.AlertThreshold{ID: ids.New(ids.Threshold), EnvID: e.ID, App: app, Env: env, Metric: in.Metric, Above: in.Above, Minutes: in.Minutes, CreatedBy: who(r).Name, CreatedAt: time.Now().UTC()}
	if err := s.db.AddAlertThreshold(r.Context(), t); err != nil {
		if errors.Is(err, store.ErrThresholdExists) {
			problem(w, http.StatusConflict, "exists", sentenceOf(err))
			return
		}
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.thresholdDoc(t))
}

func (s *Server) removeThreshold(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	e, err := s.db.Environment(r.Context(), app, env)
	if s.runFailed(w, err) {
		return
	}
	if s.runFailed(w, s.db.DeleteAlertThreshold(r.Context(), e.ID, r.PathValue("id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
