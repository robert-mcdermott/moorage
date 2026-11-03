package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
)

// logs streams an environment's app logs as NDJSON: the last ?tail= lines
// (default 100) of every instance merged by time, then, with ?follow=1, new
// lines as they are written, until the client goes away. ?process= keeps one
// process.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	tail := 100
	if v := q.Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			problem(w, http.StatusBadRequest, "bad_request", "tail must be a number of lines")
			return
		}
		tail = min(n, 10000)
	}
	process := q.Get("process")
	follow := q.Get("follow") == "1" || q.Get("follow") == "true"
	dir := reconcile.LogDir(s.dataDir, app, env)

	w.Header().Set("Content-Type", apitypes.NDJSONType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	rc := http.NewResponseController(w)

	files := logFiles(dir, process)
	var lines []apitypes.LogLine
	for _, f := range files {
		got, _ := logs.Tail(f.path, tail)
		for _, l := range got {
			lines = append(lines, f.line(l))
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time.Before(lines[j].Time) })
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	for _, l := range lines {
		enc.Encode(l)
	}
	rc.Flush()
	if !follow {
		return
	}

	followers := map[string]*logs.Follower{}
	byPath := map[string]logFile{}
	for _, f := range files {
		followers[f.path] = logs.NewFollower(f.path)
		byPath[f.path] = f
	}
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
		// Instances started since (a rollout, a restart) appear as new files.
		for _, f := range logFiles(dir, process) {
			if _, ok := followers[f.path]; !ok {
				followers[f.path] = &logs.Follower{Path: f.path}
				byPath[f.path] = f
			}
		}
		var fresh []apitypes.LogLine
		for path, fol := range followers {
			got, _ := fol.Read()
			for _, l := range got {
				fresh = append(fresh, byPath[path].line(l))
			}
		}
		sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].Time.Before(fresh[j].Time) })
		for _, l := range fresh {
			if err := enc.Encode(l); err != nil {
				return
			}
		}
		if len(fresh) > 0 {
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

type logFile struct {
	path     string
	process  string
	index    int
	instance string
}

func (f logFile) line(l logs.Line) apitypes.LogLine {
	return apitypes.LogLine{Time: l.Time, Instance: f.instance, Process: f.process, Index: f.index, Stream: l.Stream, Text: l.Text}
}

// logFiles lists the current log files (<process>.<index>.<instance>.log) in
// dir, oldest first by modification time.
func logFiles(dir, process string) []logFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []logFile
	type withTime struct {
		logFile
		mod time.Time
	}
	var all []withTime
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".log") {
			continue
		}
		parts := strings.SplitN(strings.TrimSuffix(name, ".log"), ".", 3)
		if len(parts) != 3 || (process != "" && parts[0] != process) {
			continue
		}
		index, _ := strconv.Atoi(parts[1])
		info, err := e.Info()
		if err != nil {
			continue
		}
		all = append(all, withTime{logFile{filepath.Join(dir, name), parts[0], index, parts[2]}, info.ModTime()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for _, f := range all {
		out = append(out, f.logFile)
	}
	return out
}
