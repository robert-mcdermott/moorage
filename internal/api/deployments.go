package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func (s *Server) deploymentDoc(d store.Deployment) apitypes.Deployment {
	doc := apitypes.Deployment{
		ID: d.ID, App: d.App, Env: d.Env, URL: s.appURL(d.App), Source: d.SourceType, Status: d.Status,
		Build: d.BuildID, Error: d.Error, Output: d.Output, CreatedBy: d.CreatedBy,
		CreatedAt: d.CreatedAt, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
	}
	if d.Version > 0 {
		doc.Release = &apitypes.Release{Version: d.Version, Build: d.BuildID}
	}
	return doc
}

func (s *Server) releaseDoc(r store.Release) *apitypes.Release {
	rel := release(r)
	return &rel
}

func (s *Server) deployments(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	limit := 20
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 200 {
		limit = n
	}
	deps, err := s.db.Deployments(r.Context(), app, env, limit)
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]apitypes.Deployment, 0, len(deps))
	for _, d := range deps {
		out = append(out, s.deploymentDoc(d))
	}
	writeJSON(w, http.StatusOK, out)
}

// lookupDeployment reads the {id} path value and its deployment.
func (s *Server) lookupDeployment(w http.ResponseWriter, r *http.Request) (store.Deployment, bool) {
	id := r.PathValue("id")
	if prefix, _, err := ids.Parse(id); err != nil || prefix != ids.Deployment {
		problem(w, http.StatusNotFound, "not_found", "There is no deployment "+id+".")
		return store.Deployment{}, false
	}
	d, err := s.db.Deployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusNotFound, "not_found", err.Error())
		return d, false
	}
	if err != nil {
		s.internal(w, err)
		return d, false
	}
	return d, true
}

func (s *Server) deployment(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDeployment(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.deploymentDoc(d))
}

// deploymentLog streams a deployment's log as NDJSON lines. With
// follow=true, the stream stays open until the deployment has finished.
func (s *Server) deploymentLog(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDeployment(w, r)
	if !ok {
		return
	}
	follow := r.URL.Query().Get("follow") == "true"
	w.Header().Set("Content-Type", apitypes.NDJSONType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	f := logs.FollowFromStart(deploy.LogPath(s.dataDir, d.App, d.ID))
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		// The deployment's status is read before its log: a deployment
		// writes its last line before it records its end.
		finished := store.Final(d.Status)
		lines, err := f.Read()
		if err != nil {
			s.log.Error("read a deployment log", "deployment", d.ID, "err", err)
			return
		}
		for _, l := range lines {
			if err := enc.Encode(apitypes.LogLine{Time: l.Time, Stream: l.Stream, Text: l.Text}); err != nil {
				return
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		if finished || !follow {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
		if d, err = s.db.Deployment(r.Context(), d.ID); err != nil {
			return
		}
	}
}
