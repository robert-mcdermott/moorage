package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"context"
	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func (s *Server) deploymentDoc(d store.Deployment) apitypes.Deployment {
	doc := apitypes.Deployment{
		ID: d.ID, App: d.App, Env: d.Env, URL: s.envURL(d.App, d.Env), Source: d.SourceType, Status: d.Status,
		Ref: d.SourceRef, Commit: d.SourceCommit, Message: d.SourceMessage,
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
	limit, cursor, ok := pageParams(w, r, 20)
	if !ok {
		return
	}
	var created time.Time
	var id string
	if cursor != "" {
		at, rest, _ := strings.Cut(cursor, "/")
		ms, err := strconv.ParseInt(at, 10, 64)
		if err != nil || rest == "" {
			problem(w, http.StatusBadRequest, "invalid_cursor", "That cursor is not one the API gave.")
			return
		}
		created, id = time.UnixMilli(ms), rest
	}
	access, err := s.auth.AppAccess(r.Context(), who(r), app)
	if err != nil {
		s.internal(w, err)
		return
	}
	deps, err := s.db.DeploymentsBefore(r.Context(), app, env, created, id, limit+1)
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	page := apitypes.Page[apitypes.Deployment]{Items: []apitypes.Deployment{}}
	for i, d := range deps {
		if i == limit {
			last := deps[i-1]
			page.NextCursor = encodeCursor(strconv.FormatInt(last.CreatedAt.UnixMilli(), 10) + "/" + last.ID)
			break
		}
		// What its release command said is a log, and may be kept from viewers.
		if !access.Logs {
			d.Output = nil
		}
		page.Items = append(page.Items, s.deploymentDoc(d))
	}
	writeJSON(w, http.StatusOK, page)
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
	access, err := s.auth.AppAccess(r.Context(), who(r), d.App)
	if err != nil {
		s.internal(w, err)
		return d, false
	}
	// A token limited to previews sees nothing of what else there is.
	seen := access.See && !s.outOfBounds(r, d.App, d.Env)
	if !seen {
		problem(w, http.StatusNotFound, "not_found", "There is no deployment "+id+".")
		return d, false
	}
	if !access.Logs {
		d.Output = nil
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
// follow=true, the stream stays open until the deployment has finished. skip
// leaves out that many of the first lines, for a page that has them already.
func (s *Server) deploymentLog(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDeployment(w, r)
	if !ok {
		return
	}
	access, err := s.auth.AppAccess(r.Context(), who(r), d.App)
	if err != nil {
		s.internal(w, err)
		return
	}
	if msg := roleRefused(route{perm: canReadLogs}, access); msg != "" {
		problem(w, http.StatusForbidden, "forbidden", msg)
		return
	}
	follow := r.URL.Query().Get("follow") == "true"
	w.Header().Set("Content-Type", apitypes.NDJSONType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
	write := func(l logs.Line) error {
		if skip > 0 {
			skip--
			return nil
		}
		return enc.Encode(apitypes.LogLine{Time: l.Time, Stream: l.Stream, Text: l.Text})
	}
	if !follow {
		lines, err := logs.FollowFromStart(deploy.LogPath(s.dataDir, d.App, d.ID)).Read()
		if err != nil {
			s.log.Error("read a deployment log", "deployment", d.ID, "err", err)
			return
		}
		for _, l := range lines {
			if write(l) != nil {
				return
			}
		}
		return
	}
	// What stays open must end when whoever opened it may no longer read it. The route names no app to look at again, and this does.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go s.watchAccess(ctx, cancel, r.Clone(ctx), route{perm: canReadLogs}, d.App)
	_, err = s.deployer.Follow(ctx, d.ID, func(l logs.Line) error {
		if err := write(l); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		s.log.Error("follow a deployment log", "deployment", d.ID, "err", err)
	}
}
