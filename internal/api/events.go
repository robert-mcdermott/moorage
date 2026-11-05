package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// keepAlive is how often a quiet stream says something, for the proxies that close
// silent connections, and to notice clients that have gone.
var keepAlive = 25 * time.Second

// eventFor is an event as someone may read it, who may or may not be able to deploy its app.
func eventFor(e store.Event, deployable func(string) bool) apitypes.Event {
	if !deployable(e.App) {
		e = events.WithoutVariables(e)
	}
	return apitypes.Event{ID: e.ID, At: e.At, Type: e.Type, App: e.App, Env: e.Env, Actor: e.Actor, Summary: events.Summary(e), Level: events.Level(e.Type), Transient: events.Transient(e.Type), Data: e.Data}
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := pageParams(w, r, 50)
	if !ok {
		return
	}
	q := r.URL.Query()
	app, env := q.Get("app"), q.Get("env")
	p := who(r)
	if app != "" && !p.CanApp(app) {
		problem(w, http.StatusForbidden, "forbidden", "This token is restricted to another app.")
		return
	}
	// A token for one app sees that app's events, and no others.
	if app == "" {
		app = p.App
	}
	visible, err := s.auth.Visible(r.Context(), p)
	if err != nil {
		s.internal(w, err)
		return
	}
	if app != "" && !visible(app) {
		problem(w, http.StatusNotFound, "not_found", "There is no app "+app+" that you can see.")
		return
	}
	deployable, err := s.auth.Deployable(r.Context(), p)
	if err != nil {
		s.internal(w, err)
		return
	}
	list, err := s.visibleEvents(r.Context(), visible, store.EventFilter{App: app, Env: env, Before: cursor, Limit: limit + 1})
	if err != nil {
		s.internal(w, err)
		return
	}
	page := apitypes.Page[apitypes.Event]{Items: []apitypes.Event{}}
	for i, e := range list {
		if i == limit {
			page.NextCursor = encodeCursor(list[i-1].ID)
			break
		}
		page.Items = append(page.Items, eventFor(e, deployable))
	}
	writeJSON(w, http.StatusOK, page)
}

// streamEvents follows what happens, and first, what happened since the event the
// client last saw: a browser's EventSource says so when it reconnects.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.internal(w, fmt.Errorf("this connection cannot stream"))
		return
	}
	q := r.URL.Query()
	app, env := q.Get("app"), q.Get("env")
	p := who(r)
	if app != "" && !p.CanApp(app) {
		problem(w, http.StatusForbidden, "forbidden", "This token is restricted to another app.")
		return
	}
	if app == "" {
		app = p.App
	}
	visible, err := s.auth.Visible(r.Context(), p)
	if err != nil {
		s.internal(w, err)
		return
	}
	if app != "" && !visible(app) {
		problem(w, http.StatusNotFound, "not_found", "There is no app "+app+" that you can see.")
		return
	}
	deployable, err := s.auth.Deployable(r.Context(), p)
	if err != nil {
		s.internal(w, err)
		return
	}
	// Who may see what changes while a stream stays open, and new apps appear in it: it
	// looks again every minute.
	seen := time.Now()
	wanted := func(e store.Event) bool {
		if time.Since(seen) > time.Minute || (e.App != "" && !visible(e.App)) {
			again, err := s.auth.Visible(r.Context(), p)
			can, err2 := s.auth.Deployable(r.Context(), p)
			if err == nil && err2 == nil {
				visible, deployable, seen = again, can, time.Now()
			}
		}
		return visible(e.App) && (app == "" || e.App == app) && (env == "" || e.Env == env)
	}

	// Following starts first, so that nothing falls between the two.
	sub := s.bus.Subscribe()
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", apitypes.SSEType)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = q.Get("after")
	}
	write := func(e store.Event) error {
		data, err := json.Marshal(eventFor(e, deployable))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "id: %s\nevent: moorage\ndata: %s\n\n", e.ID, data)
		return err
	}
	if last != "" {
		for {
			missed, err := s.db.Events(r.Context(), store.EventFilter{App: app, Env: env, After: last, Limit: 200})
			if err != nil {
				return
			}
			for _, e := range missed {
				last = e.ID
				if !wanted(e) {
					continue
				}
				if write(e) != nil {
					return
				}
			}
			if len(missed) < 200 {
				break
			}
		}
		flusher.Flush()
	}

	tick := time.NewTicker(keepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": still here\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, open := <-sub.C:
			if !open {
				return
			}
			if !wanted(e) || (last != "" && e.ID <= last) {
				continue
			}
			if write(e) != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// visibleEvents lists events, keeping those someone may see. It reads on past what they
// may not, so that a page is a full one if there is enough to fill it.
func (s *Server) visibleEvents(ctx context.Context, visible func(string) bool, f store.EventFilter) ([]store.Event, error) {
	want := f.Limit
	var out []store.Event
	for {
		list, err := s.db.Events(ctx, f)
		if err != nil {
			return nil, err
		}
		for _, e := range list {
			if visible(e.App) {
				out = append(out, e)
				if len(out) == want {
					return out, nil
				}
			}
		}
		if len(list) < f.Limit {
			return out, nil
		}
		f.Before = list[len(list)-1].ID
	}
}
