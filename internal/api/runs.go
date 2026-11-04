package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func runDoc(r store.Run) apitypes.Run {
	doc := apitypes.Run{
		ID: r.ID, App: r.App, Env: r.Env, Job: r.Job, Command: r.Command, Trigger: r.Trigger, CreatedBy: r.CreatedBy, Version: r.Version,
		Status: r.Status, Error: r.Error, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
	if r.HasExit {
		code := r.ExitCode
		doc.ExitCode = &code
	}
	return doc
}

func (s *Server) runFailed(w http.ResponseWriter, err error) bool {
	var refused *deploy.NotRunnableError
	switch {
	case err == nil:
		return false
	case errors.As(err, &refused):
		problem(w, http.StatusConflict, "not_runnable", sentenceOf(err))
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", sentenceOf(err))
	default:
		s.internal(w, err)
	}
	return true
}

func (s *Server) cronJobs(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	jobs, err := s.deployer.CronJobs(r.Context(), app, env)
	if s.runFailed(w, err) {
		return
	}
	docs := make([]apitypes.CronJob, 0, len(jobs))
	for _, j := range jobs {
		doc := apitypes.CronJob{Name: j.Name, Schedule: j.Schedule, Command: j.Command, Timezone: j.Timezone, Overlap: j.Overlap, Timeout: j.Limit().String(), Next: j.Next}
		if j.Last != nil {
			last := runDoc(*j.Last)
			doc.Last = &last
		}
		docs = append(docs, doc)
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) runCronJob(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	run, err := s.deployer.RunCronJob(r.Context(), app, env, r.PathValue("job"), who(r).Name)
	if s.runFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, runDoc(run))
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	list, err := s.db.Runs(r.Context(), app, env, r.URL.Query().Get("job"), 100)
	if s.runFailed(w, err) {
		return
	}
	docs := make([]apitypes.Run, 0, len(list))
	for _, run := range list {
		docs = append(docs, runDoc(run))
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	var req apitypes.NewRun
	if !decode(w, r, &req) {
		return
	}
	var limit time.Duration
	if req.Timeout != "" {
		var d manifest.Duration
		if err := d.UnmarshalText([]byte(req.Timeout)); err != nil {
			problem(w, http.StatusUnprocessableEntity, "invalid_timeout", sentenceOf(err))
			return
		}
		limit = d.Std()
	}
	run, err := s.deployer.StartRun(lasting(r), deploy.RunRequest{App: app, Env: env, By: who(r).Name, Command: req.Command, Timeout: limit})
	if s.runFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, runDoc(run))
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	run, err := s.db.Run(r.Context(), r.PathValue("app"), r.PathValue("id"))
	if s.runFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, runDoc(run))
}

func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	if s.runFailed(w, s.deployer.StopRun(r.Context(), app, r.PathValue("id"))) {
		return
	}
	s.run(w, r)
}

// runLog streams what a run says as NDJSON lines, until it ends if follow=true.
func (s *Server) runLog(w http.ResponseWriter, r *http.Request) {
	app, id := r.PathValue("app"), r.PathValue("id")
	if _, err := s.db.Run(r.Context(), app, id); s.runFailed(w, err) {
		return
	}
	w.Header().Set("Content-Type", apitypes.NDJSONType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	write := func(l logs.Line) error {
		if err := enc.Encode(apitypes.LogLine{Time: l.Time, Stream: l.Stream, Text: l.Text}); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	if r.URL.Query().Get("follow") != "true" {
		lines, err := logs.FollowFromStart(deploy.LogPath(s.dataDir, app, id)).Read()
		if err != nil {
			s.log.Error("read a run's output", "run", id, "err", err)
			return
		}
		for _, l := range lines {
			if write(l) != nil {
				return
			}
		}
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
	if _, err := s.deployer.FollowRun(r.Context(), app, id, write); err != nil && r.Context().Err() == nil {
		s.log.Warn("follow a run", "run", id, "err", err)
	}
}
