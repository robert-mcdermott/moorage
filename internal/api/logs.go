package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/applogs"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
)

// logs streams an environment's logs as NDJSON: the newest ?tail= lines (default 100),
// merged by time, then, with ?follow=1, new ones as they are written, until the client
// goes away. ?format=text sends them as plain text, and ?download=1 as a file to keep.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	asText := q.Get("format") == "text"
	download := q.Get("download") == "1"
	tail := 100
	limit := 10000
	if download {
		tail, limit = applogs.MaxLines, applogs.MaxLines
	}
	if v := q.Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			problem(w, http.StatusBadRequest, "bad_request", "tail must be a number of lines")
			return
		}
		tail = min(n, limit)
	}
	query := applogs.Query{Process: q.Get("process"), Instance: q.Get("instance"), Grep: q.Get("grep"), Tail: tail,
		Problems: q.Get("problems") == "1" || q.Get("problems") == "true"}
	var err error
	if query.Sources, err = applogs.SplitSources(q.Get("source")); err != nil {
		problem(w, http.StatusUnprocessableEntity, "invalid_source", err.Error())
		return
	}
	now := time.Now()
	for _, bound := range []struct {
		name string
		to   *time.Time
	}{{"since", &query.Since}, {"until", &query.Until}, {"after", &query.After}} {
		if *bound.to, err = applogs.ParseTime(q.Get(bound.name), now, time.Local); err != nil {
			problem(w, http.StatusUnprocessableEntity, "invalid_time", bound.name+": "+err.Error())
			return
		}
	}
	follow := q.Get("follow") == "1" || q.Get("follow") == "true"

	if asText {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", apitypes.NDJSONType)
	}
	if download {
		w.Header().Set("Content-Disposition", `attachment; filename="`+app+`-`+now.UTC().Format("20060102-150405")+`.log"`)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	rc := http.NewResponseController(w)
	write := func(e applogs.Entry) error {
		if asText {
			_, err := fmt.Fprintf(w, "%s %s %s %s\n", e.Time.UTC().Format(logs.TimeFormat), e.Who(), e.Source, e.Text)
			return err
		}
		return enc.Encode(apitypes.LogLine{
			Time: e.Time, Instance: e.Instance, Process: e.Process, Index: e.Index, Stream: e.Stream(), Text: e.Text,
			Source: e.Source, Level: e.Level, Message: e.Message,
		})
	}
	reader := applogs.Reader{DB: s.db, DataDir: s.dataDir}
	if !follow {
		entries, _, err := reader.Read(r.Context(), app, env, query)
		if err != nil {
			s.log.Error("read an app's logs", "app", app, "err", err)
			return
		}
		for _, e := range entries {
			if write(e) != nil {
				return
			}
		}
		return
	}
	rc.Flush()
	err = reader.Follow(r.Context(), app, env, query, func(e applogs.Entry) error {
		if err := write(e); err != nil {
			return err
		}
		return rc.Flush()
	})
	if err != nil && r.Context().Err() == nil {
		s.log.Warn("follow an app's logs", "app", app, "err", err)
	}
}
